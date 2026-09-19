# metrics

[![Go Reference](https://pkg.go.dev/badge/github.com/effective-security/metrics.svg)](https://pkg.go.dev/github.com/effective-security/metrics)

A small instrumentation library for Go services: counters, gauges and timing
samples emitted through one interface, delivered to Prometheus, AWS CloudWatch,
memory, or several of them at once.

```sh
go get github.com/effective-security/metrics
```

Requires Go 1.27.

## Why another metrics package

This is a trimmed fork of the well-known `armon/go-metrics` shape, with the
parts that do not survive production removed:

- The `Provider` interface has four methods and no `…WithValues` variants.
- `EmitKey` is gone: Prometheus, the most common backend, has no type that
  retains an arbitrary number of values.
- The `AllowedLabels` config is gone. It is hard to configure correctly and
  costs on every emission; decide which labels you publish instead, and keep
  their cardinality bounded.
- Keys are plain strings, not `[]string`, and tags are explicit `Tag` values.

```go
type Provider interface {
	SetGauge(key string, val float64, tags ...Tag)
	IncrCounter(key string, val float64, tags ...Tag)
	AddSample(key string, val float64, tags ...Tag)
	MeasureSince(key string, start time.Time, tags ...Tag)
}
```

## Quick start

Install a global provider once, during startup, then emit from anywhere.

```go
package main

import (
	"net/http"
	"time"

	"github.com/effective-security/metrics"
	esprom "github.com/effective-security/metrics/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	cfg := metrics.DefaultConfig("billing")
	cfg.GlobalTags = []metrics.Tag{{Name: "env", Value: "prod"}}

	sink, err := esprom.NewSinkFrom(esprom.Opts{
		Name:       "billing",
		Expiration: time.Minute,
		Help:       cfg.Help(chargeMetrics),
	})
	if err != nil {
		panic(err)
	}
	if _, err = metrics.NewGlobal(cfg, sink); err != nil {
		panic(err)
	}

	http.Handle("/metrics", promhttp.Handler())
	_ = http.ListenAndServe(":8080", nil)
}
```

Emitting, with the package-level functions:

```go
func (s *Service) Charge(ctx context.Context, amount float64) error {
	defer metrics.MeasureSince("charge_duration", time.Now(),
		metrics.Tag{Name: "currency", Value: "usd"})

	metrics.IncrCounter("charge_total", 1)
	metrics.SetGauge("charge_amount", amount)
	return nil
}
```

Or against an instance, when you do not want global state — useful in tests:

```go
sink := metrics.NewInmemSink(10*time.Second, time.Minute)
prov, err := metrics.New(metrics.DefaultConfig("billing"), sink)
if err != nil {
	return err
}
prov.IncrCounter("charge_total", 1)
```

## Declaring metrics

Declaring a metric with `Describe` keeps its name, help text and tag names in
one place, validates the tag count on every call, and feeds the help text to
the Prometheus sink through `Config.Help`.

```go
var (
	chargeDuration = &metrics.Describe{
		Type:         metrics.TypeSample,
		Name:         "charge_duration",
		Help:         "charge_duration provides the time spent charging a card",
		RequiredTags: []string{"currency", "provider"},
	}
	chargeTotal = &metrics.Describe{
		Type:         metrics.TypeCounter,
		Name:         "charge_total",
		Help:         "charge_total provides the number of charges",
		RequiredTags: []string{"currency", "provider"},
	}

	// pass to Config.Help, so /metrics carries the HELP text
	chargeMetrics = []*metrics.Describe{chargeDuration, chargeTotal}
)

func (s *Service) Charge(ctx context.Context) error {
	defer chargeDuration.MeasureSince(time.Now(), "usd", "stripe")
	chargeTotal.IncrCounter(1, "usd", "stripe")
	return nil
}
```

A call with the wrong number of values logs an error and emits with a single
`invalid_tags` tag, so the mistake shows up in the backend instead of silently
producing a differently-shaped series. `Describe.Type` must be one of
`metrics.TypeCounter`, `metrics.TypeGauge` or `metrics.TypeSample`; the
legacy value `"summary"` is accepted as an alias of `metrics.TypeSample`, since
that is how the Prometheus sink exports samples. Any other value is logged and
used as given.

## Configuration

| Field | Default (`DefaultConfig`) | Effect |
| --- | --- | --- |
| `ServiceName` | the argument | prefixes every key, or becomes the `service` tag |
| `HostName` | `os.Hostname()` | prefixes every key, or becomes the `host` tag |
| `EnableHostname` | `false` | include the hostname as a key prefix |
| `EnableHostnameLabel` | `false` | include the hostname as a tag instead (wins over the prefix) |
| `EnableServiceLabel` | `false` | include the service as a tag instead of a prefix |
| `EnableTypePrefix` | `false` | prefix the key with `counter`, `gauge` or `sample` |
| `EnableRuntimeMetrics` | `true` | emit `runtime_*` gauges and GC samples in the background, until `Close` |
| `ProfileInterval` | `1s` | how often runtime metrics are collected |
| `TimerGranularity` | `1ms` | the unit `MeasureSince` reports |
| `GlobalPrefix` | empty | prefixed before everything else |
| `GlobalTags` | empty | tags added to every metric |
| `NumberLabelPrefix` | empty | prefix for tag values of 15+ digits, so large IDs are not coerced to floats |
| `AllowedPrefixes` / `BlockedPrefixes` | empty | prefix filters, applied to the final key; a block wins over an allow |
| `FilterDefault` | `true` | verdict for keys that match no prefix rule |

The final metric name is assembled in this order, including only the parts that
are configured:

```
<GlobalPrefix>_<ServiceName>_<type>_<HostName>_<key>
```

For example, `GlobalPrefix: "es"`, `ServiceName: "billing"`,
`EnableTypePrefix: true` turns `IncrCounter("charge_total", 1)` into
`es_billing_counter_charge_total`.

### Filtering

Filters are applied to the assembled key, before the sink is called. A key
matching `BlockedPrefixes` is dropped, a key matching `AllowedPrefixes` is
emitted, and a key matching neither falls back to `FilterDefault`. A block
always wins over an allow.

```go
cfg := metrics.DefaultConfig("billing")
cfg.BlockedPrefixes = []string{"billing_runtime_"} // drop runtime noise
cfg.FilterDefault = true                           // everything else passes
```

To publish only a known set of metrics, list them and turn the default off:

```go
cfg.AllowedPrefixes = []string{"billing_charge_", "billing_refund_"}
cfg.FilterDefault = false // anything else is dropped
```

The rules can be replaced at runtime, from any goroutine, while others emit:

```go
metrics.UpdateFilter(nil, []string{"billing_runtime_"})
```

`UpdateFilter` swaps the whole rule set atomically, so an emission sees either
the old rules or the new ones. It does not write back to the `Config` the
provider was built from; `Metrics.AllowMetric` and `Metrics.Help` report the
rules in effect.

## Tags

Tags become Prometheus labels, CloudWatch dimensions, or part of the key in the
in-memory sink.

```go
metrics.IncrCounter("charge_total", 1,
	metrics.Tag{Name: "currency", Value: "usd"},
	metrics.Tag{Name: "provider", Value: "stripe"})
```

Every distinct combination of values creates a new series. Tag with bounded
values — a status, a region, a provider name — never with user ids, request
ids, or error strings. The Prometheus sink keeps counters for the lifetime of
the process unless `Opts.CounterExpiration` is set, so an unbounded tag grows
memory until then.

## Sinks

| Sink | Package | Use for |
| --- | --- | --- |
| `prometheus.Sink` | `metrics/prometheus` | scraped `/metrics` endpoint |
| `prometheus.PushSink` | `metrics/prometheus` | batch jobs, through a Pushgateway |
| `cloudwatch.Sink` | `metrics/cloudwatch` | AWS CloudWatch custom metrics |
| `metrics.InmemSink` | root | in-process aggregation, debugging, tests |
| `metrics.FanoutSink` | root | several sinks at once |
| `metrics.BlackholeSink` | root | discard everything (the default) |

### Prometheus

```go
sink, err := esprom.NewSinkFrom(esprom.Opts{
	Name:       "billing",
	Expiration: time.Minute,          // drop gauges and summaries idle for a minute
	Registerer: prom.DefaultRegisterer,
	Help:       cfg.Help(chargeMetrics),
	CounterDefinitions: []esprom.CounterDefinition{
		{
			Name: "billing_charge_total",
			Help: "billing_charge_total provides the number of charges",
		},
	},
})
```

Pre-declared series are initialized at zero, are never expired, and appear in
the first scrape even before anything is emitted. Metric names are sanitized
(` `, `.`, `=`, `-`, `/` become `_`). Samples are exported as summaries with the
0.5, 0.9 and 0.99 quantiles.

A series the client library would reject (an empty name, a label name starting
with `__`, invalid UTF-8) is dropped and logged instead of being registered:
one such series would otherwise fail every scrape with HTTP 500 until it
expires. Two sinks on the same registerer need different `Name`s;
`NewSinkFrom` returns an error, and no sink, for a duplicate.

`Expiration` covers gauges and summaries. Counters are kept for the lifetime of
the process, because deleting one resets the series and breaks `rate()` across
the gap. If a counter label is unbounded and a reset is the lesser problem, set
`CounterExpiration`:

```go
opts := esprom.Opts{
	Expiration:        time.Minute,
	CounterExpiration: time.Hour, // drop counters idle for an hour
}
```

To push instead of being scraped, use `NewPushSinkFrom`, which takes the same
options, and `Shutdown` to stop the push loop:

```go
sink, err := esprom.NewPushSinkFrom(gatewayAddr, time.Minute, "nightly-job", opts)
if err != nil {
	return err
}
defer sink.Shutdown()
```

### CloudWatch

```go
sink, err := cloudwatch.NewSink(&cloudwatch.Config{
	AwsRegion:       "us-west-2",
	Namespace:       "billing",
	PublishInterval: time.Minute,
	MetricsExpiry:   time.Hour,
	WithSampleCount: true, // also publish _count, _sum and _avg per sample
	WithCleanup:     true, // each publish covers one interval
})
if err != nil {
	return err
}
go sink.Run(ctx) // publishes until ctx is cancelled
if _, err = metrics.NewGlobal(cfg, sink); err != nil {
	return err
}
```

Metrics are aggregated in memory and published with `PutMetricData` on the
interval, in batches of 1000. Credentials come from the standard AWS chain;
`AWS_REGION` or `AWS_DEFAULT_REGION` is used when `AwsRegion` is empty. Prefer
`WithCleanup: true` — without it each publish repeats the running total.

### In memory, and dumping on a signal

```go
inm := metrics.NewInmemSink(10*time.Second, time.Minute) // bucket, retention
sig := metrics.DefaultInmemSignal(inm)                   // SIGUSR1, SIGBREAK on Windows
defer sig.Stop()

if _, err := metrics.NewGlobal(metrics.DefaultConfig("billing"), inm); err != nil {
	return err
}

// a JSON-shaped snapshot of the last finished interval, for a debug endpoint
summary, err := inm.DisplayMetrics()
```

`retain` must be at least twice `interval`, since one interval is always still
being aggregated. Values outside that range are clamped, with an error logged
naming the parameter; `factory.NewMetricSinkFromURL` rejects them instead.
`Data` returns a snapshot, so it is safe to read while other goroutines emit.

On the signal, the retained intervals are written to stderr:

```
[2026-09-19 14:57:33 +0000 UTC][G] "billing_charge_amount": 42.000
[2026-09-19 14:57:33 +0000 UTC][C] "billing_charge_total": Count: 3 Sum: 3.000 LastUpdated: …
[2026-09-19 14:57:33 +0000 UTC][S] "billing_charge_duration.usd": Count: 3 Min: 1.000 Mean: 41.000 Max: 80.000 Stddev: 39.509 Sum: 123.000 LastUpdated: …
```

### Several backends at once

```go
fan := metrics.NewFanoutSink(promSink, cwSink)
_, err := metrics.NewGlobal(cfg, fan)
```

Emission is sequential, so a slow sink slows down the caller.

### From a URL

```go
sink, err := factory.NewMetricSinkFromURL("inmem://localhost?interval=10s&retain=1m")
```

Only the `inmem` scheme is registered; the other sinks need options that do not
map onto a URL.

## Runtime metrics

With `EnableRuntimeMetrics` (on in `DefaultConfig`) a background goroutine
emits, every `ProfileInterval`:

`runtime_num_goroutines`, `runtime_alloc_bytes`, `runtime_sys_bytes`,
`runtime_malloc_count`, `runtime_free_count`, `runtime_heap_objects`,
`runtime_total_gc_pause_ns`, `runtime_total_gc_runs`, and
`runtime_gc_pause_ns` as a sample per GC cycle.

The collector calls `runtime.ReadMemStats`, which stops the world; on a large
heap, raise `ProfileInterval` or turn the collector off
([FINDINGS #14](FINDINGS.md#14)). Call `Close` to stop it:

```go
prov, err := metrics.New(cfg, sink)
if err != nil {
	return err
}
defer prov.Close()
```

## Testing instrumentation

Use an `InmemSink` and read it back:

```go
inm := metrics.NewInmemSink(time.Second, time.Minute)
prov, err := metrics.New(&metrics.Config{FilterDefault: true}, inm)
require.NoError(t, err)
defer prov.Close()

prov.IncrCounter("charge_total", 1, metrics.Tag{Name: "currency", Value: "usd"})

data := inm.Data()
require.Len(t, data, 1)
assert.Contains(t, data[0].Counters, "charge_total;currency=usd")
```

The aggregation key is the metric name with its tags appended as
`;name=value`. For assertions on what reaches the sink, implement the
three-method `metrics.Sink` interface with a testify mock.

## Development

```sh
make test     # go test ./...
make covtest  # tests + coverage report (CI gate: 92%)
make test RACE=true  # tests under the race detector
make lint     # golangci-lint
make all      # clean, tools, generate, covtest
```

- [`Documentation/codemap.md`](Documentation/codemap.md) — file-by-file map,
  entry points and invariants, for agents and new contributors.
- [`AGENTS.md`](AGENTS.md) — conventions for changes to this repository.
- [`FINDINGS.md`](FINDINGS.md) — known defects, referenced by ID from the code.
- [`ROADMAP.md`](ROADMAP.md) — larger planned work.

## License

[MIT](LICENSE)
