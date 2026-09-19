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
| Allow/block filtering | `start.go`, `metrics.go` | `Config.AllowMetric`, `Metrics.UpdateFilter`, `StringStartsWithOneOf` |
| Declare metrics with help text and tag names | `start.go` | `Describe`, `Describe.Tags`, `Describe.{SetGauge,IncrCounter,AddSample,MeasureSince}`, `Config.Help` |
| Metric type constants | `start.go` | `TypeCounter`, `TypeGauge`, `TypeSample` |
| Runtime (GC/goroutine/memory) metrics | `metrics.go` | `Metrics.collectStats`, `Metrics.emitRuntimeStats`, `maxGCPauses` |
| Sink and provider contracts | `sink.go` | `Sink`, `Provider`, `Tag` |
| Discard / duplicate emissions | `sink.go` | `BlackholeSink`, `FanoutSink`, `NewFanoutSink` |
| In-memory aggregation | `inmem.go` | `InmemSink`, `NewInmemSink`, `NewInmemSinkFromURL`, `InmemSink.Data` |
| Interval bucketing and key flattening | `inmem.go` | `IntervalMetrics`, `NewIntervalMetrics`, `InmemSink.getInterval`, `InmemSink.flattenKeyLabels`, `keyReplacer` |
| Sample statistics | `aggregate.go` | `AggregateSample`, `Ingest`, `Mean`, `Stddev`, `String` |
| JSON-shaped snapshot for a debug endpoint | `inmem_endpoint.go` | `InmemSink.DisplayMetrics`, `Summary`, `GaugeValue`, `SampledValue`, `formatSamples` |
| Dump metrics on a signal | `inmem_signal.go` | `InmemSignal`, `NewInmemSignal`, `DefaultInmemSignal`, `InmemSignal.Stop` |
| Per-OS default dump signal | `const_unix.go`, `const_windows.go` | `DefaultSignal` |
| Expose to Prometheus | `prometheus/prometheus.go` | `Sink`, `NewSink`, `NewSinkFrom`, `Opts`, `DefaultPrometheusOpts` |
| Pre-declared Prometheus series | `prometheus/prometheus.go` | `GaugeDefinition`, `SummaryDefinition`, `CounterDefinition`, `initGauges`, `initSummaries`, `initCounters` |
| Prometheus scrape and expiry | `prometheus/prometheus.go` | `Sink.Describe`, `Sink.Collect`, `Sink.collectAtTime`, `ObservationMaxAge` |
| Prometheus name sanitizing | `prometheus/prometheus.go` | `flattenKey`, `forbiddenCharsReplacer`, `prometheusLabels` |
| Push to a Pushgateway | `prometheus/prometheus.go` | `PushSink`, `NewPushSink`, `PushSink.Shutdown` |
| Publish to CloudWatch | `cloudwatch/cloudwatch.go` | `Sink`, `NewSink`, `Config`, `Sink.Run`, `Sink.Flush`, `Sink.Publish` |
| CloudWatch aggregation and expiry | `cloudwatch/cloudwatch.go` | `Sink.Data`, `Sink.flattenKey`, `dimensions` |
| CloudWatch client and credentials | `cloudwatch/cloudwatch.go` | `newPublisher`, `Publisher` |
| Build a sink from configuration | `factory/factory.go` | `NewMetricSinkFromURL`, `sinkRegistry` |

## Package `metrics` (root)

Import path: `github.com/effective-security/metrics`.

### Files

| File | Owns |
| --- | --- |
| `doc.go` | package documentation, key composition rules, usage sample |
| `start.go` | `Config`, `Metrics`, construction, global instance, `Prepare`/`AllowMetric`, `Describe`, `Help` |
| `metrics.go` | the four emit methods, `UpdateFilter`, runtime stats collector, `StringStartsWithOneOf` |
| `sink.go` | `Tag`, `Sink`, `Provider`, `BlackholeSink`, `FanoutSink` |
| `inmem.go` | `InmemSink` aggregation, interval lifecycle, key flattening |
| `aggregate.go` | `AggregateSample` statistics |
| `inmem_endpoint.go` | display/JSON shapes derived from `InmemSink` |
| `inmem_signal.go` | signal handler that dumps `InmemSink` to a writer |
| `const_unix.go` / `const_windows.go` | `DefaultSignal` (`SIGUSR1`; `syscall.Signal(21)`/SIGBREAK on Windows) |

### Entry points

- `New(conf *Config, sink Sink) (*Metrics, error)` — builds an instance. The
  config is copied. The returned error is always nil.
- `NewGlobal(conf, sink)` — same, and installs the instance as the global one.
- `Metrics` implements `Provider`; the package-level functions forward to the
  global instance.
- `Describe` — declare a metric once, emit it by value: it validates the tag
  count on each call and feeds `Config.Help`.

### Invariants

- **Key composition order** (`Config.Prepare`):
  `<GlobalPrefix>_<ServiceName>_<type>_<HostName>_<key>`. `EnableHostnameLabel`
  and `EnableServiceLabel` turn those parts into `host` / `service` tags instead
  of prefixes, and take precedence over the corresponding prefix flag.
- **Filtering happens before the sink.** `Prepare` returns `allowed=false` and
  the emit method returns without touching the sink. A blocked prefix always
  wins; everything else currently resolves to `FilterDefault`
  ([FINDINGS #1](../FINDINGS.md#1)).
- **`Prepare` owns the tags it is given.** It appends to the caller's slice and
  rewrites values in place for `NumberLabelPrefix` ([FINDINGS #7](../FINDINGS.md#7)).
  Never pass a slice you intend to reuse; `Describe.Tags` allocates a fresh one.
- **`NumberLabelPrefix`** applies to tag values of at least `maxNumberlen` (15)
  digits, optionally signed, so large IDs are not coerced to floats downstream.
- **Global state**: `globalMetrics` is an `atomic.Pointer[Metrics]`, initialized
  in `init` with a `BlackholeSink`, so emitting before `NewGlobal` is safe and
  silent. `NewGlobal` replaces the pointer and does not stop the previous
  instance.
- **Goroutines**: `New` starts `collectStats` when `EnableRuntimeMetrics` is
  set. It runs until the process exits ([FINDINGS #12](../FINDINGS.md#12)).
  `NewInmemSignal` starts a goroutine that `Stop` ends.
- **Reconfiguration is not synchronized.** `UpdateFilter` writes the filter
  slices that emissions read ([FINDINGS #5](../FINDINGS.md#5)).
- **`InmemSink` interval lifecycle**: values land in the bucket for
  `time.Now().Truncate(interval)`; `Data` forces the current bucket to exist and
  returns oldest first, with the current (still aggregating) bucket last.
  `maxIntervals` is `retain / interval` and must be at least 2
  ([FINDINGS #3](../FINDINGS.md#3), [#4](../FINDINGS.md#4)).
- **`InmemSink` locking**: `intervalLock` guards the interval slice, each
  `IntervalMetrics` guards its own maps. `Data` copies the maps of the current
  interval but not the `*AggregateSample` values inside
  ([FINDINGS #2](../FINDINGS.md#2)).
- **`AggregateSample` is not self-synchronized**; callers hold the interval
  lock. `Stddev` returns 0 for fewer than two values and is computed from
  running sums.
- **Aggregation key** (`flattenKeyLabels`): `name;tag=value;tag=value`, with
  spaces replaced by `_`. The returned second value is the unmodified name.
  Package-level `keyReplacer` — do not build a `strings.NewReplacer` per call.
- **`PointValue` is dead code**; `EmitKey` is deliberately absent because
  Prometheus has no matching type.
- **Panics are part of the current contract** in `NewInmemSink` (zero interval)
  and `InmemSink.Data` (retention shorter than one interval). Both are tracked
  defects, not intended API.

## Package `prometheus`

Import path: `github.com/effective-security/metrics/prometheus`.
Files: `doc.go`, `prometheus.go`. Depends on the root package and
`github.com/prometheus/client_golang`.

### Entry points

- `NewSinkFrom(opts Opts) (*Sink, error)` — the main constructor; registers the
  sink with `opts.Registerer` (default `prometheus.DefaultRegisterer`) and
  returns the registration error.
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
- **Expiry**: gauges and summaries created at runtime (`canDelete`) are dropped
  during `Collect` after `Opts.Expiration` without an update. Pre-declared
  series never expire. **Counters are never deleted**, by design, so their tag
  cardinality is retained for the process lifetime
  ([FINDINGS #11](../FINDINGS.md#11)).
- **Update pattern**: the `sync.Map` values are treated as immutable. Each emit
  copies the struct, mutates the copy and stores a new pointer; the embedded
  Prometheus metric is shared and thread-safe. This costs ~4 allocations per
  emit ([FINDINGS #15](../FINDINGS.md#15)).
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
- **Type mapping**: gauge → `Value` (last write wins), counter → `Value`
  (accumulated), sample → `StatisticValues` (min/max/sum/count). All data is
  published with `Unit = Count` and `StorageResolution = 60`.
- **Retention**: `WithCleanup` deletes what `Data` returned, so each publish
  covers one interval. Without it, values keep accumulating and are
  republished until `MetricsExpiry` passes without an update.
  `WithSampleCount` adds `_count`, `_sum` and `_avg` datums per sample.
- **`Data` does not deep-copy**: the returned datums share `StatisticValues`
  and `Dimensions` with the live sink, and publishing happens outside `mu`
  ([FINDINGS #6](../FINDINGS.md#6)).
- **Batching**: `Flush` splits into chunks of `maxMetricsPerRequest` (1000).
- **`dimensions` panics** above `maxDimensions` (10) tags
  ([FINDINGS #8](../FINDINGS.md#8)).
- **Credentials**: the standard AWS chain, overridden by
  `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN` when both id
  and secret are set. Region falls back to `AWS_REGION`, then
  `AWS_DEFAULT_REGION`. `AwsEndpoint` maps to `config.WithBaseEndpoint`.
- `Run` stops without retrying when the error text contains `expired` or
  `NoCredentialProviders`, and its shutdown flush reuses the cancelled context
  ([FINDINGS #9](../FINDINGS.md#9)).

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
| `cloudwatch/cloudwatch_test.go` | `package cloudwatch_test`: config validation and a `mockPublisher` capturing `PutMetricData`. |
| `factory/factory_test.go` | `package factory_test`: URL parsing and error strings. |

Conventions: black-box `package foo_test` unless the test needs unexported
symbols; `assert`/`require` from testify; table tests for conversion helpers.
There is no testdata directory and no generated mocks.

Timing: several tests sleep to cross an interval boundary. Tests that install a
global provider or mutate `DefaultPrometheusOpts` affect the rest of the
package and must not be run in parallel.

## Build and CI

- `make test` / `make covtest` / `make lint` / `make all` — the targets come
  from `.project/gomod-project.mk`.
- Lint: `.golangci.yml` (v2 config) with `revive`, `errcheck`, `misspell`,
  `copyloopvar`, `asasalint`, `bidichk`, plus `goimports`/`gofmt` formatters.
  Tests are excluded from linting.
- CI: `.github/workflows/unittest.yml`, Go version from `go.mod` (1.27),
  coverage gate `MIN_TESTCOV=92` (currently ~93.8%). Tests do **not** run with
  `-race`, which hides [FINDINGS #5](../FINDINGS.md#5).
