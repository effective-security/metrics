# Code map

Navigation index for agents and new contributors: concept → file → entry
points → invariants. Start here instead of grepping the tree. If you had to
grep for something that belongs in this map, add the row in the same change
(see [AGENTS.md](../AGENTS.md)).

- High level purpose and samples: [README.md](../README.md)
- Known defects, with IDs referenced from code comments: [FINDINGS.md](../FINDINGS.md)
- Larger planned work: [ROADMAP.md](../ROADMAP.md)

## Module layout

```
github.com/effective-security/metrics      instrumentation API, filters, in-memory sink
├── prometheus/                             Prometheus collector sink + Pushgateway sink
├── cloudwatch/                             AWS CloudWatch sink
└── factory/                                sink construction from a URL
```

There is no service, `cmd/`, generated mock tree, or build artifact: this
module is a library. Subpackages import the root package; the root package
imports none of them.

## Concept index

| Concept | File | Entry points |
| --- | --- | --- |
| Emit a metric from application code | `metrics.go` | `Metrics.SetGauge`, `Metrics.IncrCounter`, `Metrics.AddSample`, `Metrics.MeasureSince` |
| Emit without holding an instance | `start.go` | package-level `SetGauge`, `IncrCounter`, `AddSample`, `MeasureSince`, `UpdateFilter` |
| Configure the provider | `start.go` | `Config`, `DefaultConfig`, `New`, `NewGlobal` |
| Final metric name and label assembly | `start.go` | `Config.Prepare`, `maxNumberlen`, `isBigNumber` |
| Allow/block filtering | `start.go`, `metrics.go` | `allowMetric`, `Config.AllowMetric`, `Metrics.AllowMetric`, `Metrics.UpdateFilter`, `metricFilters`, `StringStartsWithOneOf` |
| Provider lifecycle | `metrics.go` | `Metrics.Close`, `Metrics.collectStats` |
| Declare metrics with help text and tag names | `start.go`, `metrics.go` | `Describe`, `Describe.Tags`, `Describe.emitType`, `Describe.{SetGauge,IncrCounter,AddSample,MeasureSince}`, `Config.Help`, `Metrics.Help`, `describeHelp` |
| Metric type constants | `start.go` | `TypeCounter`, `TypeGauge`, `TypeSample` |
| Runtime (GC/goroutine/memory) metrics | `metrics.go` | `Metrics.collectStats`, `Metrics.emitRuntimeStats`, `maxGCPauses` |
| Sink and provider contracts | `sink.go` | `Sink`, `Provider`, `Tag` |
| Discard / duplicate emissions | `sink.go` | `BlackholeSink`, `FanoutSink`, `NewFanoutSink` |
| In-memory aggregation | `inmem.go` | `InmemSink`, `NewInmemSink`, `NewInmemSinkFromURL`, `InmemSink.Data`, `IntervalMetrics.clone`, `cloneSampledValues` |
| Interval bucketing and key flattening | `inmem.go` | `IntervalMetrics`, `NewIntervalMetrics`, `InmemSink.getInterval`, `InmemSink.flattenKeyLabels`, `keyReplacer` |
| Sample statistics | `aggregate.go` | `AggregateSample`, `Ingest`, `Mean`, `Stddev`, `String` |
| JSON-shaped snapshot for a debug endpoint | `inmem_endpoint.go` | `InmemSink.DisplayMetrics`, `Summary`, `GaugeValue`, `SampledValue`, `formatSamples` |
| Dump metrics on a signal | `inmem_signal.go` | `InmemSignal`, `NewInmemSignal`, `DefaultInmemSignal`, `InmemSignal.Stop` |
| Per-OS default dump signal | `const_unix.go`, `const_windows.go` | `DefaultSignal` |
| Expose to Prometheus | `prometheus/prometheus.go` | `Sink`, `NewSink`, `NewSinkFrom`, `Opts`, `DefaultPrometheusOpts` |
| Pre-declared Prometheus series | `prometheus/prometheus.go` | `GaugeDefinition`, `SummaryDefinition`, `CounterDefinition`, `initGauges`, `initSummaries`, `initCounters` |
| Prometheus scrape and expiry | `prometheus/prometheus.go` | `Sink.Describe`, `Sink.Collect`, `Sink.collectAtTime`, `retire`, `lastUpdate`, `ObservationMaxAge` |
| Prometheus name sanitizing and validation | `prometheus/prometheus.go` | `flattenKey`, `forbiddenCharsReplacer`, `validSeries`, `reservedLabelPrefix`, `prometheusLabels` |
| Push to a Pushgateway | `prometheus/prometheus.go` | `PushSink`, `NewPushSink`, `NewPushSinkFrom`, `PushSink.push`, `PushSink.Shutdown` |
| Publish to CloudWatch | `cloudwatch/cloudwatch.go` | `Sink`, `NewSink`, `Config`, `Sink.Run`, `Sink.Flush`, `Sink.Publish` |
| CloudWatch aggregation and expiry | `cloudwatch/cloudwatch.go` | `Sink.Data`, `cloneDatum`, `derivedDatum`, `Sink.flattenKey`, `limitTags`, `dimensions` |
| CloudWatch client and credentials | `cloudwatch/cloudwatch.go` | `newPublisher`, `Publisher` |
| Build a sink from configuration | `factory/factory.go` | `NewMetricSinkFromURL`, `sinkRegistry` |

## Package `metrics` (root)

Import path: `github.com/effective-security/metrics`.

### Files

| File | Owns |
| --- | --- |
| `doc.go` | package documentation, key composition rules, usage sample |
| `start.go` | `Config`, `Metrics`, construction, global instance, `Prepare`/`AllowMetric`, filter snapshot, `Describe`, `Help` |
| `metrics.go` | the four emit methods, `Metrics.Prepare`/`AllowMetric`/`Help`/`UpdateFilter`/`Close`, runtime stats collector, `StringStartsWithOneOf` |
| `sink.go` | `Tag`, `Sink`, `Provider`, `BlackholeSink`, `FanoutSink` |
| `inmem.go` | `InmemSink` aggregation, interval lifecycle, key flattening |
| `aggregate.go` | `AggregateSample` statistics |
| `inmem_endpoint.go` | display/JSON shapes derived from `InmemSink` |
| `inmem_signal.go` | signal handler that dumps `InmemSink` to a writer |
| `const_unix.go` / `const_windows.go` | `DefaultSignal` (`SIGUSR1`; `syscall.Signal(21)`/SIGBREAK on Windows) |

### Entry points

- `New(conf *Config, sink Sink) (*Metrics, error)` — builds an instance. The
  config is copied, including its tag and prefix slices, and non-positive
  durations fall back to their defaults. The returned error is always nil.
- `NewGlobal(conf, sink)` — same, and installs the instance as the global one.
- `Metrics.Close()` — stops the runtime collector; idempotent.
- `Metrics` implements `Provider`; the package-level functions forward to the
  global instance.
- `Describe` — declare a metric once, emit it by value: it validates the tag
  count on each call and feeds `Config.Help`. `Describe.emitType` maps the
  declared type onto the one emission uses (`"summary"` is an alias of
  `TypeSample`), so a help key matches the metric name it describes.
  `Config.Help` and `Metrics.Help` share `describeHelp`; only the latter sees
  the rules installed by `UpdateFilter`.

### Invariants

- **Key composition order** (`Config.Prepare`):
  `<GlobalPrefix>_<ServiceName>_<type>_<HostName>_<key>`. `EnableHostnameLabel`
  and `EnableServiceLabel` turn those parts into `host` / `service` tags instead
  of prefixes, and take precedence over the corresponding prefix flag.
- **Filtering happens before the sink.** `Prepare` returns `allowed=false` and
  the emit method returns without touching the sink. Precedence, implemented
  once in `allowMetric`: a blocked prefix wins, then an allowed prefix admits
  the key, then `FilterDefault` decides.
- **`Prepare` never modifies the caller's tags.** When tags must be added
  (`GlobalTags`, `host`, `service`) or rewritten (`NumberLabelPrefix`) it
  returns a new slice; `prefixNumbers` clones before the first rewrite when the
  slice is still the caller's.
- **`NumberLabelPrefix`** applies to tag values of at least `maxNumberlen` (15)
  digits, not counting an optional leading `-`, so large IDs are not coerced to
  floats downstream.
- **Global state**: `globalMetrics` is an `atomic.Pointer[Metrics]`, initialized
  in `init` with a `BlackholeSink`, so emitting before `NewGlobal` is safe and
  silent. `NewGlobal` replaces the pointer and does not stop the previous
  instance.
- **Goroutines**: `New` starts `collectStats` when `EnableRuntimeMetrics` is
  set, on a ticker, until `Metrics.Close` (idempotent, `sync.Once`). The
  collection itself still calls `runtime.ReadMemStats`, which stops the world
  ([FINDINGS #14](../FINDINGS.md#14)).
  `NewInmemSignal` starts a goroutine that `Stop` ends.
- **Two filter paths, deliberately.** `Config.AllowMetric` reads the struct
  fields; `Metrics` keeps an immutable `metricFilters` snapshot in an
  `atomic.Pointer`, replaced as a whole by `UpdateFilter`, and shadows
  `Prepare`/`AllowMetric`/`Help` so emission and introspection agree.
  `UpdateFilter` does not write back to the embedded `Config`, which keeps the
  construction-time rules.
- **`InmemSink` interval lifecycle**: values land in the bucket for
  `time.Now().Truncate(interval)`; `Data` forces the current bucket to exist and
  returns oldest first, with the current (still aggregating) bucket last.
  `maxIntervals` is `retain / interval`; the constructor clamps `interval` to
  `defaultInmemInterval` and `retain` to `minRetainIntervals * interval`, so it
  is always at least 2. `createInterval` evicts the oldest buckets and clears
  the vacated tail of the slice, so an evicted interval is collectable.
  `NewInmemSinkFromURL` returns an error instead of clamping.
- **`InmemSink` locking**: `intervalLock` guards the interval slice, each
  `IntervalMetrics` guards its own maps. `Data` returns a snapshot: every
  interval is cloned under its own `RLock` (`IntervalMetrics.clone`), including
  the `*AggregateSample` behind each value (`cloneSampledValues`). A finished
  interval is copied too, because an emitter can resolve its interval, be
  descheduled across the rollover, and write to it afterwards. `DisplayMetrics`
  and `InmemSignal.dumpStats` therefore need no lock of their own. The `Labels`
  slices are shared and read-only.
- **`AggregateSample` is not self-synchronized**; callers hold the interval
  lock. `Stddev` returns 0 for fewer than two values and is computed from
  running sums.
- **Aggregation key** (`flattenKeyLabels`): `name;tag=value;tag=value`, with
  spaces replaced by `_`. The returned second value is the unmodified name.
  Package-level `keyReplacer` — do not build a `strings.NewReplacer` per call.
- **`PointValue` is dead code**; `EmitKey` is deliberately absent because
  Prometheus has no matching type.
- **No library call panics.** A misconfiguration is clamped (`NewInmemSink`;
  `New` and `cloudwatch.NewSink` for non-positive durations, which would
  otherwise reach `time.NewTicker`), rejected with an error
  (`NewInmemSinkFromURL`, `NewPushSinkFrom`), or reported and truncated
  (`cloudwatch.limitTags`). A non-positive expiration means "never expire"
  rather than "expire everything".

## Package `prometheus`

Import path: `github.com/effective-security/metrics/prometheus`.
Files: `doc.go`, `prometheus.go`. Depends on the root package and
`github.com/prometheus/client_golang`.

### Entry points

- `NewSinkFrom(opts Opts) (*Sink, error)` — the main constructor; registers the
  sink with `opts.Registerer` (default `prometheus.DefaultRegisterer`). On a
  registration error it returns `nil` and the wrapped error, with the cause
  preserved for `errors.As` (`prometheus.AlreadyRegisteredError`).
- `NewSink()` — `NewSinkFrom(DefaultPrometheusOpts)`.
- `NewPushSink(address, pushInterval, name)` — Pushgateway variant; starts the
  push loop immediately, `Shutdown` stops it.

### Invariants

- `Sink` **is** a `prometheus.Collector`. `Describe` emits one dummy descriptor
  named after `Opts.Name`, so two sinks on the same registerer need different
  names, and the sink is a "checked" collector: a pedantic registry would
  reject the runtime-created series.
- **Type mapping**: gauge → `Gauge`, counter → `Counter`, sample → `Summary`
  with objectives 0.5/0.9/0.99 and `MaxAge = ObservationMaxAge` (10m,
  independent of `Opts.Expiration`).
- **Series identity** is the `flattenKey` hash (`name;tag=value;…`). The
  metric name is sanitized (` `, `.`, `=`, `-`, `/` → `_`); tag names are not,
  and are escaped by the client library at exposition time.
- **A series the client library rejects is dropped, not registered.**
  `validSeries` applies the `prometheus.NewDesc` rules (UTF-8 metric and label
  names, no `__` label prefix, UTF-8 label values) when a series is about to be
  created, logs the rejection and returns. A metric with an erroring descriptor
  would otherwise fail every `Gather` — HTTP 500 on `/metrics` — until it
  expires. Existing entries were validated when they were created.
- **Expiry**: gauges and summaries created at runtime (`canDelete`) are dropped
  during `Collect` after `Opts.Expiration` without an update. Pre-declared
  series never expire. Counters are kept unless `Opts.CounterExpiration` is
  set, because deleting one resets the series and breaks `rate()` over the gap.
- **Update pattern**: the `sync.Map` entry is created once and then mutated in
  place. The embedded Prometheus metric is thread-safe and `lastUpdate` holds
  an `atomic.Int64`, so an emission is one map load plus one CAS — no struct
  copy and no `Store`. The metric structs must not be copied (`go vet`
  enforces it).
- **Expiry and emission are coordinated by a tombstone, and the order matters.**
  An emit method updates the metric **first** and then commits with
  `lastUpdate.touch`; a new series is filled in before `LoadOrStore` publishes
  it. So a value is never applied to a series the collector has already decided
  to drop without the emission finding out: `touch` fails on a tombstoned entry,
  the emit method removes it (`CompareAndDelete`) and applies the value again to
  a fresh series.
- **The emit loops are unbounded and lock-free.** A turn that does not return
  was caused by another goroutine completing its step (the collector retired
  the entry, or a concurrent emission published the series first), and the
  emission removes a tombstoned entry itself before retrying, so every turn
  makes progress on the map. There is no attempt limit: one would have to drop
  the value silently when reached.
- **`retire` is conditional twice over**: it tombstones only if the entry has
  not been updated since `idleSince` read it (CAS on the timestamp), and deletes
  only if it is still the same entry (`CompareAndDelete`). Every successful
  `touch` changes the stored timestamp, so a retire decided before it always
  fails — while the timestamp still only moves forward, so a slow emission
  cannot make a fresh series look idle. An entry that is already tombstoned
  (a concurrent scrape got there first) is skipped without being collected.
  `idleSince` subtracts timestamps rather than adding the expiration to one,
  so an expiration near `math.MaxInt64` cannot overflow into "everything is
  idle". What remains is the intended behavior: a series retired on the
  timestamp of the emission that filled it has been idle for a full expiration
  since, so it expired before anything scraped it.
- **`PushSink.Shutdown` waits for its loop.** It closes `stopChan`, waits on
  `doneChan`, and only then pushes a last time: two concurrent `Push` calls
  would share one `push.Pusher`. Both the loop and the final push go through
  `PushSink.push`, which logs a failure; there is no caller to return it to.
- `Opts.Help` is retained by the sink and written to by the pre-declared
  definitions: the caller's map is modified.
- `DefaultPrometheusOpts` is package-level mutable state read only at
  construction.

## Package `cloudwatch`

Import path: `github.com/effective-security/metrics/cloudwatch`.
Files: `doc.go`, `cloudwatch.go`. Depends on the root package,
`aws-sdk-go-v2` and `effective-security/x/values`.

### Entry points

- `NewSink(c *Config) (*Sink, error)` — validates `Namespace` and the region,
  builds the AWS client. Does not publish.
- `Sink.Run(ctx)` — publish loop; returns after a final flush when `ctx` ends.
- `Sink.Flush(ctx)` / `Sink.Publish(ctx, data)` — on-demand publishing.
- `Sink.Publisher` is an exported embedded field so a test or wrapper can
  substitute the client.

### Invariants

- **Aggregation, not per-emit publishing**: emissions accumulate in the
  `gauges`, `samples` and `counters` maps under `mu`, keyed by the
  `name;tag=value` hash, and are converted to `types.MetricDatum` by `Data`.
- **`limitTags` runs before the key is built.** The aggregation key and the
  published dimensions must come from the same tags, or two emissions differing
  only in a dropped tag would occupy two map entries and fight over one
  CloudWatch series.
- **Type mapping**: gauge → `Value` (last write wins), counter → `Value`
  (accumulated), sample → `StatisticValues` (min/max/sum/count). All data is
  published with `Unit = Count` and `StorageResolution = 60`. Updates write
  through the datum's pointers in place, under `mu`; every statistic has its
  own pointer, so no field aliases another.
- **Retention**: `WithCleanup` deletes what `Data` returned, so each publish
  covers one interval. Without it, values keep accumulating and are
  republished until `MetricsExpiry` passes without an update.
  `WithSampleCount` adds `_count`, `_sum` and `_avg` datums per sample.
- **`Data` returns a deep copy**: `cloneDatum` duplicates every pointer field,
  including the statistic set, the dimension strings and the name, so the batch
  stays stable while `Publish` serializes it outside `mu` and a caller may
  modify what it gets back. The `WithSampleCount` datums come from
  `derivedDatum`, which clones again, so the four datums of one sample share
  nothing with each other either.
- **`Publish` logs the batch size, never the batch**: a failed request can
  carry `maxMetricsPerRequest` datums.
- **Batching**: `Flush` splits into chunks of `maxMetricsPerRequest` (1000).
- **`limitTags` truncates** above `maxDimensions` (30, the AWS limit) and logs
  the dropped tag names; `dimensions` assumes it already ran. Neither panics:
  losing a dimension beats failing the batch or killing the emitting
  goroutine.
- **Non-positive durations fall back to the defaults** in `NewSink`, because
  `PublishInterval` reaches `time.NewTicker` and a negative `MetricsExpiry`
  would drop every datum on the next flush.
- **Credentials**: the standard AWS chain, overridden by
  `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN` when both id
  and secret are set. Region falls back to `AWS_REGION`, then
  `AWS_DEFAULT_REGION`. `AwsEndpoint` maps to `config.WithBaseEndpoint`.
- `Run` stops without retrying when the error text contains `expired` or
  `NoCredentialProviders` ([FINDINGS #16](../FINDINGS.md#16)). Its shutdown
  flush runs on `context.WithoutCancel(ctx)` plus `shutdownFlushTimeout`, so
  the last interval is published instead of failing on the context that just
  ended. With `WithCleanup`, a batch that fails to publish is lost
  ([FINDINGS #15](../FINDINGS.md#15)).

## Package `factory`

Import path: `github.com/effective-security/metrics/factory`.
Files: `doc.go`, `factory.go`.

- `NewMetricSinkFromURL(urlStr)` resolves `sinkRegistry` by URL scheme.
- Only `inmem` is registered, mapping to `metrics.NewInmemSinkFromURL`, which
  requires the `interval` and `retain` duration parameters.
- Adding a scheme means adding a `func(*url.URL) (metrics.Sink, error)` to
  `sinkRegistry` and a row here. Prometheus and CloudWatch are intentionally
  absent: their options do not map onto a URL.

## Internal dependencies

```
factory ──▶ metrics
prometheus ──▶ metrics
cloudwatch ──▶ metrics
metrics ──▶ (cockroachdb/errors, effective-security/xlog)
```

The root package must not import a sink subpackage; that would create an import
cycle with the `Sink` interface. Errors are created and wrapped with
`github.com/cockroachdb/errors`, logging goes through
`xlog.NewPackageLogger("github.com/effective-security/metrics", "<pkg>")`.

## Test layout

| File | Scope |
| --- | --- |
| `metrics_test.go` | `package metrics_test`: config combinations, filters, fanout, global instance, `Describe`, `Help`. Defines the shared `run(Provider, times)` helper and the testify `mockedSink`. |
| `inmem_test.go` | `package metrics_test`: `InmemSink` end to end, `DisplayMetrics`, signal handling (sends a real `DefaultSignal` to the test process). |
| `prometheus/prometheus_test.go` | `package prometheus_test`: scrape through `promhttp`, expiry, unregistration. Mutates `DefaultPrometheusOpts`. |
| `prometheus/internal_test.go` | `package prometheus`: white-box expiry (`collectAtTime`), pre-declared definitions, `flattenKey` table test, `NewPushSink` against a fake Pushgateway. Installs a global provider. |
| `cloudwatch/cloudwatch_test.go` | `package cloudwatch_test`: config validation, flush and `WithCleanup` accounting, expiry, the `Run` ticker and its credentials bail-out. `TestMain` clears AWS credentials from the environment. |
| `factory/factory_test.go` | `package factory_test`: URL parsing and error strings. |
| `metrics_extra_test.go` | `package metrics_test`: filter precedence table, tag-slice ownership, `UpdateFilter` under concurrent emission, `Close`, interval clamping and eviction, snapshot isolation, `Describe` type aliases, `Metrics.Help`. |
| `cloudwatch/cloudwatch_extra_test.go` | `package cloudwatch_test`: snapshot isolation, dimension truncation, the shutdown flush. Defines the shared `syncPublisher` (mutex-guarded fake client) and `newTestSink`. |
| `prometheus/internal_extra_test.go` | `package prometheus`: counter expiry through `collectAtTime`, in-place updates, the tombstone protocol (`touch`/`retire`/`idleSince`, mid-flight retirement, overflow), `validSeries` table, `PushSink.Shutdown` against a fake gateway. |
| `prometheus/prometheus_extra_test.go` | `package prometheus_test`: `PushSink` validation and idempotent `Shutdown`, concurrent emission, an invalid series not failing `Gather`, duplicate sink names. |

Conventions: black-box `package foo_test` unless the test needs unexported
symbols; `assert`/`require` from testify; table tests for conversion helpers.
There is no testdata directory and no generated mocks.

Timing: several tests sleep to cross an interval boundary. Tests that install a
global provider or mutate `DefaultPrometheusOpts` affect the rest of the
package and must not be run in parallel. No test lets `cloudwatch.Sink.Run`
publish concurrently with an explicit `Flush` into an unsynchronized fake:
the `RaceTest` CI step would fail on it.

## Build and CI

- `make test` / `make covtest` / `make lint` / `make all` — the targets come
  from `.project/gomod-project.mk`.
- Lint: `.golangci.yml` (v2 config) with `revive`, `errcheck`, `misspell`,
  `copyloopvar`, `asasalint`, `bidichk`, plus `goimports`/`gofmt` formatters.
  Tests are excluded from linting.
- CI: `.github/workflows/unittest.yml`, Go version from `go.mod` (1.27),
  coverage gate `MIN_TESTCOV=92` (currently ~97%). The `RaceTest` step runs
  `make test RACE=true`, so a data race fails the build.
