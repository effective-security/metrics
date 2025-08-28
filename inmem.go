package metrics

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
)

// InmemSink provides a MetricSink that does in-memory aggregation
// without sending metrics over a network. It can be embedded within
// an application to provide profiling information.
type InmemSink struct {
	// How long is each aggregation interval
	interval time.Duration

	// Retain controls how many metrics interval we keep
	retain time.Duration

	// maxIntervals is the maximum length of intervals.
	// It is retain / interval.
	maxIntervals int

	// intervals is a slice of the retained intervals
	intervals    []*IntervalMetrics
	intervalLock sync.RWMutex

	rateDenom float64
}

// IntervalMetrics stores the aggregated metrics
// for a specific interval
type IntervalMetrics struct {
	sync.RWMutex

	// The start time of the interval
	Interval time.Time

	// Gauges maps the key to the last set value
	Gauges map[string]GaugeValue

	// Points maps the string to the list of emitted values
	// from EmitKey
	//Points map[string][]float32

	// Counters maps the string key to a sum of the counter
	// values
	Counters map[string]SampledValue

	// Samples maps the key to an AggregateSample,
	// which has the rolled up view of a sample
	Samples map[string]SampledValue
}

// NewIntervalMetrics creates a new IntervalMetrics for a given interval
func NewIntervalMetrics(intv time.Time) *IntervalMetrics {
	return &IntervalMetrics{
		Interval: intv,
		Gauges:   make(map[string]GaugeValue),
		//Points:   make(map[string][]float32),
		Counters: make(map[string]SampledValue),
		Samples:  make(map[string]SampledValue),
	}
}

// NewInmemSinkFromURL creates an InmemSink from a URL. It is used
// (and tested) from NewMetricSinkFromURL.
func NewInmemSinkFromURL(u *url.URL) (Sink, error) {
	params := u.Query()

	interval, err := time.ParseDuration(params.Get("interval"))
	if err != nil {
		return nil, errors.WithMessage(err, "bad 'interval' param")
	}

	retain, err := time.ParseDuration(params.Get("retain"))
	if err != nil {
		return nil, errors.WithMessage(err, "bad 'retain' param")
	}

	return NewInmemSink(interval, retain), nil
}

// NewInmemSink is used to construct a new in-memory sink.
// Uses an aggregation interval and maximum retention period.
func NewInmemSink(interval, retain time.Duration) *InmemSink {
	rateTimeUnit := time.Second
	i := &InmemSink{
		interval:     interval,
		retain:       retain,
		maxIntervals: int(retain / interval),
		rateDenom:    float64(interval.Nanoseconds()) / float64(rateTimeUnit.Nanoseconds()),
	}
	i.intervals = make([]*IntervalMetrics, 0, i.maxIntervals)
	return i
}

// SetGauge should retain the last value it is set to
func (i *InmemSink) SetGauge(key string, val float64, tags []Tag) {
	k, name := i.flattenKeyLabels(key, tags)
	intv := i.getInterval()

	intv.Lock()
	defer intv.Unlock()
	intv.Gauges[k] = GaugeValue{Name: name, Value: val, Labels: tags}
}

// IncrCounter should accumulate values
func (i *InmemSink) IncrCounter(key string, val float64, tags []Tag) {
	k, name := i.flattenKeyLabels(key, tags)
	intv := i.getInterval()

	intv.Lock()
	defer intv.Unlock()

	agg, ok := intv.Counters[k]
	if !ok {
		agg = SampledValue{
			Name:            name,
			AggregateSample: &AggregateSample{},
			Labels:          tags,
		}
		intv.Counters[k] = agg
	}
	agg.Ingest(float64(val), i.rateDenom)
}

// AddSample is for timing information, where quantiles are used
func (i *InmemSink) AddSample(key string, val float64, tags []Tag) {
	k, name := i.flattenKeyLabels(key, tags)
	intv := i.getInterval()

	intv.Lock()
	defer intv.Unlock()

	agg, ok := intv.Samples[k]
	if !ok {
		agg = SampledValue{
			Name:            name,
			AggregateSample: &AggregateSample{},
			Labels:          tags,
		}
		intv.Samples[k] = agg
	}
	agg.Ingest(float64(val), i.rateDenom)
}

// Data is used to retrieve all the aggregated metrics
// Intervals may be in use, and a read lock should be acquired
func (i *InmemSink) Data() []*IntervalMetrics {
	// Get the current interval, forces creation
	i.getInterval()

	i.intervalLock.RLock()
	defer i.intervalLock.RUnlock()

	n := len(i.intervals)
	intervals := make([]*IntervalMetrics, n)

	copy(intervals[:n-1], i.intervals[:n-1])
	current := i.intervals[n-1]

	// make its own copy for current interval
	intervals[n-1] = &IntervalMetrics{}
	copyCurrent := intervals[n-1]
	current.RLock()
	copyCurrent.Interval = current.Interval

	copyCurrent.Gauges = make(map[string]GaugeValue, len(current.Gauges))
	for k, v := range current.Gauges {
		copyCurrent.Gauges[k] = v
	}
	// saved values will be not change, just copy its link
	/*
		copyCurrent.Points = make(map[string][]float32, len(current.Points))
		for k, v := range current.Points {
			copyCurrent.Points[k] = v
		}
	*/
	copyCurrent.Counters = make(map[string]SampledValue, len(current.Counters))
	for k, v := range current.Counters {
		copyCurrent.Counters[k] = v
	}
	copyCurrent.Samples = make(map[string]SampledValue, len(current.Samples))
	for k, v := range current.Samples {
		copyCurrent.Samples[k] = v
	}
	current.RUnlock()

	return intervals
}

func (i *InmemSink) getExistingInterval(intv time.Time) *IntervalMetrics {
	i.intervalLock.RLock()
	defer i.intervalLock.RUnlock()

	n := len(i.intervals)
	if n > 0 && i.intervals[n-1].Interval.Equal(intv) {
		return i.intervals[n-1]
	}
	return nil
}

func (i *InmemSink) createInterval(intv time.Time) *IntervalMetrics {
	i.intervalLock.Lock()
	defer i.intervalLock.Unlock()

	// Check for an existing interval
	n := len(i.intervals)
	if n > 0 && i.intervals[n-1].Interval.Equal(intv) {
		return i.intervals[n-1]
	}

	// Add the current interval
	current := NewIntervalMetrics(intv)
	i.intervals = append(i.intervals, current)
	n++

	// Truncate the intervals if they are too long
	if n >= i.maxIntervals {
		copy(i.intervals[0:], i.intervals[n-i.maxIntervals:])
		i.intervals = i.intervals[:i.maxIntervals]
	}
	return current
}

// getInterval returns the current interval to write to
func (i *InmemSink) getInterval() *IntervalMetrics {
	intv := time.Now().Truncate(i.interval)
	if m := i.getExistingInterval(intv); m != nil {
		return m
	}
	return i.createInterval(intv)
}

// Flattens the key for formatting along with its tags, removes spaces
func (i *InmemSink) flattenKeyLabels(key string, tags []Tag) (string, string) {
	buf := &bytes.Buffer{}
	replacer := strings.NewReplacer(" ", "_")

	_, _ = replacer.WriteString(buf, key)

	for _, label := range tags {
		_, _ = replacer.WriteString(buf, fmt.Sprintf(";%s=%s", label.Name, label.Value))
	}

	return buf.String(), key
}
