// Package cloudwatch provides a metrics.Sink that publishes to AWS CloudWatch.
//
// A [Sink] aggregates emissions in memory and flushes them with PutMetricData
// on the interval configured by [Config.PublishInterval]. Aggregation avoids one
// API call per emission, which CloudWatch bills per request.
//
// Mapping from the metrics package:
//
//   - SetGauge    -> MetricDatum.Value, last value wins
//   - IncrCounter -> MetricDatum.Value, accumulated
//   - AddSample   -> MetricDatum.StatisticValues (min/max/sum/count)
//
// Tags become CloudWatch dimensions. Metric names are published verbatim; unlike
// the Prometheus sink no character replacement is applied.
//
// # Lifecycle
//
// [NewSink] only builds the client. Call [Sink.Run] in its own goroutine to
// start the publish loop, and cancel its context to stop it; [Sink.Flush]
// publishes on demand.
//
//	sink, err := cloudwatch.NewSink(&cloudwatch.Config{
//		AwsRegion:       "us-west-2",
//		Namespace:       "billing",
//		PublishInterval: time.Minute,
//		MetricsExpiry:   time.Hour,
//		WithSampleCount: true,
//		WithCleanup:     true,
//	})
//	if err != nil {
//		return err
//	}
//	go sink.Run(ctx)
//	if _, err := metrics.NewGlobal(cfg, sink); err != nil {
//		return err
//	}
//
// # Retention
//
// With [Config.WithCleanup] the aggregated values are dropped once published, so
// each datum reports the activity of one publish interval. Without it, values
// keep accumulating and every flush republishes the running total until
// [Config.MetricsExpiry] elapses without an update. Prefer WithCleanup unless a
// consumer depends on the cumulative shape.
package cloudwatch
