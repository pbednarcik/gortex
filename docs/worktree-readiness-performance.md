# Worktree readiness and sparse-generation scans

Follow-up to issue #767. This change addresses admission priority and a confirmed
generation-scan amplification path. It does not claim to resolve every source of
startup latency, write amplification, or first-view latency.

## Observed failure

A newly selected automatic checkout was registered but had no published route.
The primary was ready, and its recorded HEAD tree matched the new checkout.
Nevertheless, the initial route waited for physical-build admission. A preceding
worktree commit-layer rebuild took 252 seconds, including 44 seconds parsing 202
files; another rebuild followed it. Repeated selections did not promote an
already-running coordinator. Runtime stacks showed background admission waits,
but did not expose checkout IDs, so an exact queue position was not established.

The active builder also reached `ScanRepoCapabilityEdges`. Its predicates kept
results generation-isolated, but `NOT INDEXED` forced traversal of shared edge-ID
ranges before filtering to the requested generation. The page limit bounded
matching results, not examined rows. This is keyset pagination, not an OFFSET
rescan of the same prefix on every page.

## Behavior and invariants

- Selecting an automatic checkout coalesces admission demand. Both a newly
  activated coordinator and an already-queued coordinator can become interactive.
- Demand does not cancel a running build, reset debounce, or schedule redundant
  reconcile cycles. The gate consumes demand under its mutex when making
  admission decisions, avoiding a missed-promotion race with slot release.
- A promoted request joins the interactive queue's tail. If that queue is full,
  it keeps its background position and pending promotion. Existing queue bounds,
  FIFO order within each class, and the four-interactive-build fairness rule are
  preserved. There is still only one active physical build.
- Queue statistics can continue to classify buffered demand as background until
  the next scheduling decision. No checkout identity is added to metric labels.
- Positive-generation capability scans and their high-water lookup explicitly
  satisfy the existing partial `edges_by_generation(view_gen, id)` index. The
  generation-zero query is unchanged. No schema migration or new index is needed.
- Projection fields, generation-aware joins, repository filtering, pagination,
  high-water behavior, and enrichment remain intact. The optional index is not
  forced with `INDEXED BY`, so its absence does not turn a query into an error.
- Base fallback remains read-only and is never advertised as an exact checkout.

## Validation

Deterministic gate/coordinator tests hold the physical slot, queue background
work, select a later checkout, and verify admission order. They cover saturation,
fairness, cancellation, repeated requests, transition-owned coordinators, and
concurrent coalescing. The focused gate and coordinator suites also run under the
race detector.

Scanner tests cover colliding logical IDs in different generations, nil/empty/
scoped repository selections, more than 4,096 candidates, callback re-entry,
high-water stability, early termination, and operation without the optional
index. Query-plan assertions exercise the production SQL, not an approximation.

The isolated scanner benchmark uses the same file-backed store for legacy and
indexed controls, with 128 target edges and either zero or 50,000 foreign edges.
On the development machine, three 500 ms samples with foreign edges measured
4.84–5.02 ms/op for the legacy query and 0.502–0.537 ms/op for the indexed query
(about 9.2x at the medians). The zero-foreign case was noisy and did not show a
universal improvement. These are query microbenchmarks, not end-to-end latency or
SSD NAND-write measurements.

Three gate microbenchmark samples measured ordinary admission at a median
170.8 ns/op, admission with an undemanded channel at 180.9 ns/op, and prebuffered
demand at 209.5 ns/op (264 B and five allocations for each path). Scanning 1,024
promotable waiters measured 9.48 microseconds with no allocations. Repeated
coalesced coordinator notification measured 14.63–21.77 ns/op with no allocations.
These compare paths in the patched implementation; they are not a historical
before/after benchmark or a prediction of end-to-end latency.

An opt-in subprocess test exercises configured cold startup, automatic discovery
of a clean linked checkout, exact search, edit dry-run, real MCP edit, refreshed
search, replacement masking, primary isolation, warm restart, and removal. It
uses a private Git fixture, XDG directories, store, socket, and captured child PID;
it never restarts or addresses the user's daemon. It does not simulate an
occupied build slot; deterministic coordinator tests cover that condition.

The fixed-binary lifecycle run passed in 43.30 seconds, including automatic
removal grace. Its observed clean-worktree search, edit-to-search, and private
warm-restart-to-search times were 3.21 s, 1.24 s, and 1.91 s respectively. Other
tests were running concurrently, so these are successful-run observations, not
comparative performance results.

The existing isolated idle-I/O regression also passed cold and warm 15-second
intervals. Neither interval changed graph counts or allocated generations. The
process disk-write counter delta was zero in both, but logical-write deltas were
360,448 and 344,064 bytes, and each WAL grew 24,720 bytes. This is deliberately
not reported as zero I/O or proof of production-scale behavior. The fixture uses
an accelerated five-second janitor interval and an 8 MiB write-budget assertion.

```sh
go test ./internal/graph/store_sqlite -run '^TestCapabilityGeneration' -count=3
go test -race ./internal/indexer -run '^Test(ViewBuildGate|CheckoutSelection)' -count=10
go test ./internal/graph/store_sqlite -run '^$' -bench CapabilityGeneration -benchmem
go test ./internal/indexer -run '^$' -bench 'ViewBuildGate|CheckoutSelectionDemand' -benchmem

go build -o /absolute/private/path/gortex ./cmd/gortex
GORTEX_ISSUE767_READINESS_BINARY=/absolute/private/path/gortex \
  go test ./cmd/gortex -run '^TestIssue767WorktreeReadinessIntegration$' \
  -count=1 -timeout=10m -v
```

## Remaining work

An active expensive build can still delay first use. A same-tree check alone is
not a safe admission bypass: dirty construction re-samples the checkout, and an
adopted generation can enter a physical parse even when a provisional plan is
empty. An eventual metadata-only path needs an explicit empty-only builder
contract that refuses nonempty/adopted work before parsing or enrichment and
preserves final base/configuration/dirty identity validation and route CAS.

Large-generation retirement was another sampled cost during an abandoned build.
Its core deletion queries already use generation indexes; this patch does not
change cleanup scheduling, reference checks, leases, sealing, or chunk semantics.
Phase attribution and production-shaped cold/warm/retirement replay remain
necessary before making broader performance claims.
