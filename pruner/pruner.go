// Package pruner removes old block bodies and receipts from a geth-style chain
// database (Arbitrum Nitro's l2chaindata, or a plain geth chaindata), while
// keeping every header and the full state.
//
// It works offline, on a stopped node, in two steps that mirror upstream
// `geth prune-history`:
//
//  1. drop transaction-lookup entries for blocks below the cutoff, and
//  2. truncate the tail of the prunable ancient tables (bodies, receipts).
//
// Headers and canonical hashes are not prunable tables, so they stay.
package pruner

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

// Options selects the cutoff. Exactly one of Retention or RetainBlocks is set.
type Options struct {
	// Retention keeps blocks whose timestamp is newer than Now-Retention.
	Retention time.Duration
	// RetainBlocks keeps the newest N blocks.
	RetainBlocks uint64
	// Now is the reference time for Retention. Zero means time.Now().
	Now time.Time
}

// TableEstimate describes the effect of the cutoff on one ancient table.
type TableEstimate struct {
	Name         string
	FilesDeleted int
	BytesFreed   int64
}

// Plan is what a prune would do. It is computed without modifying anything.
type Plan struct {
	Head       uint64 // latest known block (max of head block and head header)
	Frozen     uint64 // number of blocks in the ancient store
	Tail       uint64 // current first block with a body in the ancient store
	Cutoff     uint64 // new tail: first block whose body and receipts are kept
	CutoffHash common.Hash
	CutoffTime time.Time
	Tables     []TableEstimate
}

// BytesFreed is the total size of data files that the prune deletes.
func (p *Plan) BytesFreed() int64 {
	var total int64
	for _, t := range p.Tables {
		total += t.BytesFreed
	}
	return total
}

// Noop reports whether the cutoff is not above the current tail.
func (p *Plan) Noop() bool { return p.Cutoff <= p.Tail }

// prunableTables are the chain freezer tables that TruncateTail shortens.
var prunableTables = []string{rawdb.ChainFreezerBodiesTable, rawdb.ChainFreezerReceiptTable}

// MakePlan computes the cutoff and estimates the space it frees. freezerDir is
// the chain freezer directory (see ResolveFreezerDir).
func MakePlan(db ethdb.Database, freezerDir string, opts Options) (*Plan, error) {
	if (opts.Retention > 0) == (opts.RetainBlocks > 0) {
		return nil, errors.New("set exactly one of retention or retain-blocks")
	}
	head, err := headNumber(db)
	if err != nil {
		return nil, err
	}
	frozen, err := db.Ancients()
	if err != nil {
		return nil, fmt.Errorf("reading ancient item count: %w", err)
	}
	tail, err := db.Tail()
	if err != nil {
		return nil, fmt.Errorf("reading ancient tail: %w", err)
	}
	plan := &Plan{Head: head, Frozen: frozen, Tail: tail}

	if opts.RetainBlocks > 0 {
		if opts.RetainBlocks >= head {
			plan.Cutoff = 0
		} else {
			plan.Cutoff = head - opts.RetainBlocks + 1
		}
	} else {
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		cutoff, err := firstBlockAtOrAfter(db, tail, head, uint64(now.Add(-opts.Retention).Unix()))
		if err != nil {
			return nil, err
		}
		plan.Cutoff = cutoff
	}
	if plan.Noop() {
		plan.Cutoff = tail
		return plan, nil
	}
	// Only data already in the ancient store can be truncated. Blocks above
	// `frozen` live in the key-value store and are always recent.
	if plan.Cutoff >= frozen {
		return nil, fmt.Errorf("cutoff %d is not below the ancient store head %d; retention is shorter than what the node keeps outside the freezer", plan.Cutoff, frozen)
	}
	hash := rawdb.ReadCanonicalHash(db, plan.Cutoff)
	if hash == (common.Hash{}) {
		return nil, fmt.Errorf("no canonical hash for cutoff block %d", plan.Cutoff)
	}
	header := rawdb.ReadHeader(db, hash, plan.Cutoff)
	if header == nil {
		return nil, fmt.Errorf("no header for cutoff block %d", plan.Cutoff)
	}
	plan.CutoffHash = hash
	plan.CutoffTime = time.Unix(int64(header.Time), 0).UTC()

	for _, name := range prunableTables {
		est, err := estimateTable(freezerDir, name, plan.Cutoff)
		if err != nil {
			return nil, err
		}
		plan.Tables = append(plan.Tables, est)
	}
	return plan, nil
}

// Execute applies a plan. The node must be stopped; the database lock taken
// when opening it guarantees that.
func Execute(db ethdb.Database, plan *Plan) error {
	if plan.Noop() {
		return nil
	}
	// Re-check the cutoff hash so a plan is never applied to a different chain.
	if got := rawdb.ReadCanonicalHash(db, plan.Cutoff); got != plan.CutoffHash {
		return fmt.Errorf("cutoff block %d hash changed: plan %s, database %s", plan.Cutoff, plan.CutoffHash, got)
	}
	// Index first: if we crash after this, the bodies are still there and a
	// re-run finishes the job. The reverse order would leave lookups pointing
	// at deleted bodies.
	rawdb.PruneTransactionIndex(db, plan.Cutoff)
	if _, err := db.TruncateTail(plan.Cutoff); err != nil {
		return fmt.Errorf("truncating ancient tail to %d: %w", plan.Cutoff, err)
	}
	if err := db.SyncAncient(); err != nil {
		return fmt.Errorf("syncing ancient store: %w", err)
	}
	tail, err := db.Tail()
	if err != nil {
		return err
	}
	if tail != plan.Cutoff {
		return fmt.Errorf("ancient tail is %d after truncation, expected %d", tail, plan.Cutoff)
	}
	return nil
}

func headNumber(db ethdb.Database) (uint64, error) {
	var head uint64
	found := false
	for _, hash := range []common.Hash{rawdb.ReadHeadBlockHash(db), rawdb.ReadHeadHeaderHash(db)} {
		if hash == (common.Hash{}) {
			continue
		}
		if n, ok := rawdb.ReadHeaderNumber(db, hash); ok {
			head = max(head, n)
			found = true
		}
	}
	if !found {
		return 0, errors.New("database has no head block; is this a chain database?")
	}
	return head, nil
}

// firstBlockAtOrAfter binary-searches canonical headers in [lo, hi] for the
// first block with timestamp >= ts. Headers are never pruned, and canonical
// block timestamps never decrease, so the search is valid. If every block is
// older than ts, it returns hi, so the head block is always kept.
func firstBlockAtOrAfter(db ethdb.Database, lo, hi, ts uint64) (uint64, error) {
	headerTime := func(n uint64) (uint64, error) {
		h := readCanonicalHeader(db, n)
		if h == nil {
			return 0, fmt.Errorf("missing canonical header %d", n)
		}
		return h.Time, nil
	}
	var searchErr error
	idx := sort.Search(int(hi-lo+1), func(i int) bool {
		if searchErr != nil {
			return true
		}
		t, err := headerTime(lo + uint64(i))
		if err != nil {
			searchErr = err
			return true
		}
		return t >= ts
	})
	if searchErr != nil {
		return 0, searchErr
	}
	return min(lo+uint64(idx), hi), nil
}

func readCanonicalHeader(db ethdb.Reader, n uint64) *types.Header {
	hash := rawdb.ReadCanonicalHash(db, n)
	if hash == (common.Hash{}) {
		return nil
	}
	return rawdb.ReadHeader(db, hash, n)
}

// ResolveFreezerDir returns the chain freezer directory for an ancient root.
// Newer geth keeps chain data in <ancient>/chain; older layouts (and Nitro
// databases created by them) keep it directly in <ancient>. This matches
// rawdb's own resolution.
func ResolveFreezerDir(ancient string) string {
	chain := filepath.Join(ancient, rawdb.ChainFreezerName)
	if fi, err := os.Stat(chain); err == nil && fi.IsDir() {
		return chain
	}
	if _, err := os.Stat(filepath.Join(ancient, rawdb.ChainFreezerHeaderTable+".cidx")); err == nil {
		return ancient
	}
	return chain
}

const indexEntrySize = 6

// estimateTable reads a freezer table index to find which data files lie
// entirely below the cutoff. Index format: each 6-byte entry is a big-endian
// uint16 file number and uint32 end offset. Entry 0 holds the tail file
// number and the count of already-deleted items; entry i+1 marks the end of
// item (deleted + i).
func estimateTable(dir, name string, cutoff uint64) (TableEstimate, error) {
	est := TableEstimate{Name: name}
	f, err := os.Open(filepath.Join(dir, name+".cidx"))
	if err != nil {
		return est, fmt.Errorf("opening %s index: %w", name, err)
	}
	defer f.Close()

	buf := make([]byte, indexEntrySize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return est, fmt.Errorf("reading %s index header: %w", name, err)
	}
	tailFile := uint32(binary.BigEndian.Uint16(buf[:2]))
	deleted := uint64(binary.BigEndian.Uint32(buf[2:6]))
	if cutoff <= deleted {
		return est, nil
	}
	fi, err := f.Stat()
	if err != nil {
		return est, err
	}
	entries := uint64(fi.Size()) / indexEntrySize
	pos := cutoff - deleted + 1
	if pos >= entries {
		return est, fmt.Errorf("cutoff %d is past the end of the %s table", cutoff, name)
	}
	if _, err := f.ReadAt(buf, int64(pos*indexEntrySize)); err != nil {
		return est, fmt.Errorf("reading %s index entry: %w", name, err)
	}
	newTailFile := uint32(binary.BigEndian.Uint16(buf[:2]))
	for n := tailFile; n < newTailFile; n++ {
		fi, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%s.%04d.cdat", name, n)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return est, err
		}
		est.FilesDeleted++
		est.BytesFreed += fi.Size()
	}
	return est, nil
}
