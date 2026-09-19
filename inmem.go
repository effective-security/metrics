package metrics

import (
	"maps"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/effective-security/xlog"
)

// InmemSink provides a Sink that does in-memory aggregation
// without sending metrics over a network. It can be embedded within
// an application to provide profiling information.
//
// Values are bucketed into fixed aggregation intervals and the oldest buckets
// are discarded, so memory is bounded by retain/interval times the number of
// distinct key and tag combinations. Read the aggregated data with Data or
// DisplayMetrics, or dump it on a signal with InmemSignal.
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
// for a specific interval.
//
// The embedded RWMutex guards the maps; hold it while reading them.
type IntervalMetrics struct {
	sync.RWMutex

	// The start time of the interval
	Interval time.Time

	// Gauges maps the key to the last set value
	Gauges map[string]GaugeValue

	// Counters maps the string key to a sum of the counter
	// values
	Counters map[string]SampledValue

	// Samples maps the key to an AggregateSample,
	// which has the rolled up view of a sample
	Samples map[string]SampledValue
}

// NewIntervalMetrics creates a new IntervalMetrics for a given interval.
func NewIntervalMetrics(intv time.Time) *IntervalMetrics {
	return &IntervalMetrics{
		Interval: intv,
		Gauges:   make(map[string]GaugeValue),
		Counters: make(map[string]SampledValue),
		Samples:  make(map[string]SampledValue),
	}
}

// NewInmemSinkFromURL creates an InmemSink from a URL. It is used
// (and tested) from factory.NewMetricSinkFromURL.
//
// The "interval" and "retain" query parameters are required and must parse as
// durations, for example:
//
//	inmem://localhost?interval=10s&retain=1m
//
// Unlike NewInmemSink, an interval that is not positive or a retention shorter
// than two intervals is rejected with an error instead of being clamped.
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
	if interval <= 0 {
		return nil, errors.Errorf("bad 'interval' param: must be positive, got %s", interval)
	}
	if retain < minRetainIntervals*interval {
		return nil, errors.Errorf("bad 'retain' param: must be at least %s for interval %s, got %s",
			minRetainIntervals*interval, interval, retain)
	}

	return NewInmemSink(interval, retain), nil
}

// Retention bounds enforced by NewInmemSink.
const (
	// defaultInmemInterval replaces an interval that is not positive.
	defaultInmemInterval = 10 * time.Second
	// minRetainIntervals is the smallest usable retention, in intervals:
	// one interval is always being aggregated, so reporting a finished one
	// needs a second.
	minRetainIntervals = 2
)

// NewInmemSink is used to construct a new in-memory sink,
// with an aggregation interval and a maximum retention period.
//
// interval must be greater than zero and retain must be at least twice the
// interval. Values outside that range are clamped, with an error logged
// naming the parameter, so a misconfiguration degrades the reported window
// instead of panicking at the first emission.
func NewInmemSink(interval, retain time.Duration) *InmemSink {
	if interval <= 0 {
		logger.KV(xlog.ERROR,
			"reason", "invalid_interval",
			"interval", interval,
			"using", defaultInmemInterval,
		)
		interval = defaultInmemInterval
	}
	if minRetain := minRetainIntervals * interval; retain < minRetain {
		logger.KV(xlog.ERROR,
			"reason", "invalid_retain",
			"retain", retain,
			"interval", interval,
			"using", minRetain,
		)
		retain = minRetain
	}

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

// SetGauge retains the last value it is set to, within the current interval.
func (i *InmemSink) SetGauge(key string, val float64, tags []Tag) {
	k, name := i.flattenKeyLabels(key, tags)
	intv := i.getInterval()

	intv.Lock()
	defer intv.Unlock()
	intv.Gauges[k] = GaugeValue{Name: name, Value: val, Labels: tags}
}

// IncrCounter accumulates values within the current interval.
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
	agg.Ingest(val, i.rateDenom)
}

// AddSample records an observation within the current interval,
// rolled up into an AggregateSample.
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
	agg.Ingest(val, i.rateDenom)
}

// Data is used to retrieve all the aggregated metrics, oldest interval first.
// The last entry is the interval currently being written to.
//
// Every interval is an independent copy, taken under its own lock, including
// the AggregateSample behind each counter and sample. Reading the result needs
// no lock and it does not change afterwards. Even a finished interval has to
// be copied: an emitter can resolve its interval, be descheduled across the
// bucket rollover, and write to it after it stopped being the current one.
//
// The Tag slices in Labels are shared with the sink and must be treated as
// read-only.
func (i *InmemSink) Data() []*IntervalMetrics {
	// Get the current interval, forces creation
	i.getInterval()

	i.intervalLock.RLock()
	defer i.intervalLock.RUnlock()

	intervals := make([]*IntervalMetrics, len(i.intervals))
	for idx, intv := range i.intervals {
		intervals[idx] = intv.clone()
	}

	return intervals
}

// clone returns an independent copy of the interval, taken under its read lock.
func (m *IntervalMetrics) clone() *IntervalMetrics {
	m.RLock()
	defer m.RUnlock()

	out := &IntervalMetrics{
		Interval: m.Interval,
		Gauges:   make(map[string]GaugeValue, len(m.Gauges)),
		Counters: cloneSampledValues(m.Counters),
		Samples:  cloneSampledValues(m.Samples),
	}
	maps.Copy(out.Gauges, m.Gauges)

	return out
}

// cloneSampledValues copies the map together with the AggregateSample each
// value points to, so the result shares no statistics with the source.
func cloneSampledValues(src map[string]SampledValue) map[string]SampledValue {
	out := make(map[string]SampledValue, len(src))
	for k, v := range src {
		if v.AggregateSample != nil {
			agg := *v.AggregateSample
			v.AggregateSample = &agg
		}
		out[k] = v
	}
	return out
}

// getExistingInterval returns the interval for intv if it is the current one.
func (i *InmemSink) getExistingInterval(intv time.Time) *IntervalMetrics {
	i.intervalLock.RLock()
	defer i.intervalLock.RUnlock()

	n := len(i.intervals)
	if n > 0 && i.intervals[n-1].Interval.Equal(intv) {
		return i.intervals[n-1]
	}
	return nil
}

// createInterval appends the interval for intv, unless a racing caller already
// did, and drops the intervals that fell out of the retention window.
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

	// Truncate the intervals if they are too long, and clear the vacated
	// tail so the evicted intervals can be collected
	if n > i.maxIntervals {
		copy(i.intervals[0:], i.intervals[n-i.maxIntervals:])
		clear(i.intervals[i.maxIntervals:n])
		i.intervals = i.intervals[:i.maxIntervals]
	}
	return current
}

// getInterval returns the current interval to write to, creating it if needed.
func (i *InmemSink) getInterval() *IntervalMetrics {
	intv := time.Now().Truncate(i.interval)
	if m := i.getExistingInterval(intv); m != nil {
		return m
	}
	return i.createInterval(intv)
}

// keyReplacer normalizes keys and tags for the flattened aggregation key.
var keyReplacer = strings.NewReplacer(" ", "_")

// labelKeySizeHint is the assumed size of one ";name=value" pair,
// used to pre-size the flattened key buffer.
const labelKeySizeHint = 24

// flattenKeyLabels flattens the key for formatting along with its tags and
// removes spaces. It returns the aggregation key, which includes the tags, and
// the plain metric name.
func (i *InmemSink) flattenKeyLabels(key string, tags []Tag) (string, string) {
	if len(tags) == 0 {
		return keyReplacer.Replace(key), key
	}

	buf := new(strings.Builder)
	buf.Grow(len(key) + len(tags)*labelKeySizeHint)

	_, _ = keyReplacer.WriteString(buf, key)
	for _, label := range tags {
		buf.WriteByte(';')
		_, _ = keyReplacer.WriteString(buf, label.Name)
		buf.WriteByte('=')
		_, _ = keyReplacer.WriteString(buf, label.Value)
	}

	return buf.String(), key
}
