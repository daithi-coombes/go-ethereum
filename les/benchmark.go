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
	"encoding/binary"
	"fmt"
	"math/big"
	"math/rand"
	"sort"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

type requestBenchmark interface {
	init(pm *ProtocolManager, count int) error
	request(peer *peer, index int) error
}

type benchmarkBlockHeaders struct {
	amount, skip    int
	reverse, byHash bool
	offset, randMax int64
	hashes          []common.Hash
}

func (b *benchmarkBlockHeaders) init(pm *ProtocolManager, count int) error {
	d := int64(b.amount-1) * int64(b.skip+1)
	b.offset = 0
	b.randMax = pm.blockchain.CurrentHeader().Number.Int64() + 1 - d
	if b.randMax < 0 {
		return fmt.Errorf("chain is too short")
	}
	if b.reverse {
		b.offset = d
	}
	if b.byHash {
		b.hashes = make([]common.Hash, count)
		for i, _ := range b.hashes {
			b.hashes[i] = rawdb.ReadCanonicalHash(pm.chainDb, uint64(b.offset+rand.Int63n(b.randMax)))
		}
	}
	return nil
}

func (b *benchmarkBlockHeaders) request(peer *peer, index int) error {
	if b.byHash {
		return peer.RequestHeadersByHash(0, 0, b.hashes[index], b.amount, b.skip, b.reverse)
	} else {
		return peer.RequestHeadersByNumber(0, 0, uint64(b.offset+rand.Int63n(b.randMax)), b.amount, b.skip, b.reverse)
	}
}

type benchmarkBodiesOrReceipts struct {
	receipts bool
	hashes   []common.Hash
}

func (b *benchmarkBodiesOrReceipts) init(pm *ProtocolManager, count int) error {
	randMax := pm.blockchain.CurrentHeader().Number.Int64() + 1
	b.hashes = make([]common.Hash, count)
	for i, _ := range b.hashes {
		b.hashes[i] = rawdb.ReadCanonicalHash(pm.chainDb, uint64(rand.Int63n(randMax)))
	}
	return nil
}

func (b *benchmarkBodiesOrReceipts) request(peer *peer, index int) error {
	if b.receipts {
		return peer.RequestReceipts(0, 0, []common.Hash{b.hashes[index]})
	} else {
		return peer.RequestBodies(0, 0, []common.Hash{b.hashes[index]})
	}
}

type benchmarkProofsOrCode struct {
	code     bool
	headHash common.Hash
}

func (b *benchmarkProofsOrCode) init(pm *ProtocolManager, count int) error {
	b.headHash = pm.blockchain.CurrentHeader().Hash()
	return nil
}

func (b *benchmarkProofsOrCode) request(peer *peer, index int) error {
	key := make([]byte, 32)
	rand.Read(key)
	if b.code {
		return peer.RequestCode(0, 0, []CodeReq{CodeReq{BHash: b.headHash, AccKey: key}})
	} else {
		return peer.RequestProofs(0, 0, []ProofReq{ProofReq{BHash: b.headHash, Key: key}})
	}
}

type benchmarkHelperTrie struct {
	bloom                 bool
	reqCount              int
	sectionCount, headNum uint64
}

func (b *benchmarkHelperTrie) init(pm *ProtocolManager, count int) error {
	if b.bloom {
		b.sectionCount, b.headNum, _ = pm.server.bloomTrieIndexer.Sections()
	} else {
		b.sectionCount, _, _ = pm.server.chtIndexer.Sections()
		b.sectionCount /= (params.CHTFrequencyClient / params.CHTFrequencyServer)
		b.headNum = b.sectionCount*params.CHTFrequencyClient - 1
	}
	if b.sectionCount == 0 {
		return fmt.Errorf("no processed sections available")
	}
	return nil
}

func (b *benchmarkHelperTrie) request(peer *peer, index int) error {
	reqs := make([]HelperTrieReq, b.reqCount)

	if b.bloom {
		bitIdx := uint16(rand.Intn(2048))
		for i, _ := range reqs {
			key := make([]byte, 10)
			binary.BigEndian.PutUint16(key[:2], bitIdx)
			binary.BigEndian.PutUint64(key[2:], uint64(rand.Int63n(int64(b.sectionCount))))
			reqs[i] = HelperTrieReq{Type: htBloomBits, TrieIdx: b.sectionCount - 1, Key: key}
		}
	} else {
		for i, _ := range reqs {
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key[:], uint64(rand.Int63n(int64(b.headNum))))
			reqs[i] = HelperTrieReq{Type: htCanonical, TrieIdx: b.sectionCount - 1, Key: key, AuxReq: auxHeader}
		}
	}

	return peer.RequestHelperTrieProofs(0, 0, reqs)
}

type benchmarkTxSend struct {
	txs types.Transactions
}

func (b *benchmarkTxSend) init(pm *ProtocolManager, count int) error {
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	signer := types.NewEIP155Signer(big.NewInt(18))
	b.txs = make(types.Transactions, count)

	for i, _ := range b.txs {
		data := make([]byte, txSizeCostLimit)
		rand.Read(data)
		tx, err := types.SignTx(types.NewTransaction(0, addr, new(big.Int), 0, new(big.Int), data), signer, key)
		if err != nil {
			panic(err)
		}
		b.txs[i] = tx
	}
	return nil
}

func (b *benchmarkTxSend) request(peer *peer, index int) error {
	enc, _ := rlp.EncodeToBytes(types.Transactions{b.txs[index]})
	return peer.SendTxs(0, 0, enc)
}

type benchmarkTxStatus struct{}

func (b *benchmarkTxStatus) init(pm *ProtocolManager, count int) error {
	return nil
}

func (b *benchmarkTxStatus) request(peer *peer, index int) error {
	var hash common.Hash
	rand.Read(hash[:])
	return peer.RequestTxStatus(0, 0, []common.Hash{hash})
}

type benchmarkType struct {
	name        string
	newInstance func() requestBenchmark
	outSizeCorr uint32
	avgTimeCorr float64
}

var benchmarkTypes = map[string]benchmarkType{
	"header1n": {name: "header by number (single)", newInstance: func() requestBenchmark {
		return &benchmarkBlockHeaders{amount: 1}
	}},
	"header1h": {name: "header by hash (single)", newInstance: func() requestBenchmark {
		return &benchmarkBlockHeaders{amount: 1, byHash: true}
	}},
	"header192n": {name: "headers by number (192)", newInstance: func() requestBenchmark {
		return &benchmarkBlockHeaders{amount: 192}
	}},
	"header192hr": {name: "headers by hash  (192, reverse)", newInstance: func() requestBenchmark {
		return &benchmarkBlockHeaders{amount: 192, byHash: true, reverse: true}
	}},
	"body": {name: "block body", newInstance: func() requestBenchmark {
		return &benchmarkBodiesOrReceipts{receipts: false}
	}},
	"receipts": {name: "block receipts", newInstance: func() requestBenchmark {
		return &benchmarkBodiesOrReceipts{receipts: true}
	}},
	"proof": {name: "merkle proof", newInstance: func() requestBenchmark {
		return &benchmarkProofsOrCode{code: false}
	}, outSizeCorr: 500, avgTimeCorr: 2.5},
	"code": {name: "contract code", newInstance: func() requestBenchmark {
		return &benchmarkProofsOrCode{code: true}
	}, outSizeCorr: 100000, avgTimeCorr: 1.5},
	"cht1": {name: "cht (single)", newInstance: func() requestBenchmark {
		return &benchmarkHelperTrie{bloom: false, reqCount: 1}
	}},
	"cht16": {name: "cht (16)", newInstance: func() requestBenchmark {
		return &benchmarkHelperTrie{bloom: false, reqCount: 16}
	}},
	"bloom1": {name: "bloom trie (single)", newInstance: func() requestBenchmark {
		return &benchmarkHelperTrie{bloom: true, reqCount: 1}
	}},
	"bloom16": {name: "bloom trie (16)", newInstance: func() requestBenchmark {
		return &benchmarkHelperTrie{bloom: true, reqCount: 16}
	}},
	"txsend": {name: "send transaction", newInstance: func() requestBenchmark {
		return &benchmarkTxSend{}
	}, outSizeCorr: 50},
	"txstatus": {name: "get transaction status", newInstance: func() requestBenchmark {
		return &benchmarkTxStatus{}
	}, outSizeCorr: 50},
}

var reqBenchMap = []struct {
	code      uint64
	id, idMax []string
	maxCount  uint64
}{
	{GetBlockHeadersMsg, []string{"header1n", "header1h"}, []string{"header192n", "header192hr"}, 192},
	{GetBlockBodiesMsg, []string{"body"}, nil, 1},
	{GetReceiptsMsg, []string{"receipts"}, nil, 1},
	{GetCodeMsg, []string{"code"}, nil, 1},
	{GetProofsV1Msg, []string{"proof"}, nil, 1},
	{GetProofsV2Msg, []string{"proof"}, nil, 1},
	{GetHeaderProofsMsg, []string{"cht1"}, []string{"cht16"}, 16},
	{GetHelperTrieProofsMsg, []string{"cht1", "bloom1"}, []string{"cht16", "bloom16"}, 16},
	{SendTxMsg, []string{"txsend"}, nil, 1},
	{SendTxV2Msg, []string{"txsend"}, nil, 1},
	{GetTxStatusMsg, []string{"txstatus"}, nil, 1},
}

type benchmarkSetup struct {
	req                   requestBenchmark
	id, name              string
	totalCount            int
	totalTime, avgTime    time.Duration
	maxInSize, maxOutSize uint32
	err                   error
}

var reqBenchmarkKey = []byte("_requestBenchmarks3_")

const (
	passCount          = 10
	firstCount         = 50
	totalBenchmarkTime = time.Second * 20
	discardAge         = 100000
	rerunAge           = 10000
	rerunCount         = 5
)

type benchmarkData struct {
	BlockNumber, AvgTime  uint64
	MaxInSize, MaxOutSize uint32
}

type benchmarkDataByTime []benchmarkData

func (s benchmarkDataByTime) Len() int           { return len(s) }
func (s benchmarkDataByTime) Less(i, j int) bool { return s[i].AvgTime < s[j].AvgTime }
func (s benchmarkDataByTime) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func dataToCost(id string, data []benchmarkData, inSizeCostFactor, outSizeCostFactor float64) uint64 {
	var (
		maxInSize, maxOutSize uint32
		avgTime               uint64
	)
	for _, d := range data {
		if d.MaxInSize > maxInSize {
			maxInSize = d.MaxInSize
		}
		if d.MaxOutSize > maxOutSize {
			maxOutSize = d.MaxOutSize
		}
	}
	var cost uint64
	if len(data) > 0 {
		sort.Sort(benchmarkDataByTime(data))
		skip := len(data) / 5
		for i := skip; i < len(data)-skip; i++ {
			avgTime += data[i].AvgTime
		}
		avgTime /= uint64(len(data) - skip*2)
		bt := benchmarkTypes[id]
		maxOutSize += bt.outSizeCorr
		if bt.avgTimeCorr != 0 {
			avgTime = uint64(float64(avgTime) * bt.avgTimeCorr)
		}
		cost = avgTime * 2
	}
	inSizeCost := uint64(float64(maxInSize) * inSizeCostFactor * 1.25)
	outSizeCost := uint64(float64(maxOutSize) * outSizeCostFactor * 1.25)
	if inSizeCost > cost {
		cost = inSizeCost
	}
	if outSizeCost > cost {
		cost = outSizeCost
	}
	return cost
}

func (pm *ProtocolManager) benchmarkCosts(threadCount int, inSizeCostFactor, outSizeCostFactor float64) (costList RequestCostList, minBufLimit uint64) {
	blockNumber := pm.blockchain.CurrentHeader().Number.Uint64()
	allData := make(map[string][]benchmarkData)
	run := false
	for id, _ := range benchmarkTypes {
		var data []benchmarkData
		if enc, err := pm.chainDb.Get(append(reqBenchmarkKey, []byte(id)...)); err == nil {
			if rlp.DecodeBytes(enc, &data) != nil {
				data = nil
			}
		}
		for len(data) > 0 && data[0].BlockNumber+discardAge <= blockNumber {
			data = data[1:]
		}
		if len(data) < rerunCount || data[len(data)-1].BlockNumber+rerunAge <= blockNumber {
			run = true
		}
		allData[id] = data
	}

	if run {
		res := pm.runBenchmark()
		for _, r := range res {
			if r.err == nil {
				data := append(allData[r.id], benchmarkData{BlockNumber: blockNumber, AvgTime: uint64(r.avgTime) * uint64(threadCount), MaxInSize: r.maxInSize, MaxOutSize: r.maxOutSize})
				allData[r.id] = data
				if enc, err := rlp.EncodeToBytes(data); err == nil {
					pm.chainDb.Put(append(reqBenchmarkKey, []byte(r.id)...), enc)
				}
			}
		}
	}

	// calculate upper cost estimates based on AvgTime and MaxSize
	costs := make(map[string]uint64)
	for id, data := range allData {
		costs[id] = dataToCost(id, data, inSizeCostFactor, outSizeCostFactor)
	}
	var maxAllCosts uint64
	// create linear cost functions for actual request types using reqBenchMap
	res := make(RequestCostList, len(reqBenchMap))
	for i, m := range reqBenchMap {
		res[i].MsgCode = m.code
		var cost uint64
		for _, id := range m.id {
			if c, ok := costs[id]; ok {
				if c > cost {
					cost = c
				}
			} else {
				panic(nil)
			}
		}
		if m.idMax == nil {
			res[i].BaseCost = 0
			res[i].ReqCost = cost
		} else {
			var maxCost uint64
			for _, id := range m.idMax {
				if c, ok := costs[id]; ok {
					if c > maxCost {
						maxCost = c
					}
				} else {
					panic(nil)
				}
			}
			if maxCost < cost {
				maxCost = cost
			}
			if maxCost > maxAllCosts {
				maxAllCosts = maxCost
			}
			dc := (maxCost - cost) / (m.maxCount - 1)
			if cost < dc {
				dc = maxCost / m.maxCount
				cost = dc
			}
			res[i].BaseCost = cost - dc
			res[i].ReqCost = dc
		}
	}
	return res, maxAllCosts * 2
}

func (pm *ProtocolManager) runBenchmark() []*benchmarkSetup {
	log.Info("running benchmark")
	setup := make([]*benchmarkSetup, len(benchmarkTypes))
	i := 0
	for id, bt := range benchmarkTypes {
		setup[i] = &benchmarkSetup{id: id, name: bt.name, req: bt.newInstance()}
		i++
	}
	targetTime := totalBenchmarkTime / time.Duration(len(benchmarkTypes)*passCount)
	for i := 0; i < passCount; i++ {
		todo := make([]*benchmarkSetup, len(benchmarkTypes))
		copy(todo, setup)
		for len(todo) > 0 {
			// select a random element
			index := rand.Intn(len(todo))
			next := todo[index]
			todo[index] = todo[len(todo)-1]
			todo = todo[:len(todo)-1]

			if next.err == nil {
				// calculate request count
				count := firstCount
				if next.totalTime > 0 {
					count = int(uint64(next.totalCount) * uint64(targetTime) / uint64(next.totalTime))
				}
				if err := pm.measure(next, count); err != nil {
					next.err = err
				}
			}
		}
		log.Info("completed", "percent", (i+1)*100/passCount)
	}

	for _, s := range setup {
		if s.err == nil {
			s.avgTime = s.totalTime / time.Duration(s.totalCount)
			log.Debug("result", "name", s.name, "avgTime", s.avgTime, "reqCount", s.totalCount, "maxInSize", s.maxInSize, "maxOutSize", s.maxOutSize)
		} else {
			log.Warn("failed", "name", s.name, "error", s.err)
		}
	}
	return setup
}

type meteredPipe struct {
	rw      p2p.MsgReadWriter
	maxSize uint32
}

func (m *meteredPipe) ReadMsg() (p2p.Msg, error) {
	return m.rw.ReadMsg()
}

func (m *meteredPipe) WriteMsg(msg p2p.Msg) error {
	if msg.Size > m.maxSize {
		m.maxSize = msg.Size
	}
	return m.rw.WriteMsg(msg)
}

func (pm *ProtocolManager) measure(setup *benchmarkSetup, count int) error {
	clientPipe, serverPipe := p2p.MsgPipe()
	clientMeteredPipe := &meteredPipe{rw: clientPipe}
	serverMeteredPipe := &meteredPipe{rw: serverPipe}
	var id enode.ID
	rand.Read(id[:])
	clientPeer := pm.newPeer(lpv2, NetworkId, p2p.NewPeer(id, "client", nil), clientMeteredPipe)
	serverPeer := pm.newPeer(lpv2, NetworkId, p2p.NewPeer(id, "server", nil), serverMeteredPipe)
	serverPeer.sendQueue = newExecQueue(count)
	serverPeer.announceType = announceTypeNone
	serverPeer.fcCosts = make(requestCostTable)
	c := &requestCosts{}
	for code, _ := range requests {
		serverPeer.fcCosts[code] = c
	}
	serverPeer.fcParams = flowcontrol.ServerParams{BufLimit: 1, MinRecharge: 1}
	serverPeer.fcClient = flowcontrol.NewClientNode(pm.server.fcManager, serverPeer.fcParams)

	if err := setup.req.init(pm, count); err != nil {
		return err
	}

	errCh := make(chan error, 10)
	start := mclock.Now()

	go func() {
		for i := 0; i < count; i++ {
			if err := setup.req.request(clientPeer, i); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		for i := 0; i < count; i++ {
			if err := pm.handleMsg(serverPeer); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		for i := 0; i < count; i++ {
			msg, err := clientPipe.ReadMsg()
			if err != nil {
				errCh <- err
				return
			}
			var i interface{}
			msg.Decode(&i)
		}
		// at this point we can be sure that the other two
		// goroutines finished successfully too
		close(errCh)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
	case <-pm.quitSync:
		clientPipe.Close()
		serverPipe.Close()
		return fmt.Errorf("Benchmark cancelled")
	}

	setup.totalTime += time.Duration(mclock.Now() - start)
	setup.totalCount += count
	setup.maxInSize = clientMeteredPipe.maxSize
	setup.maxOutSize = serverMeteredPipe.maxSize
	clientPipe.Close()
	serverPipe.Close()
	//serverPeer.fcClient.Remove(pm.server.fcManager)
	return nil
}

type requestCostStats struct {
	costs requestCostTable
	stats map[uint64][]uint64
}

func newCostStats(table requestCostTable) *requestCostStats {
	stats := make(map[uint64][]uint64)
	for code, _ := range table {
		stats[code] = make([]uint64, 10)
	}
	return &requestCostStats{
		costs: table,
		stats: stats,
	}
}

func (s *requestCostStats) update(msgCode, reqCnt, cost uint64) {
	if s == nil {
		return // not initialized yet during benchmark
	}
	c := s.costs[msgCode]
	est := c.baseCost + reqCnt*c.reqCost
	cost <<= 4
	l := 0
	for l < 9 && cost > est {
		l++
		cost >>= 1
	}
	ptr := &s.stats[msgCode][l]
	atomic.AddUint64(ptr, 1)
}

func (s *requestCostStats) printStats() {
	if s.stats == nil {
		return
	}
	for code, arr := range s.stats {
		log.Debug("cost stats", "code", code, "1/16", arr[0], "1/8", arr[1], "1/4", arr[2], "1/2", arr[3], "1", arr[4], "2", arr[5], "4", arr[6], "8", arr[7], "16", arr[8], ">16", arr[9])
	}
}
