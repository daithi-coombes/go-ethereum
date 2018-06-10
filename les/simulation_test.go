// Copyright 2016 The go-ethereum Authors
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
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/simulations"
	"github.com/ethereum/go-ethereum/p2p/simulations/adapters"
	"github.com/ethereum/go-ethereum/params"
	colorable "github.com/mattn/go-colorable"
)

func init() {
	flag.Parse()
	// register the Delivery service which will run as a devp2p
	// protocol when using the exec adapter
	adapters.RegisterServices(services)

	log.PrintOrigins(true)
	log.Root().SetHandler(log.LvlFilterHandler(log.Lvl(*loglevel), log.StreamHandler(colorable.NewColorableStderr(), log.TerminalFormat(true))))
}

var (
	adapter  = flag.String("adapter", "sim", "type of simulation: sim|socket|exec|docker")
	loglevel = flag.Int("loglevel", 2, "verbosity of logs")
	nodes    = flag.Int("nodes", 0, "number of nodes")
)

var services = adapters.Services{
	"lesclient": newLesClientService,
	"lesserver": newLesServerService,
}

func NewAdapter(adapterType string, services adapters.Services) (adapter adapters.NodeAdapter, teardown func(), err error) {
	teardown = func() {}
	switch adapterType {
	case "sim":
		adapter = adapters.NewSimAdapter(services)
		//	case "socket":
		//		adapter = adapters.NewSocketAdapter(services)
	case "exec":
		baseDir, err0 := ioutil.TempDir("", "les-test")
		if err0 != nil {
			return nil, teardown, err0
		}
		teardown = func() { os.RemoveAll(baseDir) }
		adapter = adapters.NewExecAdapter(baseDir)
	case "docker":
		adapter, err = adapters.NewDockerAdapter()
		if err != nil {
			return nil, teardown, err
		}
	default:
		return nil, teardown, errors.New("adapter needs to be one of sim, socket, exec, docker")
	}
	return adapter, teardown, nil
}

func TestSim(t *testing.T) {
	net, teardown, err := NewNetwork()
	defer teardown()
	if err != nil {
		t.Fatalf("Failed to create network: %v", err)
	}

	clientconf := adapters.RandomNodeConfig()
	clientconf.Services = []string{"lesclient"}
	client, err := net.NewNodeWithConfig(clientconf)

	serverconf := adapters.RandomNodeConfig()
	serverconf.Services = []string{"lesserver"}
	server, err := net.NewNodeWithConfig(serverconf)

	if err := net.Start(client.ID()); err != nil {
		t.Fatalf("Failed to start client node: %v", err)
	}
	if err := net.Start(server.ID()); err != nil {
		t.Fatalf("Failed to start server node: %v", err)
	}
	net.Connect(client.ID(), server.ID())

	sim := simulations.NewSimulation(net)

	action := func(ctx context.Context) error {
		return nil
	}

	check := func(ctx context.Context, id discover.NodeID) (bool, error) {
		fmt.Println(id, "*****")
		// check we haven't run out of time
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}

		// get the node
		node := net.GetNode(id)
		if node == nil {
			return false, fmt.Errorf("unknown node: %s", id)
		}
		client, err := node.Client()
		if err != nil {
			return false, err
		}
		var s string
		if err := client.CallContext(ctx, &s, "eth_blockNumber"); err != nil {
			return false, err
		}

		fmt.Println(id, s)
		return s == "0x3e8", nil
	}

	timeout := 300 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	trigger := make(chan discover.NodeID)
	go func() {
		for {
			select {
			case trigger <- client.ID():
			case <-ctx.Done():
				return
			}
			select {
			case trigger <- server.ID():
			case <-ctx.Done():
				return
			}
			time.Sleep(time.Millisecond * 100)
		}
		//		trigger <- client.ID()
		//		trigger <- server.ID()
	}()

	step := &simulations.Step{
		Action:  action,
		Trigger: trigger,
		Expect: &simulations.Expectation{
			Nodes: []discover.NodeID{server.ID(), client.ID()},
			Check: check,
		},
	}

	result := sim.Run(ctx, step)
	if result.Error != nil {
		t.Fatalf("Simulation failed: %s", result.Error)
	}
}

func NewNetwork() (*simulations.Network, func(), error) {
	adapter, adapterTeardown, err := NewAdapter(*adapter, services)
	if err != nil {
		return nil, adapterTeardown, err
	}
	defaultService := "streamer"
	net := simulations.NewNetwork(adapter, &simulations.NetworkConfig{
		ID:             "0",
		DefaultService: defaultService,
	})
	teardown := func() {
		adapterTeardown()
		net.Shutdown()
	}

	return net, teardown, nil
}

func newLesClientService(ctx *adapters.ServiceContext) (node.Service, error) {
	config := eth.DefaultConfig
	config.NetworkId = 12345
	config.SyncMode = downloader.LightSync
	config.Ethash.PowMode = ethash.ModeFake
	config.Genesis = &core.Genesis{Config: params.TestChainConfig}
	return New(ctx.NodeContext, &config)
}

func newLesServerService(ctx *adapters.ServiceContext) (node.Service, error) {
	config := eth.DefaultConfig
	config.NetworkId = 12345
	config.SyncMode = downloader.FullSync
	config.LightServ = 50
	config.Ethash.PowMode = ethash.ModeFake
	config.Genesis = &core.Genesis{Config: params.TestChainConfig}
	ethereum, err := eth.New(ctx.NodeContext, &config)
	if err != nil {
		return nil, err
	}

	db := ethereum.ChainDb()
	chain := ethereum.BlockChain()
	genesis := chain.GetBlockByNumber(0)
	if genesis == nil {
		return nil, fmt.Errorf("no genesis block")
	}
	blocks, _ := core.GenerateChain(params.TestChainConfig, genesis, ethash.NewFaker(), db, 1000, nil)
	if i, err := chain.InsertChain(blocks); err != nil {
		return nil, fmt.Errorf("error at inserting block #%d: %v", i, err)
	}
	server, err := NewLesServer(ethereum, &config)
	if err != nil {
		return nil, err
	}
	ethereum.AddLesServer(server)
	return ethereum, nil
}
