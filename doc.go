// Package metrics provides a lightweight facade to instrument Go code with
// counters, gauges and samples, and to deliver them to a pluggable backend.
//
// # Model
//
// A [Metrics] instance owns a [Config] and a single [Sink]. Application code
// emits through the [Provider] interface (or the package-level proxies that
// forward to the global instance), and the sink converts the emission into a
// backend-specific representation:
//
//   - [Provider.SetGauge] retains the last value it is set to.
//   - [Provider.IncrCounter] accumulates values.
//   - [Provider.AddSample] records an observation for quantile calculation.
//   - [Provider.MeasureSince] records elapsed time as a sample, in units of
//     [Config.TimerGranularity].
//
// Every emission carries zero or more [Tag] values, which become labels
// (Prometheus), dimensions (CloudWatch) or key suffixes ([InmemSink]).
//
// # Key composition
//
// [Config.Prepare] builds the final metric key from the configured parts,
// in this order (innermost last):
//
//	<GlobalPrefix>_<ServiceName>_<type>_<HostName>_<key>
//
// Each part is included only when the matching option is set: [Config.GlobalPrefix],
// [Config.ServiceName] (unless [Config.EnableServiceLabel] turns it into a label),
// [Config.EnableTypePrefix], and [Config.HostName] (unless
// [Config.EnableHostnameLabel] turns it into a label). The result is then
// matched against [Config.AllowedPrefixes] and [Config.BlockedPrefixes] by
// [Config.AllowMetric]; blocked metrics are dropped before reaching the sink.
//
// # Usage
//
// Create a sink, wire it into the global provider and emit:
//
//	sink := metrics.NewInmemSink(10*time.Second, time.Minute)
//	cfg := metrics.DefaultConfig("billing")
//	cfg.GlobalTags = []metrics.Tag{{Name: "env", Value: "prod"}}
//	if _, err := metrics.NewGlobal(cfg, sink); err != nil {
//		return err
//	}
//
//	defer metrics.MeasureSince("charge_duration", time.Now(),
//		metrics.Tag{Name: "currency", Value: "usd"})
//
// Prefer declaring metrics up-front with [Describe], which validates the tag
// count on every emission and feeds help text to the Prometheus sink:
//
//	var chargeDuration = &metrics.Describe{
//		Type:         metrics.TypeSample,
//		Name:         "charge_duration",
//		Help:         "charge_duration provides the time spent charging a card",
//		RequiredTags: []string{"currency"},
//	}
//
//	defer chargeDuration.MeasureSince(time.Now(), "usd")
//
// # Sinks
//
// The root package ships [InmemSink] (in-memory aggregation, optionally dumped
// on a signal by [InmemSignal]), [BlackholeSink] and [FanoutSink]. Network
// backends live in subpackages: `prometheus` and `cloudwatch`. The `factory`
// subpackage builds a sink from a URL.
//
// # Concurrency
//
// All sinks in this module are safe for concurrent emission, and
// [Metrics.UpdateFilter] may be called while other goroutines emit: it
// replaces the filter rules as a whole. Writing to the fields of a [Config]
// that is in use is not synchronized; construct it, pass it to [New], and
// change the filters through [Metrics.UpdateFilter] afterwards.
//
// [Metrics.Close] stops the runtime metrics collector started by [New].
package metrics
