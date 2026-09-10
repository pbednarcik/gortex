# Reference-fact write convergence

Performance specification and validation for [issue #767](https://github.com/zzet/gortex/issues/767).
For checkout mutation routing, see [worktree-source-mutations.md](worktree-source-mutations.md).

## Evidence and scope

The September 2026 investigation identified a concrete source of write amplification in
`Store.replaceRefFactsForFiles`: refreshing an unchanged frontier deleted its existing
reference facts and inserted the same projection again. A controlled 1,000-fact frontier
therefore performed 2,000 row mutations per call. A target-only change affecting one fact
also rewrote all 1,000 facts. A 15-second sample of the active production daemon showed
94.7% cumulative sampled CPU beneath this refresh path; that is CPU-stack attribution,
not a measurement of its share of disk bytes or evidence that the daemon was idle.

This fixes the demonstrated incremental reference-fact rewrite mechanism. It does not
establish that this mechanism explains every observation in issue #767. Mutation-batch
counts from different daemon lifetimes must not be used to calculate bytes per batch.
Repository watermarks are generation-scoped; a different checkout SHA alone is not proof
of cross-checkout corruption.

## Required behavior and implementation

For one `(view_gen, repo_prefix, source-file frontier)`, recomputing an identical reference-fact
projection must not mutate its persisted rows. Changed facts must update only their changed
payload; newly eligible facts must be inserted, and obsolete or newly ineligible facts removed.
File moves, target-name changes, candidates, language, resolution origin/tier, source deletion,
and cross-generation isolation must retain their existing semantics. A source-content hash
cannot replace this comparison: unchanged source can have changed resolution or target metadata.

The implementation retains a two-statement transaction: scoped deletion of identities no longer
in the desired projection, followed by an UPSERT whose update predicate compares all six non-key
payload columns using null-safe comparisons. The projection starts from requested files. Its
fact-identity collisions are collapsed deterministically before reconciliation; each statement
materializes its own desired set. The embedded SQLite plan must use a keyed anti-join lookup,
not a correlated full scan of the desired set for every old fact.

There is no schema migration, new index, public signature, full-repository rebuild, checkpoint
policy, or automatic compaction change. The query must not introduce a hard dependency on the
optional `nodes_by_file` index, which bulk loading can temporarily remove. The existing transaction
helpers return a plain SQL transaction and do not themselves advance graph revisions or invalidate
analysis on this reference-fact path. This does not rule out surrounding indexer activity.

## Measured storage regression

Apple M1 Pro, darwin/arm64, Go 1.27.0, embedded modernc SQLite v1.58.0; baseline
`0f462eff08089704618be886b221a5412feff04d`. Each case has 1,000 facts plus 10,000 unrelated
nodes. Results are medians of three 20-iteration runs; final timing was rerun after the heavy
race suite finished.

| Refresh | Baseline time | Fixed time | Baseline rows/op | Fixed rows/op | Baseline WAL bytes/op | Fixed WAL bytes/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Unchanged | 38.018 ms | 29.519 ms | 2,000 | 0 | 255,442 | 0 |
| One changed target | 38.032 ms | 30.229 ms | 2,000 | 1 | 267,802 | 20,602 |

The changed-target case includes the underlying target-node update in its time and WAL accounting;
the row-mutation metric counts only reference-fact refresh. A separate repeated-refresh test
asserts zero row mutations and WAL growth for identical facts. The changed-target WAL reduction
is 92.3%; these figures must not be extrapolated directly to whole-daemon or SSD NAND writes.

```sh
go test ./internal/graph/store_sqlite -run '^$' -bench '^BenchmarkRefFactRefreshWrites$' -benchtime=20x -count=3 -benchmem
go test -race ./internal/graph/store_sqlite -timeout=30m
```

Correctness tests cover generation/source/target isolation, eligibility boundaries, duplicate-edge
identity convergence, payload-only changes, moved/deleted source files, missing optional indexes,
existing rollback behavior, and the actual embedded-driver query plan. Normal storage/indexer
suites, the full storage race suite, targeted indexer and helper race tests, build, and scoped lint
are the validation gates for this change.

## Isolated daemon validation

The opt-in `TestIssue767IdleIOIntegration` uses explicit test binaries and private XDG
configuration/data/cache roots. It never invokes a global daemon restart, store deletion, or
re-track. Its primary checkout stays dirty while an automatic linked worktree is created, edited,
queried with an exact selector, and removed. Primary queries use its own CWD. Assertions cover
primary isolation, removal cleanup, warm restart, selected-primary facts before and after idle,
generation allocation (including create-and-GC cycles), and same-process I/O counters.

Default CI skips the daemon test unless a binary is supplied. Requested durations are bounded
and checked against Go's test deadline before creating fixtures; captured children have
cancellation/exit deadlines and identity-bound cleanup. The optional write budget is an explicit
assertion, not an invented default SLO.

The matched baseline/fixed 60-second lifecycle smoke passed, as did the final safeguarded
15-second cold/warm smoke and deadline-refusal tests. The final binary's continuous warm window
lasted 1,804.759 seconds: 1,212,416 process disk-accounted bytes (about 672 B/s) and 43,778,776
logical bytes (about 24.3 kB/s). WAL size changed from 78,312 to 539,752 bytes; this was not a
zero-I/O run. The primary stayed at 101 nodes, 200 edges, and two reference facts. Generation
allocation stayed at 2 with zero retained overlay generations, including six external progress
samples. Independent checks before the phase and after clean teardown confirmed both facts
belonged to primary generation 0 and repository `issue767`, with only the primary checkout left.
No restart or safety-cap crossing occurred during the window. The captured child, socket, and
PID file were absent after teardown.

The long run used the same final production binary as the final guarded short smoke
(SHA-256 `e1c8433ab2839513a87bac20a81b9f13d2e4282b28a916fb2e934402c32dfc9f`). It began
before the two harness safeguards were added; its 45-minute test deadline and independent
primary-scope checks supplied those protections. The finalized guarded harness was separately
smoke-tested, including refusal of a one-hour request under a five-second test deadline before
any fixture or child existed.

For a 30-minute warm run, supply absolute disposable outer XDG directories too:

```sh
env XDG_CONFIG_HOME=/absolute/disposable/check/config \
    XDG_DATA_HOME=/absolute/disposable/check/data \
    XDG_CACHE_HOME=/absolute/disposable/check/cache \
    GORTEX_ISSUE767_TEST_BINARY=/absolute/path/to/gortex \
    GORTEX_ISSUE767_ARTIFACT_DIR=/absolute/disposable/artifacts \
    GORTEX_ISSUE767_IDLE_DURATION=30m \
    go test ./cmd/gortex -run '^TestIssue767IdleIOIntegration$' -count=1 -timeout=45m -v
```

`GORTEX_ISSUE767_BASELINE_BINARY` optionally adds a matched baseline run; allow a larger test
deadline for two long runs. `GORTEX_ISSUE767_WRITE_BUDGET_BYTES` optionally sets a positive
per-phase disk-accounted write budget for the fixed binary; unset or zero is measurement-only.

## Limits of the claim

The small fixture validates lifecycle and convergence, not reproduction of the production-scale
incident. Both baseline and fixed short runs had low, nonzero logical/WAL activity. Background
metadata/log/checkpoint activity is not synonymous with reference-fact rewrites. Constant database
or WAL sizes alone do not prove an absence of writes; process counters are not NAND measurements.

The measured SQL frontier is 1,000 facts. Larger 10,000/100,000-fact frontiers and transient
sort/materialization spills are not covered: zero persisted changes and WAL growth do not guarantee
zero temporary-file I/O. Incident-scale post-deployment profiling remains a follow-up. This change
also does not shrink an already enlarged store; it avoids unnecessary incremental rewrites
without destructive cleanup.
