package pruner

import (
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
)

const (
	chainLength  = 128
	ancientLimit = 100 // blocks [1, 100) go to the ancient store, the rest stay in the key-value store
)

type testChain struct {
	dir      string
	gspec    *core.Genesis
	blocks   []*types.Block
	receipts []types.Receipts
}

// newTestChain writes a chain of chainLength blocks, one transfer per block,
// into an on-disk pebble database with a freezer, the same layout a node uses.
func newTestChain(t *testing.T) *testChain {
	t.Helper()
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		gspec   = &core.Genesis{
			Config:  params.TestChainConfig,
			Alloc:   types.GenesisAlloc{address: {Balance: big.NewInt(1e18)}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
	)
	_, blocks, receipts := core.GenerateChainWithGenesis(gspec, beacon.New(ethash.NewFaker()), chainLength, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{1})
		tx, err := types.SignTx(types.NewTransaction(b.TxNonce(address), common.Address{2}, big.NewInt(1000), params.TxGas, b.BaseFee(), nil), signer, key)
		if err != nil {
			t.Fatal(err)
		}
		b.AddTx(tx)
	})

	tc := &testChain{dir: t.TempDir(), gspec: gspec, blocks: blocks, receipts: receipts}
	db := tc.open(t, false)
	defer db.Close()

	chain, err := core.NewBlockChain(db, nil, gspec, beacon.New(ethash.NewFaker()), core.DefaultConfig().WithStateScheme(rawdb.PathScheme))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := chain.InsertReceiptChain(blocks, types.EncodeBlockReceiptLists(receipts), ancientLimit); err != nil {
		t.Fatalf("insert receipt chain at %d: %v", n, err)
	}
	chain.Stop()

	// Index every transaction so we can check the index is pruned too.
	for _, b := range blocks {
		rawdb.WriteTxLookupEntriesByBlock(db, b)
	}
	rawdb.WriteTxIndexTail(db, 0)
	return tc
}

func (tc *testChain) open(t *testing.T, readonly bool) ethdb.Database {
	t.Helper()
	dir := filepath.Join(tc.dir, "l2chaindata")
	db, err := node.OpenDatabase(node.InternalOpenOptions{
		Directory: dir,
		DbEngine:  "pebble",
		DatabaseOptions: node.DatabaseOptions{
			AncientsDirectory: filepath.Join(dir, "ancient"),
			Cache:             16,
			Handles:           16,
			ReadOnly:          readonly,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func (tc *testChain) freezerDir() string {
	return ResolveFreezerDir(filepath.Join(tc.dir, "l2chaindata", "ancient"))
}

func TestPruneByBlocks(t *testing.T) {
	tc := newTestChain(t)
	db := tc.open(t, false)
	defer db.Close()

	plan, err := MakePlan(db, tc.freezerDir(), Options{RetainBlocks: 64})
	if err != nil {
		t.Fatal(err)
	}
	wantCutoff := uint64(chainLength - 64 + 1)
	if plan.Head != chainLength || plan.Cutoff != wantCutoff || plan.Tail != 0 {
		t.Fatalf("plan head=%d tail=%d cutoff=%d, want head=%d tail=0 cutoff=%d", plan.Head, plan.Tail, plan.Cutoff, chainLength, wantCutoff)
	}
	if err := Execute(db, plan); err != nil {
		t.Fatal(err)
	}
	if tail, _ := db.Tail(); tail != wantCutoff {
		t.Fatalf("tail %d, want %d", tail, wantCutoff)
	}
	for _, b := range tc.blocks {
		n, h := b.NumberU64(), b.Hash()
		if rawdb.ReadHeader(db, h, n) == nil {
			t.Fatalf("header %d missing after prune", n)
		}
		body := rawdb.ReadBody(db, h, n)
		rcpts := rawdb.ReadReceiptsRLP(db, h, n)
		lookup := rawdb.ReadTxLookupEntry(db, b.Transactions()[0].Hash())
		if n < wantCutoff {
			if body != nil || rcpts != nil {
				t.Fatalf("block %d: body or receipts still present below cutoff", n)
			}
			if lookup != nil {
				t.Fatalf("block %d: tx lookup still present below cutoff", n)
			}
		} else {
			if body == nil || rcpts == nil {
				t.Fatalf("block %d: body or receipts missing above cutoff", n)
			}
			if lookup == nil || *lookup != n {
				t.Fatalf("block %d: tx lookup missing above cutoff", n)
			}
		}
	}
	if tail := rawdb.ReadTxIndexTail(db); tail == nil || *tail != wantCutoff {
		t.Fatalf("tx index tail %v, want %d", tail, wantCutoff)
	}

	// Running the same retention again is a no-op.
	again, err := MakePlan(db, tc.freezerDir(), Options{RetainBlocks: 64})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Noop() {
		t.Fatalf("second plan should be a no-op, cutoff %d tail %d", again.Cutoff, again.Tail)
	}
}

func TestPruneByTime(t *testing.T) {
	tc := newTestChain(t)
	db := tc.open(t, true)
	defer db.Close()

	// Keep blocks at or after block 70's timestamp.
	target := tc.blocks[69] // block 70
	now := time.Unix(int64(target.Time()), 0).Add(time.Hour)
	plan, err := MakePlan(db, tc.freezerDir(), Options{Retention: time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Cutoff != 70 {
		t.Fatalf("cutoff %d, want 70", plan.Cutoff)
	}
	if plan.CutoffHash != target.Hash() {
		t.Fatalf("cutoff hash mismatch")
	}
}

func TestRefuseCutoffAboveFreezer(t *testing.T) {
	tc := newTestChain(t)
	db := tc.open(t, true)
	defer db.Close()

	// Keeping only 10 blocks would need to cut into the key-value store.
	if _, err := MakePlan(db, tc.freezerDir(), Options{RetainBlocks: 10}); err == nil {
		t.Fatal("expected an error for a cutoff above the ancient store")
	}
}

func TestDryRunEstimateReadsIndex(t *testing.T) {
	tc := newTestChain(t)
	db := tc.open(t, true)
	defer db.Close()

	plan, err := MakePlan(db, tc.freezerDir(), Options{RetainBlocks: 64})
	if err != nil {
		t.Fatal(err)
	}
	// The test chain fits in one data file per table, so nothing is deleted
	// on disk yet; the items are hidden until their file fills up.
	if len(plan.Tables) != 2 || plan.BytesFreed() != 0 {
		t.Fatalf("unexpected estimate %+v", plan.Tables)
	}
}

// TestUnpatchedGethRejectsPrunedDB documents why the Nitro image needs the
// accompanying go-ethereum patch: stock "keep all history" mode refuses a
// database whose tail is not a predefined prune point.
func TestUnpatchedGethRejectsPrunedDB(t *testing.T) {
	tc := newTestChain(t)
	db := tc.open(t, false)
	plan, err := MakePlan(db, tc.freezerDir(), Options{RetainBlocks: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := Execute(db, plan); err != nil {
		t.Fatal(err)
	}
	_, err = core.NewBlockChain(db, nil, tc.gspec, beacon.New(ethash.NewFaker()), core.DefaultConfig().WithStateScheme(rawdb.PathScheme))
	db.Close()
	if err == nil {
		t.Skip("this go-ethereum accepts pruned databases; the image patch is not needed")
	}
	t.Logf("unpatched geth refuses the pruned database as expected: %v", err)
}
