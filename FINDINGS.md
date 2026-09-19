# FINDINGS

Active, actionable bugs, security issues, and correctness problems.
Completed work and deprecation decisions are omitted.

Use the **ID** when commenting or assigning work. Update **Status** in the
same change as the code or decision, and remove the item when it is complete.

## Status

| Status         | Meaning                                                |
| -------------- | ------------------------------------------------------ |
| Open           | Not started                                            |
| In Progress    | Being fixed                                            |
| Needs Approval | Behavior or compatibility change that needs a decision |

Severity: **security** > **bug** > **race** > **correctness** > **performance** > **docs**.

## Index

| ID  | Package | Title | Severity | Status |
| --- | ------- | ----- | -------- | ------ |
| [1](#1)  | metrics | `AllowedPrefixes` does not restrict emission | bug | Needs Approval |
| [2](#2)  | metrics | `InmemSink` snapshots share live samples; `DisplayMetrics` reads unguarded | race | Open |
| [3](#3)  | metrics | `NewInmemSink` panics when `interval` is zero | bug | Open |
| [4](#4)  | metrics | `InmemSink.Data` panics when `retain` is shorter than `interval` | bug | Open |
| [5](#5)  | metrics | `UpdateFilter` races with the runtime collector | race | Open |
| [6](#6)  | cloudwatch | `Data` shares statistic sets with the live sink | race | Open |
| [7](#7)  | metrics | `Prepare` mutates the caller's tag slice | bug | Open |
| [8](#8)  | cloudwatch | `dimensions` panics above 10 tags; AWS allows 30 | bug | Open |
| [9](#9)  | cloudwatch | Shutdown flush uses the cancelled context and always fails | bug | Open |
| [10](#10) | metrics | `Config.Help` keys can differ from the emitted metric names | correctness | Open |
| [11](#11) | prometheus | Counters are never expired: unbounded memory | correctness | Needs Approval |
| [12](#12) | metrics | The runtime collector goroutine cannot be stopped | bug | Open |
| [13](#13) | prometheus | `NewPushSink` ignores `Opts`, never registers, panics on a non-positive interval | bug | Open |
| [14](#14) | metrics | `ReadMemStats` stops the world every `ProfileInterval` | performance | Open |
| [15](#15) | prometheus | Every emission copies a struct and stores into `sync.Map` | performance | Open |
| [16](#16) | repo | CI does not run `-race`, and the coverage gate disagrees with AGENTS.md | docs | Open |

---

<a id="1"></a>

## 1. `AllowedPrefixes` does not restrict emission

**Package** metrics · **Severity** bug · **Status** Needs Approval ·
`start.go:255` (`Config.AllowMetric`)

The allow-list branch returns `true` when the key does **not** match, so the
verdict is inverted:

```go
if len(m.AllowedPrefixes) > 0 {
    if !StringStartsWithOneOf(key, m.AllowedPrefixes) {
        return true   // should be: return false
    }
}
return m.FilterDefault
```

**Evidence**

| Config | `AllowMetric("allowed_x")` | `AllowMetric("other")` | Expected |
| --- | --- | --- | --- |
| `FilterDefault: true`, allow `allowed_` | true | **true** | true / false |
| `FilterDefault: false`, allow `allowed_` | **false** | **true** | true / false |

With `FilterDefault: false` the allow-list is exactly backwards: the listed
prefixes are the only ones dropped. The repository tests do not catch it
because every emitted key in them starts with the allowed prefix.

**Impact** An allow-list configured to cut cardinality or cost has no effect,
or the opposite effect. For a CloudWatch deployment that is a billing issue.

**Fix** `return false` in that branch, and let a matching key fall through to
`FilterDefault`. This changes emission for anyone who currently sets
`AllowedPrefixes`: metrics that are emitted today will start being dropped.
Needs a decision, a minor version bump and a table test over the four
allow/block/default combinations.

---

<a id="2"></a>

## 2. `InmemSink` snapshots share live samples; `DisplayMetrics` reads unguarded

**Package** metrics · **Severity** race · **Status** Open ·
`inmem.go:170` (`Data`), `inmem_endpoint.go:85` (`DisplayMetrics`)

Two separate defects on the same read path:

1. `Data` copies the maps of the current interval, but `SampledValue` embeds
   `*AggregateSample`. The copy therefore shares the counters and samples with
   the interval that is still being written to.
2. `DisplayMetrics` computes `data := i.Data()` and then ignores it, indexing
   `i.intervals` instead — without holding `intervalLock`.

**Evidence** Emitting from one goroutine while another calls `DisplayMetrics`
is reported by `-race`:

```
WARNING: DATA RACE
Write at 0x… by goroutine 12:
  metrics.(*InmemSink).createInterval()   inmem.go:227
  metrics.(*InmemSink).getInterval()      inmem.go:244
  metrics.(*InmemSink).IncrCounter()      inmem.go:125
Previous read at 0x… by goroutine 11:
  metrics.(*InmemSink).DisplayMetrics()   inmem_endpoint.go:88
```

The second defect is the same shape one level down: `formatSamples`
(`inmem_endpoint.go:132`) calls `Mean` and `Stddev` on the shared
`*AggregateSample` while `Ingest` (`aggregate.go:50`) writes to it from
`InmemSink.IncrCounter` (`inmem.go:139`).

**Impact** Torn statistics on a debug endpoint at best; `DisplayMetrics` is the
function a service exposes over HTTP, so the race is reachable from a request.

**Fix** Deep-copy `AggregateSample` into the snapshot (copy the struct, not the
pointer), and make `DisplayMetrics` select from `data`, the snapshot it already
took, instead of `i.intervals`.

---

<a id="3"></a>

## 3. `NewInmemSink` panics when `interval` is zero

**Package** metrics · **Severity** bug · **Status** Open · `inmem.go:105`

`maxIntervals: int(retain / interval)` divides by the interval:
`metrics.NewInmemSink(0, time.Minute)` panics with
`runtime error: integer divide by zero`. The same value reaches
`NewInmemSinkFromURL`, so `inmem://x?interval=0s&retain=1m` panics inside what
is otherwise an error-returning constructor.

**Impact** A configuration mistake crashes the process at startup instead of
returning an error.

**Fix** Validate in `NewInmemSink` (clamp to a default, or panic with a message
that names the parameter) and reject a zero interval in `NewInmemSinkFromURL`
with a wrapped error, consistent with the existing `bad 'interval' param`.

---

<a id="4"></a>

## 4. `InmemSink.Data` panics when `retain` is shorter than `interval`

**Package** metrics · **Severity** bug · **Status** Open ·
`inmem.go:105`, `inmem.go:180`

`maxIntervals` becomes 0, so `createInterval` truncates the slice to zero
length right after appending, and `Data` then evaluates `intervals[:n-1]` with
`n == 0`:

```go
s := metrics.NewInmemSink(time.Minute, time.Second) // retain < interval
s.SetGauge("x", 1, nil)
s.Data() // panic: runtime error: slice bounds out of range [:-1]
```

`retain == interval` is equally unusable: `maxIntervals` is 1, so no finished
interval is ever retained and `DisplayMetrics` can only report the partial
current one.

**Impact** Crash from a plausible configuration (`interval=1m&retain=30s`).

**Fix** Require `retain >= 2*interval` in the constructor, and guard `Data`
against an empty interval slice.

---

<a id="5"></a>

## 5. `UpdateFilter` races with the runtime collector

**Package** metrics · **Severity** race · **Status** Open ·
`metrics.go:58`, `start.go:129`, `start.go:255`

`UpdateFilter` writes `AllowedPrefixes`/`BlockedPrefixes` in place while the
`collectStats` goroutine started by `New` reads them through `Prepare` and
`AllowMetric`. The same applies to any other write to the embedded `Config`
after construction.

**Evidence** `go test -race ./...` on the unmodified repository reports 8 data
races, all of this shape:

```
Read at … by goroutine 21:
  metrics.(*Config).AllowMetric()  start.go:256
  metrics.(*Metrics).SetGauge()    metrics.go:12
  metrics.(*Metrics).emitRuntimeStats()
Previous write at … by goroutine 20:
  metrics.(*Metrics).UpdateFilter()  metrics.go:60
  metrics_test.Test_Default()        metrics_test.go:32
```

CI does not run with `-race` (see [#16](#16)), so this is invisible there.

**Impact** Undefined behavior when filters are updated at runtime, which is the
documented purpose of the exported `UpdateFilter`.

**Fix** Hold the filter rules in an immutable struct behind an
`atomic.Pointer`, swapped by `UpdateFilter` and loaded once per emission. That
also removes the per-emit re-reading of two slice headers.

---

<a id="6"></a>

## 6. CloudWatch `Data` shares statistic sets with the live sink

**Package** cloudwatch · **Severity** race · **Status** Open ·
`cloudwatch/cloudwatch.go:329`

`Data` appends `*v`, which copies the `types.MetricDatum` struct but not the
`*types.StatisticSet` it points to, nor the `Dimensions` slice. `Flush`
releases `mu` before `Publish` sends the batch, so concurrent emissions keep
mutating the statistic set that is being serialized.

**Evidence**

```go
cw.AddSample("s", 10, tags)
snap := cw.Data()            // Sum = 10
cw.AddSample("s", 90, tags)  // a normal concurrent emission
// snap[0].StatisticValues.Sum is now 100
```

**Impact** Published minimum, maximum, sum and count can belong to different
moments, and the mutation races with the AWS SDK serializing the request.

**Fix** Copy the statistic set (and the dimensions slice) into the returned
datums. With `WithCleanup` the entry is deleted anyway, so the copy only costs
on the non-cleanup path.

---

<a id="7"></a>

## 7. `Prepare` mutates the caller's tag slice

**Package** metrics · **Severity** bug · **Status** Open ·
`start.go:197`, `start.go:224`

`Prepare` appends `GlobalTags` and the host/service tags to the variadic slice
it is handed, and rewrites tag values in place when `NumberLabelPrefix` is set.
Both effects reach the caller's own array.

**Evidence**

```go
cfg := &metrics.Config{FilterDefault: true, NumberLabelPrefix: "_"}
shared := []metrics.Tag{{Name: "org", Value: "676220136511767142"}}
cfg.Prepare(metrics.TypeCounter, "k", shared...)
// shared[0].Value == "_676220136511767142" — the caller's data changed
```

With `GlobalTags` set, a caller that keeps a reusable tag buffer sees the
global tags overwritten by its own next append, and the emitted tags are wrong:
`[{a 1} {b 2}]` where `[{a 1} {g 1}]` was expected.

**Impact** Tag values drift across emissions for callers that reuse a slice,
and pre-built `[]Tag` values are silently corrupted. `Describe.Tags` allocates
a fresh slice, so metrics declared that way are unaffected.

**Fix** Build a new slice in `Prepare` when tags must be added or rewritten
(`slices.Concat`, or an explicit `make` with the final length), and document
that emitted tags are never the caller's slice.

---

<a id="8"></a>

## 8. `dimensions` panics above 10 tags; AWS allows 30

**Package** cloudwatch · **Severity** bug · **Status** Open ·
`cloudwatch/cloudwatch.go:224`

`logger.Panicf` aborts the emitting goroutine when a metric carries more than
10 tags. Two problems: a metrics library should not take the process down over
a label count, and the limit is stale — CloudWatch has accepted 30 dimensions
per metric since 2022.

**Impact** An instrumented code path panics in production for a reason that has
nothing to do with the work being done.

**Fix** Raise `maxDimensions` to 30 and drop the extra dimensions with a rate
limited error log instead of panicking.

---

<a id="9"></a>

## 9. CloudWatch shutdown flush uses the cancelled context

**Package** cloudwatch · **Severity** bug · **Status** Open ·
`cloudwatch/cloudwatch.go:142`

```go
case <-ctx.Done():
    err := p.Flush(ctx)   // ctx is already done
```

The final flush is handed the context that just ended, so `PutMetricData`
fails immediately with `context canceled` and the last interval of metrics is
lost on every clean shutdown. The error is logged, which makes it look like a
transient AWS failure.

**Fix** Flush with `context.WithoutCancel(ctx)` plus a short timeout.

---

<a id="10"></a>

## 10. `Config.Help` keys can differ from the emitted metric names

**Package** metrics · **Severity** correctness · **Status** Open ·
`start.go:360`

`Help` builds the key with `Prepare(d.Type, d.Name)`, but emissions always use
`TypeCounter`, `TypeGauge` or `TypeSample`. A `Describe` declared with
`Type: "summary"` and `EnableTypePrefix: true` produces the help key
`…_summary_x` while the metric is exported as `…_sample_x`, so the Prometheus
sink never finds the help text and falls back to the metric name.

`Help` also ignores `GlobalTags`, which is correct today only because
`prometheus.Opts.Help` is keyed by name alone.

**Fix** Validate `Describe.Type` against the three constants (or map "summary"
onto `TypeSample`) and cover the `EnableTypePrefix` path in
`Test_DescribeHelp`.

---

<a id="11"></a>

## 11. Prometheus counters are never expired

**Package** prometheus · **Severity** correctness · **Status** Needs Approval ·
`prometheus/prometheus.go:239`

`collectAtTime` deliberately skips expiry for counters, because deleting one
would reset the series. Every distinct tag combination ever emitted is
therefore retained for the process lifetime, in the `counters` `sync.Map` and
in every scrape response.

**Impact** With a tag whose cardinality is not bounded — an account id, a
status string built from an error — memory and scrape size grow without limit.
The root package documents the "keep cardinality acceptable" rule but nothing
enforces it.

**Fix** Options, in increasing order of effort: expose a maximum series count
per sink and log when it is exceeded; expire counters after a much longer,
separately configurable period; or keep the current behavior and document it as
a hard constraint. Needs a decision before implementation.

---

<a id="12"></a>

## 12. The runtime collector goroutine cannot be stopped

**Package** metrics · **Severity** bug · **Status** Open ·
`start.go:129`, `metrics.go:65`

`collectStats` is an unconditional `for { time.Sleep(...) }` loop with no stop
channel, and `Metrics` has no `Close`. Every `New` with
`EnableRuntimeMetrics` (the `DefaultConfig` value) leaks one goroutine that
keeps emitting into a sink the caller may have discarded. The package tests
leak one per test.

**Impact** Leaked goroutines and unexpected emissions in processes that build
more than one provider, plus the races in [#5](#5).

**Fix** Give `Metrics` a `Close()` (or take a `context.Context`) that stops the
collector, and use a `time.Ticker` so the period does not drift by the cost of
a collection.

---

<a id="13"></a>

## 13. `NewPushSink` ignores `Opts`, never registers, and panics on a non-positive interval

**Package** prometheus · **Severity** bug · **Status** Open ·
`prometheus/prometheus.go:461`

`NewPushSink` builds its own `Sink` literal instead of going through
`NewSinkFrom`. Consequences:

- `Opts.Help`, the pre-declared definitions, `Expiration` and `Name` cannot be
  set: expiry is hard-coded to 60s and the name to `default_prometheus_sink`.
- The sink is not registered with any `Registerer`, so it can only be scraped
  through the pusher.
- `time.NewTicker(pushInterval)` panics for a zero or negative interval,
  inside a constructor that returns an error it never uses.
- `Shutdown` closes `stopChan` unconditionally, so a second call panics.

**Fix** Take an `Opts` (or reuse `NewSinkFrom` with a `nil` registerer),
validate the interval and return an error, and make `Shutdown` idempotent with
`sync.Once`.

---

<a id="14"></a>

## 14. `ReadMemStats` stops the world every `ProfileInterval`

**Package** metrics · **Severity** performance · **Status** Open ·
`metrics.go:85`

`emitRuntimeStats` calls `runtime.ReadMemStats`, which stops the world, once per
`ProfileInterval` — one second by default. On a large heap that pause is
measurable, and it is paid by every process that uses `DefaultConfig`.

**Fix** Read the same counters through the `runtime/metrics` package, which
does not stop the world, and raise the default interval. See the roadmap entry.

---

<a id="15"></a>

## 15. Every Prometheus emission copies a struct and stores into `sync.Map`

**Package** prometheus · **Severity** performance · **Status** Open ·
`prometheus/prometheus.go:341`, `:377`, `:414`

The update pattern copies the `gauge`/`summary`/`counter` struct, mutates
`updatedAt`, and stores a new pointer on every emission, purely to keep
`updatedAt` race-free. Measured at 4 allocs and ~196 ns per `IncrCounter`,
against 2 allocs and ~105 ns for the key flattening it also does.

**Fix** Keep `updatedAt` as an `atomic.Int64` (unix nanos) inside the stored
struct and mutate it in place, dropping both the copy and the `Store`.

---

<a id="16"></a>

## 16. CI does not run `-race`, and the coverage gate disagrees with AGENTS.md

**Package** repo · **Severity** docs · **Status** Open ·
`.github/workflows/unittest.yml`

`make covtest` runs without `-race`, so the races in [#5](#5) — reproducible
with the existing tests — never fail a build. `MIN_TESTCOV` is 92 in the
workflow while AGENTS.md documents 80; current coverage is 93.8%.

**Fix** Add a `-race` test step (it can start as non-blocking) and make the two
coverage numbers agree.
