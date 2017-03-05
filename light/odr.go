// Copyright 2015 The go-ethereum Authors
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

// Package light implements on-demand retrieval capable state and chain objects
// for the Ethereum Light Client.
package light

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"golang.org/x/net/context"
)

// NoOdr is the default context passed to an ODR capable function when the ODR
// service is not required.
var NoOdr = context.Background()

// OdrBackend is an interface to a backend service that handles ODR retrievals
type OdrBackend interface {
	Database() ethdb.Database
	Retrieve(ctx context.Context, req OdrRequest) error
}

// OdrRequest is an interface for retrieval requests
type OdrRequest interface {
	StoreResult(db ethdb.Database)
}

// TrieID identifies a state or account storage trie
type TrieID struct {
	BlockHash, Root common.Hash
	BlockNumber     uint64
	AccKey          []byte
}

// StateTrieID returns a TrieID for a state trie belonging to a certain block
// header.
func StateTrieID(header *types.Header) *TrieID {
	return &TrieID{
		BlockHash:   header.Hash(),
		BlockNumber: header.Number.Uint64(),
		AccKey:      nil,
		Root:        header.Root,
	}
}

// StorageTrieID returns a TrieID for a contract storage trie at a given account
// of a given state trie. It also requires the root hash of the trie for
// checking Merkle proofs.
func StorageTrieID(state *TrieID, addr common.Address, root common.Hash) *TrieID {
	return &TrieID{
		BlockHash:   state.BlockHash,
		BlockNumber: state.BlockNumber,
		AccKey:      crypto.Keccak256(addr[:]),
		Root:        root,
	}
}

// TrieRequest is the ODR request type for state/storage trie entries
type TrieRequest struct {
	OdrRequest
	Id    *TrieID
	Key   []byte
	Proof *ProofDb
}

// StoreResult stores the retrieved data in local database
func (req *TrieRequest) StoreResult(db ethdb.Database) {
	req.Proof.StoreAll(db)
}

// CodeRequest is the ODR request type for retrieving contract code
type CodeRequest struct {
	OdrRequest
	Id   *TrieID
	Hash common.Hash
	Data []byte
}

// StoreResult stores the retrieved data in local database
func (req *CodeRequest) StoreResult(db ethdb.Database) {
	db.Put(req.Hash[:], req.Data)
}

// BlockRequest is the ODR request type for retrieving block bodies
type BlockRequest struct {
	OdrRequest
	Hash   common.Hash
	Number uint64
	Rlp    []byte
}

// StoreResult stores the retrieved data in local database
func (req *BlockRequest) StoreResult(db ethdb.Database) {
	core.WriteBodyRLP(db, req.Hash, req.Number, req.Rlp)
}

// ReceiptsRequest is the ODR request type for retrieving block bodies
type ReceiptsRequest struct {
	OdrRequest
	Hash     common.Hash
	Number   uint64
	Receipts types.Receipts
}

// StoreResult stores the retrieved data in local database
func (req *ReceiptsRequest) StoreResult(db ethdb.Database) {
	core.WriteBlockReceipts(db, req.Hash, req.Number, req.Receipts)
}

// ChtRequest is the ODR request type for retrieving old headers from a CHT structure
type ChtRequest struct {
	OdrRequest
	ChtNum, BlockNum uint64
	ChtRoot          common.Hash
	Header           *types.Header
	Td               *big.Int
	Proof            *ProofDb
}

// StoreResult stores the retrieved data in local database
func (req *ChtRequest) StoreResult(db ethdb.Database) {
	// if there is a canonical hash, there is a header too
	core.WriteHeader(db, req.Header)
	hash, num := req.Header.Hash(), req.Header.Number.Uint64()
	core.WriteTd(db, hash, num, req.Td)
	core.WriteCanonicalHash(db, hash, num)
}

// BloomRequest is the ODR request type for retrieving bloom filters from a CHT structure
type BloomRequest struct {
	OdrRequest
	BltNum, BitIdx uint64
	SectionIdxList []uint64
	BltRoot        common.Hash
	BloomBits      [][]byte
	Proofs         *ProofDb
}

// StoreResult stores the retrieved data in local database
func (req *BloomRequest) StoreResult(db ethdb.Database) {
	for i, sectionIdx := range req.SectionIdxList {
		core.StoreBloomBits(db, req.BitIdx, sectionIdx, req.BloomBits[i])
	}
}
