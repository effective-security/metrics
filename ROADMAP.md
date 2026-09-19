# Roadmap

Open work only. Shipped behavior is documented in [README.md](README.md) and
[`Documentation/codemap.md`](Documentation/codemap.md), and open
defects in [FINDINGS.md](FINDINGS.md). Completed milestones live in git history.

Items here are larger than a bug fix: they change an API, a contract, or the
shape of the module. A defect that can be fixed in place belongs in FINDINGS.

## 1. Runtime metrics from `runtime/metrics`

`emitRuntimeStats` uses `runtime.ReadMemStats`, which stops the world every
`ProfileInterval`. The `runtime/metrics` package exposes the same counters
without that pause, and for six of the eight gauges the mapping is exact.

The blocker is the GC pause metrics, which have no equivalent with the same
shape: see [FINDINGS #14](FINDINGS.md#14) for the mapping table, the two
metrics that change, and the three ways out. Needs a compatibility decision
before implementation.

## 2. Pre-compiled prefix filters

`UpdateFilter` now installs an immutable snapshot, so the rules are read
without a lock, but matching is still a linear scan of both prefix lists per
emission. For a service with tens of rules that is measurable on the hot path.
Compile the snapshot into a trie (or sorted prefixes plus binary search) when
it is built, which is also where a longest-match rule would belong if allow and
block precedence ever needs to be per-prefix rather than block-wins.

## 3. Cardinality budget

Nothing enforces the "keep cardinality acceptable" rule the README states.
`Opts.CounterExpiration` lets an operator bound Prometheus counter growth, but
it is opt-in and blunt: it resets the series. CloudWatch charges per custom
metric, and `InmemSink` keeps every key for the retention window.

Add an optional per-sink series budget: a maximum number of distinct keys, a
counter for rejected series, and a log line naming the metric that blew the
budget. Emitting nothing is better than an unbounded bill.

## 4. CloudWatch publishing semantics

Three gaps remain after the snapshot and dimension fixes:

- Counters and samples publish cumulative values unless `WithCleanup` is set,
  so the default configuration double counts across intervals. Making delta
  semantics the default needs a migration note for anyone reading the
  cumulative shape.
- Everything is published as `Unit: Count` with `StorageResolution: 60`, so a
  `MeasureSince` in milliseconds is not labelled as a time unit and
  high-resolution metrics are not supported.
- Delivery is best effort: with `WithCleanup` a failed `PutMetricData` loses
  the interval, and `Run` gives up for good on a credentials error
  ([FINDINGS #15](FINDINGS.md#15), [#16](FINDINGS.md#16)). A bounded retry
  queue, and typed error inspection instead of string matching, belong to the
  same redesign.

Target: per-metric unit and resolution declared on `Describe`, delta semantics
by default, and a retry policy that is explicit rather than accidental.

## 5. Prometheus native histograms

Samples map to summaries with fixed objectives, computed per process and not
aggregatable across instances. Native histograms (client_golang 1.17+) are
aggregatable and cheaper. Add them as an opt-in per metric through `Describe`
or `Opts`, keeping summaries as the default until the deployment side is ready.

## 6. Sink registry in `factory`

`factory` only knows the `inmem` scheme, so configuration-driven setups cannot
select a real backend. Give the Prometheus and CloudWatch sinks URL
constructors (`prometheus://?expiration=60s`, `cloudwatch://<namespace>?region=`)
and an exported `Register(scheme, func)` so applications can add their own.

## 7. Benchmarks in the repository

CI runs the tests twice, once for coverage and once under `-race`, but there is
no committed benchmark: the numbers below were measured from a scratch module.
Commit benchmarks for `Prepare`, key flattening and each sink's emit path, so a
regression on the hot path shows up in review.

Reference numbers (11th gen i7, Go 1.27, after the current round of fixes):

| Path | ns/op | allocs/op |
| --- | --- | --- |
| global `IncrCounter` into `BlackholeSink` | ~84 | 3 |
| `InmemSink.IncrCounter` | ~240 | 2 |
| `prometheus.Sink.IncrCounter` | ~115 | 1 |

## 8. Dead and legacy API

`PointValue` is unused (`EmitKey` was removed deliberately), `New` returns an
error that is always nil, and `Describe.Type` is an untyped string with three
valid values plus one alias.

The embedded `Config` is also read on every emission — `Metrics.prepare` and
`MeasureSince` use its prefixes, tags and granularity — but only the filters
can be replaced safely, through `UpdateFilter`. Writing to any other field of
a live `Metrics` races with emission. Either extend the snapshot to the whole
config, or unexport it and expose the setters that are safe. Clean these up
together in one breaking release rather than one at a time.
