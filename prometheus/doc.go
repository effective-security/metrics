// Package prometheus provides a metrics.Sink backed by the Prometheus client
// library.
//
// A [Sink] is itself a prometheus.Collector: it registers with a
// prometheus.Registerer on construction and is scraped over HTTP by promhttp.
// Metric families are created lazily on first emission, so the set of exported
// series grows with the tag combinations the application actually uses.
//
// Mapping from the metrics package:
//
//   - SetGauge   -> prometheus.Gauge
//   - IncrCounter-> prometheus.Counter
//   - AddSample  -> prometheus.Summary with 0.5/0.9/0.99 objectives
//
// Metric names are sanitized by replacing the characters " ", ".", "=", "-"
// and "/" with "_". Tag names are passed through unchanged; the Prometheus
// client escapes names that are not legacy-valid at exposition time. A series
// the client library rejects outright (an empty name, a label name with the
// reserved "__" prefix, invalid UTF-8) is dropped and logged, because one
// erroring descriptor would fail every scrape until the series expires.
//
// # Expiration
//
// Gauges and summaries created at runtime are removed after [Opts.Expiration]
// without an update, which keeps ephemeral tag combinations from accumulating.
// Counters are kept by default, because deleting a counter resets the series
// and breaks rate() over the gap; set [Opts.CounterExpiration] when the label
// cardinality is unbounded and a reset is the lesser problem. Series
// pre-declared through [Opts.GaugeDefinitions], [Opts.SummaryDefinitions] and
// [Opts.CounterDefinitions] are initialized at zero and never expire.
//
// # Usage
//
//	sink, err := prometheus.NewSinkFrom(prometheus.Opts{
//		Name:       "billing",
//		Expiration: time.Minute,
//		Registerer: prom.DefaultRegisterer,
//		Help:       cfg.Help(billing.Metrics),
//	})
//	if err != nil {
//		return err
//	}
//	if _, err := metrics.NewGlobal(cfg, sink); err != nil {
//		return err
//	}
//	http.Handle("/metrics", promhttp.Handler())
//
// Use [NewPushSinkFrom] instead to push to a Prometheus Pushgateway on an
// interval, and [PushSink.Shutdown] to stop it.
package prometheus
