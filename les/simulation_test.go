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
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/simulations"
	"github.com/ethereum/go-ethereum/p2p/simulations/adapters"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	colorable "github.com/mattn/go-colorable"
)

func init() {
	flag.Parse()
	// register the Delivery service which will run as a devp2p
	// protocol when using the exec adapter
	fmt.Println("register start")
	adapters.RegisterServices(services)
	fmt.Println("register end")

	log.PrintOrigins(true)
	log.Root().SetHandler(log.LvlFilterHandler(log.Lvl(*loglevel), log.StreamHandler(colorable.NewColorableStderr(), log.TerminalFormat(true))))
}

var (
	adapter  = flag.String("adapter", "exec", "type of simulation: sim|socket|exec|docker")
	loglevel = flag.Int("loglevel", 0, "verbosity of logs")
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
	/*case "docker":
	adapter, err = adapters.NewDockerAdapter()
	if err != nil {
		return nil, teardown, err
	}*/
	default:
		return nil, teardown, errors.New("adapter needs to be one of sim, socket, exec, docker")
	}
	return adapter, teardown, nil
}

func testSim(t *testing.T, serverCount int, clientCount int, test func(ctx context.Context, net *simulations.Network, servers []*simulations.Node, clients []*simulations.Node)) {
	fmt.Println("test start")
	net, teardown, err := NewNetwork()
	defer teardown()
	if err != nil {
		t.Fatalf("Failed to create network: %v", err)
	}
	timeout := 1800 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	servers := make([]*simulations.Node, serverCount)
	clients := make([]*simulations.Node, clientCount)

	for i, _ := range clients {
		clientconf := adapters.RandomNodeConfig()
		clientconf.Services = []string{"lesclient"}
		client, err := net.NewNodeWithConfig(clientconf)
		if err != nil {
			t.Fatalf("Failed to create client: %v", err)
		}
		clients[i] = client
	}

	for i, _ := range servers {
		serverconf := adapters.RandomNodeConfig()
		serverconf.Services = []string{"lesserver"}
		serverconf.DataDir = "/media/1TB/.ethereum" // ***
		server, err := net.NewNodeWithConfig(serverconf)
		if err != nil {
			t.Fatalf("Failed to create server: %v", err)
		}
		servers[i] = server
	}

	for _, client := range clients {
		if err := net.Start(client.ID()); err != nil {
			t.Fatalf("Failed to start client node: %v", err)
		}
	}
	for _, server := range servers {
		if err := net.Start(server.ID()); err != nil {
			t.Fatalf("Failed to start server node: %v", err)
		}
	}

	test(ctx, net, servers, clients)
}

func getHead(ctx context.Context, t *testing.T, client *rpc.Client) (uint64, common.Hash) {
	res := make(map[string]interface{})
	if err := client.CallContext(ctx, &res, "eth_getBlockByNumber", "latest", false); err != nil {
		t.Fatalf("Failed to obtain head block: %v", err)
	}
	numStr, ok := res["number"].(string)
	if !ok {
		t.Fatalf("RPC block number field invalid")
	}
	num, err := hexutil.DecodeUint64(numStr)
	if err != nil {
		t.Fatalf("Failed to decode RPC block number: %v", err)
	}
	hashStr, ok := res["hash"].(string)
	if !ok {
		t.Fatalf("RPC block number field invalid")
	}
	hash := common.HexToHash(hashStr)
	return num, hash
}

func testRequest(ctx context.Context, t *testing.T, client *rpc.Client) {
	//res := make(map[string]interface{})
	var res string
	var addr common.Address
	rand.Read(addr[:])
	//	if err := client.CallContext(ctx, &res, "eth_getProof", addr, nil, "latest"); err != nil {
	if err := client.CallContext(ctx, &res, "eth_getBalance", addr, "latest"); err != nil {
		t.Fatalf("Failed to obtain Merkle proof: %v", err)
	}
}

func setBandwidth(ctx context.Context, t *testing.T, server *rpc.Client, clientID enode.ID, bw uint64) {
	if err := server.CallContext(ctx, nil, "les_setClientBandwidth", clientID, bw); err != nil {
		t.Fatalf("Failed to set client bandwidth: %v", err)
	}
}

func getBandwidth(ctx context.Context, t *testing.T, server *rpc.Client, clientID enode.ID) uint64 {
	var s string
	if err := server.CallContext(ctx, &s, "les_getClientBandwidth", clientID); err != nil {
		t.Fatalf("Failed to get client bandwidth: %v", err)
	}
	bw, err := hexutil.DecodeUint64(s)
	if err != nil {
		t.Fatalf("Failed to decode client bandwidth: %v", err)
	}
	return bw
}

func bandwidthLimits(ctx context.Context, t *testing.T, server *rpc.Client) (uint64, uint64) {
	var s string
	if err := server.CallContext(ctx, &s, "les_totalBandwidth"); err != nil {
		t.Fatalf("Failed to query total bandwidth: %v", err)
	}
	total, err := hexutil.DecodeUint64(s)
	if err != nil {
		t.Fatalf("Failed to decode total bandwidth: %v", err)
	}
	if err := server.CallContext(ctx, &s, "les_minimumBandwidth"); err != nil {
		t.Fatalf("Failed to query minimum bandwidth: %v", err)
	}
	min, err := hexutil.DecodeUint64(s)
	if err != nil {
		t.Fatalf("Failed to decode minimum bandwidth: %v", err)
	}
	return total, min
}

const minRelBw = 0.2

func TestSim(t *testing.T) {
	testSim(t, 1, 4, func(ctx context.Context, net *simulations.Network, servers []*simulations.Node, clients []*simulations.Node) {
		if len(servers) != 1 {
			t.Fatalf("Invalid number of servers: %d", len(servers))
		}
		server := servers[0]

		clientRpcClients := make([]*rpc.Client, len(clients))

		serverRpcClient, err := server.Client()
		if err != nil {
			t.Fatalf("Failed to obtain rpc client: %v", err)
		}
		headNum, headHash := getHead(ctx, t, serverRpcClient)
		totalBw, minBw := bandwidthLimits(ctx, t, serverRpcClient)
		fmt.Printf("Server totalBw: %d  minBw: %d  head number: %d  head hash: %064x\n", totalBw, minBw, headNum, headHash)
		reqMinBw := uint64(float64(totalBw) * minRelBw / (minRelBw + float64(len(clients)-1)))
		if minBw > reqMinBw {
			t.Fatalf("Minimum client bandwidth (%d) bigger than required minimum for this test (%d)", minBw, reqMinBw)
		}

		for i, client := range clients {
			var err error
			clientRpcClients[i], err = client.Client()
			if err != nil {
				t.Fatalf("Failed to obtain rpc client: %v", err)
			}

			fmt.Println("connecting client", i)
			setBandwidth(ctx, t, serverRpcClient, client.ID(), totalBw/uint64(len(clients)))
			net.Connect(client.ID(), server.ID())

			for {
				select {
				case <-ctx.Done():
					t.Fatalf("Timeout")
				default:
				}
				num, hash := getHead(ctx, t, clientRpcClients[i])
				if num == headNum && hash == headHash {
					fmt.Println("client", i, "synced")
					break
				}
				time.Sleep(time.Millisecond * 200)
			}
		}

		var wg sync.WaitGroup
		stop := make(chan struct{})

		reqCount := make([]uint64, len(clientRpcClients))
		for i, c := range clientRpcClients {
			wg.Add(1)
			i, c := i, c
			go func() {
				queue := make(chan struct{}, 10)
				var count uint64
				for {
					select {
					case queue <- struct{}{}:
						wg.Add(1)
						go func() {
							testRequest(ctx, t, c)
							wg.Done()
							<-queue
							count++
							atomic.StoreUint64(&reqCount[i], count)
						}()
					case <-stop:
						wg.Done()
						return
					case <-ctx.Done():
						wg.Done()
						return
					}
				}
			}()
		}

		processedSince := func(start []uint64) []uint64 {
			res := make([]uint64, len(reqCount))
			for i, _ := range reqCount {
				res[i] = atomic.LoadUint64(&reqCount[i])
				if start != nil {
					res[i] -= start[i]
				}
			}
			return res
		}

		weights := make([]float64, len(clients))
		for c := 0; c < 5; c++ {
			var sum float64
			for i, _ := range clients {
				weights[i] = rand.Float64()*(1-minRelBw) + minRelBw
				sum += weights[i]
			}
			for i, client := range clients {
				weights[i] /= sum
				bandwidth := uint64(float64(totalBw) * weights[i])
				if bandwidth < getBandwidth(ctx, t, serverRpcClient, client.ID()) {
					setBandwidth(ctx, t, serverRpcClient, client.ID(), bandwidth)
				}
			}
			for i, client := range clients {
				bandwidth := uint64(float64(totalBw) * weights[i])
				if bandwidth > getBandwidth(ctx, t, serverRpcClient, client.ID()) {
					setBandwidth(ctx, t, serverRpcClient, client.ID(), bandwidth)
				}
			}

			time.Sleep(flowcontrol.DecParamDelay)
			fmt.Println("starting measurement")
			start := processedSince(nil)
			for {
				select {
				case <-ctx.Done():
					t.Fatalf("Timeout")
				default:
				}

				processed := processedSince(start)
				var avg uint64
				fmt.Printf("Processed")
				for i, p := range processed {
					fmt.Printf(" %d", p)
					processed[i] = uint64(float64(p) / weights[i])
					avg += processed[i]
				}
				avg /= uint64(len(processed))

				if avg >= 10000 {
					var maxDev float64
					for _, p := range processed {
						dev := float64(int64(p-avg)) / float64(avg)
						if dev > maxDev {
							maxDev = dev
						}
					}
					fmt.Printf("  max deviation: %f\n", maxDev)
					if maxDev <= 0.01 {
						fmt.Println("success")
						break
					}
				} else {
					fmt.Println()
				}
				time.Sleep(time.Millisecond * 200)
			}
		}

		close(stop)
		wg.Wait()

		for i, count := range reqCount {
			fmt.Println("client", i, "processed", count)
		}
	})
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
	config.SyncMode = downloader.LightSync
	config.Ethash.PowMode = ethash.ModeFake
	return New(ctx.NodeContext, &config)
}

func newLesServerService(ctx *adapters.ServiceContext) (node.Service, error) {
	fmt.Println("server init start")
	defer fmt.Println("server init end")
	config := eth.DefaultConfig
	config.SyncMode = downloader.FullSync
	config.LightServ = 200
	config.LightPeers = 20
	ethereum, err := eth.New(ctx.NodeContext, &config)
	if err != nil {
		return nil, err
	}

	server, err := NewLesServer(ethereum, &config)
	if err != nil {
		return nil, err
	}
	ethereum.AddLesServer(server)
	/*	ethereum.AddExtraAPIs([]rpc.API{{
		Namespace: "test",
		Version:   "1.0",
		Service:   &SimTestAPI{ethereum},
	}})*/
	return ethereum, nil
}

type SimTestAPI struct {
	ethereum *eth.Ethereum
}

func (s *SimTestAPI) GenerateChain(headNum uint64) (uint64, error) {
	db := s.ethereum.ChainDb()
	chain := s.ethereum.BlockChain()
	lastBlock := chain.CurrentBlock()
	lastNum := lastBlock.NumberU64()
	if headNum > lastNum {
		blocks, _ := core.GenerateChain(params.TestChainConfig, lastBlock, ethash.NewFaker(), db, int(headNum-lastNum), nil)
		if i, err := chain.InsertChain(blocks); err != nil {
			return chain.CurrentBlock().NumberU64(), fmt.Errorf("error at inserting block #%d: %v", i, err)
		}
		return chain.CurrentBlock().NumberU64(), nil
	}
	return headNum, nil
}
