package metrics

import (
	"time"
)

// Tag is used to add dimentions to metrics
type Tag struct {
	Name  string
	Value string
}

// The Sink interface is used to transmit metrics information
// to an external system
type Sink interface {
	// SetGauge should retain the last value it is set to
	SetGauge(key string, val float64, tags []Tag)
	// IncrCounter should accumulate values
	IncrCounter(key string, val float64, tags []Tag)
	// AddSample is for timing information, where quantiles are used
	AddSample(key string, val float64, tags []Tag)
}

// Provider basics
type Provider interface {
	SetGauge(key string, val float64, tags ...Tag)
	IncrCounter(key string, val float64, tags ...Tag)
	AddSample(key string, val float64, tags ...Tag)
	MeasureSince(key string, start time.Time, tags ...Tag)
}

// BlackholeSink is used to just blackhole messages
type BlackholeSink struct{}

// SetGauge should retain the last value it is set to
func (*BlackholeSink) SetGauge(_ string, _ float64, _ []Tag) {}

// IncrCounter should accumulate values
func (*BlackholeSink) IncrCounter(_ string, _ float64, _ []Tag) {}

// AddSample is for timing information, where quantiles are used
func (*BlackholeSink) AddSample(_ string, _ float64, _ []Tag) {}

// FanoutSink is used to sink to fanout values to multiple sinks
type FanoutSink []Sink

// NewFanoutSink creates fan-out sink
func NewFanoutSink(sinks ...Sink) FanoutSink {
	return FanoutSink(sinks)
}

// SetGauge should retain the last value it is set to
func (fh FanoutSink) SetGauge(key string, val float64, tags []Tag) {
	for _, s := range fh {
		s.SetGauge(key, val, tags)
	}
}

// IncrCounter should accumulate values
func (fh FanoutSink) IncrCounter(key string, val float64, tags []Tag) {
	for _, s := range fh {
		s.IncrCounter(key, val, tags)
	}
}

// AddSample is for timing information, where quantiles are used
func (fh FanoutSink) AddSample(key string, val float64, tags []Tag) {
	for _, s := range fh {
		s.AddSample(key, val, tags)
	}
}
