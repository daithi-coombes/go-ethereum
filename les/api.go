// Copyright 2018 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
package les

import (
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

var (
	ErrMinBW   = errors.New("bandwidth too small")
	ErrTotalBW = errors.New("total bandwidth exceeded")
)

// PublicLesServerAPI  provides an API to access the les server.
// It offers only methods that operate on public data that is freely available to anyone.
type PrivateLesServerAPI struct {
	server *LesServer
	pm     *ProtocolManager
	vip    *vipClientPool
}

// NewPublicLesServerAPI creates a new les server API.
func NewPrivateLesServerAPI(server *LesServer) *PrivateLesServerAPI {
	vip := &vipClientPool{
		clients: make(map[enode.ID]vipClientInfo),
		totalBw: server.totalBandwidth,
		pm:      server.protocolManager,
	}
	server.protocolManager.vipClientPool = vip
	return &PrivateLesServerAPI{
		server: server,
		pm:     server.protocolManager,
		vip:    vip,
	}
}

// Query total available bandwidth for all clients
func (api *PrivateLesServerAPI) TotalBandwidth() hexutil.Uint64 {
	return hexutil.Uint64(api.server.totalBandwidth)
}

// Query minimum assignable bandwidth for a single client
func (api *PrivateLesServerAPI) MinimumBandwidth() hexutil.Uint64 {
	return hexutil.Uint64(api.server.minBandwidth)
}

// vipClientPool stores information about prioritized clients
type vipClientPool struct {
	lock                                  sync.Mutex
	pm                                    *ProtocolManager
	clients                               map[enode.ID]vipClientInfo
	totalBw, totalVipBw, totalConnectedBw uint64
	vipCount                              int
}

// vipClientInfo entries exist for all prioritized clients and currently connected free clients
type vipClientInfo struct {
	bw        uint64 // zero for non-vip clients
	connected bool
	updateBw  func(uint64)
}

// SetClientBandwidth sets the priority bandwidth assigned to a given client.
// If the assigned bandwidth is bigger than zero then connection is always
// guaranteed. The sum of bandwidth assigned to priority clients can not exceed
// the total available bandwidth.
//
// Note: assigned bandwidth can be changed while the client is connected with
// immediate effect.
func (api *PrivateLesServerAPI) SetClientBandwidth(id enode.ID, bw uint64) error {
	if bw != 0 && bw < api.server.minBandwidth {
		return ErrMinBW
	}

	api.vip.lock.Lock()
	defer api.vip.lock.Unlock()

	c := api.vip.clients[id]
	if api.vip.totalVipBw+bw > api.vip.totalBw+c.bw {
		return ErrTotalBW
	}
	api.vip.totalVipBw += bw - c.bw
	if c.bw != 0 {
		api.vip.vipCount--
	}
	if bw != 0 {
		api.vip.vipCount++
	}
	if c.updateBw != nil {
		c.updateBw(bw)
	}
	if c.connected {
		api.vip.totalConnectedBw += bw - c.bw
		api.pm.clientPool.setConnLimit(api.pm.maxFreePeers(api.vip.vipCount, api.vip.totalConnectedBw))
	}
	if bw != 0 || c.connected {
		c.bw = bw
		api.vip.clients[id] = c
	} else {
		delete(api.vip.clients, id)
	}
	return nil
}

func (api *PrivateLesServerAPI) GetClientBandwidth(id enode.ID) hexutil.Uint64 {
	api.vip.lock.Lock()
	defer api.vip.lock.Unlock()

	return hexutil.Uint64(api.vip.clients[id].bw)
}

func (v *vipClientPool) connect(id enode.ID, updateBw func(uint64)) (uint64, bool) {
	v.lock.Lock()
	defer v.lock.Unlock()

	c := v.clients[id]
	if c.connected {
		return 0, false
	}
	c.connected = true
	c.updateBw = updateBw
	v.clients[id] = c
	if c.bw != 0 {
		v.vipCount++
	}
	v.totalConnectedBw += c.bw
	v.pm.clientPool.setConnLimit(v.pm.maxFreePeers(v.vipCount, v.totalConnectedBw))
	return c.bw, true
}

func (v *vipClientPool) disconnect(id enode.ID) {
	v.lock.Lock()
	defer v.lock.Unlock()

	c := v.clients[id]
	c.connected = false
	if c.bw != 0 {
		v.clients[id] = c
		v.vipCount--
	} else {
		delete(v.clients, id)
	}
	v.totalConnectedBw -= c.bw
	v.pm.clientPool.setConnLimit(v.pm.maxFreePeers(v.vipCount, v.totalConnectedBw))
}
