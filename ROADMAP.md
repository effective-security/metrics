# Roadmap

Open work only. Shipped behavior is documented in [README.md](README.md) and
[`Documentation/codemap.md`](Documentation/codemap.md), and open
defects in [FINDINGS.md](FINDINGS.md). Completed milestones live in git history.

Items here are larger than a bug fix: they change an API, a contract, or the
shape of the module. A defect that can be fixed in place belongs in FINDINGS.

## 1. Lifecycle for providers and sinks

`New` starts a runtime collector that never stops, `NewInmemSignal` and
`NewPushSink` start their own goroutines, and only some of them can be shut
down. Nothing in the module takes a `context.Context`.

Introduce one lifecycle contract: `Metrics.Close()`, or a context passed to
`New`, that stops the collector; a `Close()` on sinks that own goroutines; and
`Run(ctx)` as the single convention for background loops (CloudWatch already
uses it). Resolves [FINDINGS #12](FINDINGS.md#12), and makes
[#13](FINDINGS.md#13) fixable without a second constructor.

## 2. Immutable configuration snapshot

`Config` is embedded by value in `Metrics` and read on every emission, while
`UpdateFilter` writes to it. Replace the read path with an immutable snapshot
behind an `atomic.Pointer`, swapped on reconfiguration. Removes the races in
[FINDINGS #5](FINDINGS.md#5) and lets the filter rules be pre-compiled
(sorted prefixes, or a trie) instead of a linear scan per emission.

## 3. Runtime metrics from `runtime/metrics`

`emitRuntimeStats` uses `runtime.ReadMemStats`, which stops the world every
`ProfileInterval`. The `runtime/metrics` package exposes the same counters plus
GC pause histograms without a stop-the-world pause. Migrating changes which
metric names are emitted, so it needs a compatibility decision: keep the
current `runtime_*` names as a mapping layer, or emit the new set behind a
config flag. Resolves [FINDINGS #14](FINDINGS.md#14).

## 4. Cardinality budget

Nothing enforces the "keep cardinality acceptable" rule the README states.
Prometheus counters are retained forever ([FINDINGS #11](FINDINGS.md#11)),
CloudWatch charges per custom metric, and `InmemSink` keeps every key for the
retention window.

Add an optional per-sink series budget: a maximum number of distinct keys, a
counter for rejected series, and a log line naming the metric that blew the
budget. Emitting nothing is better than an unbounded bill.

## 5. CloudWatch publishing semantics

Three related gaps, worth one design pass:

- Counters and samples publish cumulative values unless `WithCleanup` is set,
  so the default configuration double counts across intervals.
- Everything is published as `Unit: Count` with `StorageResolution: 60`, so a
  `MeasureSince` in milliseconds is not labelled as a time unit and
  high-resolution metrics are not supported.
- The dimension limit is stale and enforced with a panic
  ([FINDINGS #8](FINDINGS.md#8)).

Target: per-metric unit and resolution on `Describe`, delta semantics by
default, and a documented migration for anyone reading the cumulative shape.

## 6. Prometheus native histograms

Samples map to summaries with fixed objectives, computed per process and not
aggregatable across instances. Native histograms (client_golang 1.17+) are
aggregatable and cheaper. Add them as an opt-in per metric through `Describe`
or `Opts`, keeping summaries as the default until the deployment side is ready.

## 7. Sink registry in `factory`

`factory` only knows the `inmem` scheme, so configuration-driven setups cannot
select a real backend. Give the Prometheus and CloudWatch sinks URL
constructors (`prometheus://?expiration=60s`, `cloudwatch://<namespace>?region=`)
and an exported `Register(scheme, func)` so applications can add their own.

## 8. Test and benchmark coverage of the hot path

There is no benchmark in the repository, and CI does not run `-race`
([FINDINGS #16](FINDINGS.md#16)). Add `-race` to CI, benchmarks for
`Prepare`, key flattening and each sink's emit path, and a concurrency test
that emits from several goroutines while reading `Data`/`DisplayMetrics`, so
the snapshot contract in [FINDINGS #2](FINDINGS.md#2) stays fixed once it is.

Reference numbers on the current code (11th gen i7, Go 1.27):

| Path | ns/op | allocs/op |
| --- | --- | --- |
| global `IncrCounter` into `BlackholeSink` | 76 | 3 |
| `InmemSink.IncrCounter` | 228 | 2 |
| `prometheus.Sink.IncrCounter` | 196 | 4 |

## 9. Dead and legacy API

`PointValue` is unused (`EmitKey` was removed deliberately), `New` and
`NewPushSink` return an error that is always nil, and `Describe.Type` is an
untyped string that only three values are valid for
([FINDINGS #10](FINDINGS.md#10)). Clean these up together in one breaking
release rather than one at a time.
