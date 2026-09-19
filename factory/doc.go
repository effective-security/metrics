// Package factory constructs a metrics.Sink from a URL, so the sink can be
// selected by configuration instead of by import.
//
//	sink, err := factory.NewMetricSinkFromURL("inmem://localhost?interval=10s&retain=1m")
//	if err != nil {
//		return err
//	}
//	if _, err := metrics.NewGlobal(metrics.DefaultConfig("billing"), sink); err != nil {
//		return err
//	}
//
// Only the "inmem" scheme is registered; the Prometheus and CloudWatch sinks
// need options that do not map onto a URL and must be constructed directly.
package factory
