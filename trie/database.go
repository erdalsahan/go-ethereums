// Copyright 2022 The go-ethereum Authors
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

package trie

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/trie/triedb/hashdb"
	"github.com/ethereum/go-ethereum/trie/triedb/pathdb"
	"github.com/ethereum/go-ethereum/trie/trienode"
	"github.com/ethereum/go-ethereum/trie/triestate"
)

// Config defines all necessary options for database.
type Config struct {
	Preimages bool           // Flag whether the preimage of node key is recorded
	HashDB    *hashdb.Config // Configs for hash-based scheme
	PathDB    *pathdb.Config // Configs for experimental path-based scheme
	Zktrie    bool           // use zktrie

	ExperimentalZkTrie bool // use zktree
}

// HashDefaults represents a config for using hash-based scheme with
// default settings.
var HashDefaults = &Config{
	Preimages: false,
	HashDB:    hashdb.Defaults,
}

var ZkHashDefaults = &Config{
	Preimages: false,
	HashDB:    hashdb.Defaults,
	Zktrie:    true,
}

func GetHashDefaults(isZk bool) *Config {
	if isZk {
		return ZkHashDefaults
	}
	return HashDefaults
}

// backend defines the methods needed to access/update trie nodes in different
// state scheme.
type backend interface {
	// Scheme returns the identifier of used storage scheme.
	Scheme() string

	// Initialized returns an indicator if the state data is already initialized
	// according to the state scheme.
	Initialized(genesisRoot common.Hash) bool

	// Size returns the current storage size of the diff layers on top of the
	// disk layer and the storage size of the nodes cached in the disk layer.
	//
	// For hash scheme, there is no differentiation between diff layer nodes
	// and dirty disk layer nodes, so both are merged into the second return.
	Size() (common.StorageSize, common.StorageSize)

	// Update performs a state transition by committing dirty nodes contained
	// in the given set in order to update state from the specified parent to
	// the specified root.
	//
	// The passed in maps(nodes, states) will be retained to avoid copying
	// everything. Therefore, these maps must not be changed afterwards.
	Update(root common.Hash, parent common.Hash, block uint64, nodes *trienode.MergedNodeSet, states *triestate.Set) error

	// Commit writes all relevant trie nodes belonging to the specified state
	// to disk. Report specifies whether logs will be displayed in info level.
	Commit(root common.Hash, report bool) error

	// Close closes the trie database backend and releases all held resources.
	Close() error
}

// Database is the wrapper of the underlying backend which is shared by different
// types of node backend as an entrypoint. It's responsible for all interactions
// relevant with trie nodes and node preimages.
type Database struct {
	config    *Config        // Configuration for trie database
	diskdb    ethdb.Database // Persistent database to store the snapshot
	preimages *preimageStore // The store for caching preimages
	backend   backend        // The backend for managing trie nodes
}

func NewZkDatabase(diskdb ethdb.Database) *Database {
	return NewDatabase(diskdb, ZkHashDefaults)
}

// NewDatabase initializes the trie database with default settings, note
// the legacy hash-based scheme is used by default.
func NewDatabase(diskdb ethdb.Database, config *Config) *Database {
	// Sanitize the config and use the default one if it's not specified.
	if config == nil {
		config = HashDefaults
	}
	var preimages *preimageStore
	if config.Preimages {
		preimages = newPreimageStore(diskdb)
	}
	db := &Database{
		config:    config,
		diskdb:    diskdb,
		preimages: preimages,
	}
	if config.HashDB != nil && config.PathDB != nil {
		log.Crit("Both 'hash' and 'path' mode are configured")
	}
	if config.PathDB != nil {
		if config.Zktrie {
			log.Crit("pbss does not support in zktrie")
		} else {
			db.backend = pathdb.New(diskdb, config.PathDB)
		}
	} else {
		if config.Zktrie {
			db.backend = hashdb.NewZk(diskdb, config.HashDB)
		} else {
			db.backend = hashdb.New(diskdb, config.HashDB, mptResolver{})
		}
	}
	return db
}

// Reader returns a reader for accessing all trie nodes with provided state root.
// An error will be returned if the requested state is not available.
func (db *Database) Reader(blockRoot common.Hash) (Reader, error) {
	switch b := db.backend.(type) {
	case *hashdb.Database:
		return b.Reader(blockRoot)
	case *pathdb.Database:
		return b.Reader(blockRoot)
	}
	return nil, errors.New("unknown backend")
}

// Update performs a state transition by committing dirty nodes contained in the
// given set in order to update state from the specified parent to the specified
// root. The held pre-images accumulated up to this point will be flushed in case
// the size exceeds the threshold.
//
// The passed in maps(nodes, states) will be retained to avoid copying everything.
// Therefore, these maps must not be changed afterwards.
func (db *Database) Update(root common.Hash, parent common.Hash, block uint64, nodes *trienode.MergedNodeSet, states *triestate.Set) error {
	if db.preimages != nil {
		db.preimages.commit(false)
	}
	return db.backend.Update(root, parent, block, nodes, states)
}

// Commit iterates over all the children of a particular node, writes them out
// to disk. As a side effect, all pre-images accumulated up to this point are
// also written.
func (db *Database) Commit(root common.Hash, report bool) error {
	if db.preimages != nil {
		db.preimages.commit(true)
	}
	return db.backend.Commit(root, report)
}

// Size returns the storage size of diff layer nodes above the persistent disk
// layer, the dirty nodes buffered within the disk layer, and the size of cached
// preimages.
func (db *Database) Size() (common.StorageSize, common.StorageSize, common.StorageSize) {
	var (
		diffs, nodes common.StorageSize
		preimages    common.StorageSize
	)
	diffs, nodes = db.backend.Size()
	if db.preimages != nil {
		preimages = db.preimages.size()
	}
	return diffs, nodes, preimages
}

// Initialized returns an indicator if the state data is already initialized
// according to the state scheme.
func (db *Database) Initialized(genesisRoot common.Hash) bool {
	return db.backend.Initialized(genesisRoot)
}

// Scheme returns the node scheme used in the database.
func (db *Database) Scheme() string {
	return db.backend.Scheme()
}

// Close flushes the dangling preimages to disk and closes the trie database.
// It is meant to be called when closing the blockchain object, so that all
// resources held can be released correctly.
func (db *Database) Close() error {
	db.WritePreimages()
	return db.backend.Close()
}

// WritePreimages flushes all accumulated preimages to disk forcibly.
func (db *Database) WritePreimages() {
	if db.preimages != nil {
		db.preimages.commit(true)
	}
}

// Cap iteratively flushes old but still referenced trie nodes until the total
// memory usage goes below the given threshold. The held pre-images accumulated
// up to this point will be flushed in case the size exceeds the threshold.
//
// It's only supported by hash-based database and will return an error for others.
func (db *Database) Cap(limit common.StorageSize) error {
	hdb, ok := db.backend.(hashdb.NodeDatabase)
	if !ok {
		return errors.New("not supported")
	}
	if db.preimages != nil {
		db.preimages.commit(false)
	}
	return hdb.Cap(limit)
}

// Reference adds a new reference from a parent node to a child node. This function
// is used to add reference between internal trie node and external node(e.g. storage
// trie root), all internal trie nodes are referenced together by database itself.
//
// It's only supported by hash-based database and will return an error for others.
func (db *Database) Reference(root common.Hash, parent common.Hash) error {
	hdb, ok := db.backend.(hashdb.NodeDatabase)
	if !ok {
		return errors.New("not supported")
	}
	hdb.Reference(root, parent)
	return nil
}

// Dereference removes an existing reference from a root node. It's only
// supported by hash-based database and will return an error for others.
func (db *Database) Dereference(root common.Hash) error {
	hdb, ok := db.backend.(hashdb.NodeDatabase)
	if !ok {
		return errors.New("not supported")
	}
	hdb.Dereference(root)
	return nil
}

// Node retrieves the rlp-encoded node blob with provided node hash. It's
// only supported by hash-based database and will return an error for others.
// Note, this function should be deprecated once ETH66 is deprecated.
func (db *Database) Node(hash common.Hash) ([]byte, error) {
	hdb, ok := db.backend.(hashdb.NodeDatabase)
	if !ok {
		return nil, errors.New("not supported")
	}
	return hdb.Node(hash)
}

// Recover rollbacks the database to a specified historical point. The state is
// supported as the rollback destination only if it's canonical state and the
// corresponding trie histories are existent. It's only supported by path-based
// database and will return an error for others.
func (db *Database) Recover(target common.Hash) error {
	pdb, ok := db.backend.(*pathdb.Database)
	if !ok {
		return errors.New("not supported")
	}
	return pdb.Recover(target, &trieLoader{db: db})
}

// Recoverable returns the indicator if the specified state is enabled to be
// recovered. It's only supported by path-based database and will return an
// error for others.
func (db *Database) Recoverable(root common.Hash) (bool, error) {
	pdb, ok := db.backend.(*pathdb.Database)
	if !ok {
		return false, errors.New("not supported")
	}
	return pdb.Recoverable(root), nil
}

// Reset wipes all available journal from the persistent database and discard
// all caches and diff layers. Using the given root to create a new disk layer.
// It's only supported by path-based database and will return an error for others.
func (db *Database) Reset(root common.Hash) error {
	pdb, ok := db.backend.(*pathdb.Database)
	if !ok {
		return errors.New("not supported")
	}
	return pdb.Reset(root)
}

// Journal commits an entire diff hierarchy to disk into a single journal entry.
// This is meant to be used during shutdown to persist the snapshot without
// flattening everything down (bad for reorgs). It's only supported by path-based
// database and will return an error for others.
func (db *Database) Journal(root common.Hash) error {
	pdb, ok := db.backend.(*pathdb.Database)
	if !ok {
		return errors.New("not supported")
	}
	return pdb.Journal(root)
}

// SetBufferSize sets the node buffer size to the provided value(in bytes).
// It's only supported by path-based database and will return an error for
// others.
func (db *Database) SetBufferSize(size int) error {
	pdb, ok := db.backend.(*pathdb.Database)
	if !ok {
		return errors.New("not supported")
	}
	return pdb.SetBufferSize(size)
}

func (db *Database) UpdatePreimage(preimage []byte, hashField *big.Int) {
	if _, ok := db.backend.(*hashdb.ZktrieDatabase); !ok {
		log.Error("non zkTrie database UpdatePreimage does not support ")
		return
	}
	if db.preimages != nil {
		// we must copy the input key
		preimages := make(map[common.Hash][]byte)
		preimages[common.BytesToHash(hashField.Bytes())] = common.CopyBytes(preimage)
		db.preimages.insertPreimage(preimages)
	}
}

func (db *Database) Put(k, v []byte) error {
	zdb, ok := db.backend.(*hashdb.ZktrieDatabase)
	if !ok {
		return errors.New("not supported")
	}
	return zdb.Put(k, v)
}

func (db *Database) Get(key []byte) ([]byte, error) {
	zdb, ok := db.backend.(*hashdb.ZktrieDatabase)
	if !ok {
		return nil, errors.New("not supported")
	}
	return zdb.Get(key)
}

func (db *Database) IsZk() bool          { return db.config.Zktrie }
func (db *Database) IsZkStateTrie() bool { return db.config.Zktrie && db.config.ExperimentalZkTrie }

func (db *Database) SetBackend(isZk bool) {
	if db.config.Zktrie == isZk {
		return
	}
	db.config = &Config{
		Preimages:          db.config.Preimages,
		HashDB:             db.config.HashDB,
		PathDB:             db.config.PathDB,
		Zktrie:             isZk,
		ExperimentalZkTrie: db.config.ExperimentalZkTrie,
	}
	if db.config.PathDB != nil {
		if isZk {
			log.Crit("pbss does not support in zktrie")
		} else {
			db.backend = pathdb.New(db.diskdb, db.config.PathDB)
		}
	} else {
		if isZk {
			db.backend = hashdb.NewZk(db.diskdb, db.config.HashDB)
		} else {
			db.backend = hashdb.New(db.diskdb, db.config.HashDB, mptResolver{})
		}
	}
}

func (db *Database) EmptyRoot() common.Hash {
	return types.GetEmptyRootHash(db.config != nil && db.config.Zktrie)
}
