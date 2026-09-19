package metrics

import (
	"time"
)

// Tag is used to add dimensions to metrics.
//
// A tag becomes a Prometheus label, a CloudWatch dimension, or part of the
// aggregation key in InmemSink. Every distinct value combination creates a new
// series in the backend, so keep the cardinality bounded: never tag with user
// IDs, request IDs or other unbounded values.
type Tag struct {
	// Name is the label name. It should be a valid identifier for the
	// destination backend: lower case letters, digits and underscores.
	Name string
	// Value is the label value.
	Value string
}

// The Sink interface is used to transmit metrics information
// to an external system.
//
// Implementations receive the final key, after Config.Prepare has applied the
// prefixes and filters, and must be safe for concurrent use.
type Sink interface {
	// SetGauge should retain the last value it is set to
	SetGauge(key string, val float64, tags []Tag)
	// IncrCounter should accumulate values
	IncrCounter(key string, val float64, tags []Tag)
	// AddSample is for timing information, where quantiles are used
	AddSample(key string, val float64, tags []Tag)
}

// Provider is the interface application code should depend on to emit metrics.
// It is implemented by *Metrics, and mirrored by the package-level functions
// that emit to the global instance.
type Provider interface {
	// SetGauge should retain the last value it is set to
	SetGauge(key string, val float64, tags ...Tag)
	// IncrCounter should accumulate values
	IncrCounter(key string, val float64, tags ...Tag)
	// AddSample is for timing information, where quantiles are used
	AddSample(key string, val float64, tags ...Tag)
	// MeasureSince emits the time elapsed since start as a sample
	MeasureSince(key string, start time.Time, tags ...Tag)
}

// BlackholeSink is used to just blackhole messages.
// It is the sink installed on the global instance until NewGlobal is called.
type BlackholeSink struct{}

// SetGauge discards the value.
func (*BlackholeSink) SetGauge(_ string, _ float64, _ []Tag) {}

// IncrCounter discards the value.
func (*BlackholeSink) IncrCounter(_ string, _ float64, _ []Tag) {}

// AddSample discards the value.
func (*BlackholeSink) AddSample(_ string, _ float64, _ []Tag) {}

// FanoutSink is used to fan out values to multiple sinks,
// for example to expose metrics to Prometheus and CloudWatch at once.
//
// Emission is sequential and synchronous, so a slow sink slows down every
// caller, and a panicking sink prevents the remaining sinks from being called.
type FanoutSink []Sink

// NewFanoutSink creates fan-out sink.
func NewFanoutSink(sinks ...Sink) FanoutSink {
	return FanoutSink(sinks)
}

// SetGauge forwards the gauge to every sink.
func (fh FanoutSink) SetGauge(key string, val float64, tags []Tag) {
	for _, s := range fh {
		s.SetGauge(key, val, tags)
	}
}

// IncrCounter forwards the counter increment to every sink.
func (fh FanoutSink) IncrCounter(key string, val float64, tags []Tag) {
	for _, s := range fh {
		s.IncrCounter(key, val, tags)
	}
}

// AddSample forwards the sample to every sink.
func (fh FanoutSink) AddSample(key string, val float64, tags []Tag) {
	for _, s := range fh {
		s.AddSample(key, val, tags)
	}
}
