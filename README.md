# nitro-history-pruner

Keep only recent block history on an Arbitrum Nitro node, while keeping the
full state.

An Arbitrum One full node stores every block body and receipt since genesis:
about 1.4 TB in late 2026, and growing. Most RPC users only need recent
blocks. This tool deletes bodies and receipts older than a retention window,
for example 180 days, and frees about 1 TB on Arbitrum One.

What is kept:

- **All block headers and canonical hashes**, so the chain stays verifiable.
- **The full current state.** State pruning is a separate thing; see
  Nitro's `--init.prune`.
- **Bodies, receipts and transaction lookups** inside the retention window.

Requests for older blocks return Geth's standard
`pruned history unavailable` error (code 4444).

## How it works

It works offline, on a stopped node, the same way upstream `geth prune-history`
does:

1. It drops transaction-lookup entries for blocks below the cutoff.
2. It truncates the tail of the `bodies` and `receipts` ancient tables up to
   the cutoff. Whole 2 GB data files below the cutoff are deleted.

The cutoff is the first block whose timestamp falls inside the retention
window. It's found by binary search over headers, so `-retention 180d` means
180 days of wall-clock time, whatever the block rate.

### Why Nitro needs a patch

By default, Geth refuses to start on a database whose history was cut at a
block it doesn't know (`unexpected database tail`). Upstream Geth only
recognizes the Ethereum Merge block. Nitro doesn't set a history mode, so a
pruned Arbitrum database fails at startup.

[`patches/`](patches) holds a small patch to Nitro's go-ethereum. It accepts
any ancient tail whose header is still present and uses it as the history
cutoff. The patch includes a test.
[`scripts/build-nitro-image.sh`](scripts/build-nitro-image.sh) builds an image
that is the official Nitro image with only `/usr/local/bin/nitro` replaced, and
the pruner added.

## Build the image

On an amd64 host with Docker and about 40 GB of free disk:

```bash
scripts/build-nitro-image.sh v3.11.3 offchainlabs/nitro-node:v3.11.3-beb2108 nitro-node-pruned:v3.11.3
```

## Prune

```bash
docker stop -t 1800 arbitrum-nitro

# Dry run: prints the cutoff block and how much space it frees.
docker run --rm -v /var/lib/arbitrum:/home/user/.arbitrum \
  --entrypoint nitro-history-pruner nitro-node-pruned:v3.11.3 \
  -chaindata /home/user/.arbitrum/arb1/nitro/l2chaindata -retention 180d

# Prune for real.
docker run --rm -v /var/lib/arbitrum:/home/user/.arbitrum \
  --entrypoint nitro-history-pruner nitro-node-pruned:v3.11.3 \
  -chaindata /home/user/.arbitrum/arb1/nitro/l2chaindata -retention 180d -yes

# Start Nitro from the patched image.
```

Then run Nitro from `nitro-node-pruned:<tag>` with the same flags as before.
Setting `--execution.tx-indexer.tx-lookup-limit` to about the retention window
in blocks (180 days ≈ 62,208,000 blocks on Arbitrum One) keeps the transaction
index matched to it.

Re-run the pruner on a schedule, for example monthly, to keep a rolling window.

| Flag | Meaning |
|---|---|
| `-chaindata` | Path to `l2chaindata` (or a Geth `chaindata`). Required. |
| `-ancient` | Ancient store, if it's not `<chaindata>/ancient`. |
| `-retention` | Age to keep: `180d`, `90d`, `720h`. |
| `-retain-blocks` | Alternatively, the number of newest blocks to keep. |
| `-yes` | Actually prune. Without it, the tool only prints the plan. |

The tool opens the database read-write only with `-yes`, which takes the
database lock, so it can't run alongside a live node.

## Caveats

- **Pruning can't be undone.** To get old history back, re-sync or restore a
  snapshot.
- **The transaction-index step scans every lookup entry.** On Arbitrum One that
  takes tens of minutes. Bodies and receipts are deleted in seconds.
- **Every Nitro release needs the patch re-applied.** Patches live in
  `patches/<nitro tag>/`; the build script stops if none exist for the tag you
  ask for.
- **Not for validators.** Validators need old blocks to build proofs. Run this
  only on RPC and full nodes.

## Development

```bash
go test ./...
```

The tests build a real on-disk chain with a freezer, prune it, and check
headers, bodies, receipts and transaction lookups on both sides of the cutoff.
One test checks that unpatched Geth refuses the pruned database.

## License

LGPL-3.0, the same as the go-ethereum library code it builds on. See
`COPYING.LESSER` and `COPYING`.
