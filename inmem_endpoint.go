package metrics

import (
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// Summary holds a roll-up of metrics info for a given interval,
// in a shape suitable for JSON serialization on a debug endpoint.
type Summary struct {
	Timestamp string
	Gauges    []GaugeValue
	Counters  []SampledValue
	Samples   []SampledValue
}

// GaugeValue provides gauge value.
type GaugeValue struct {
	// Name is the metric name, without the flattened tags.
	Name string
	// Hash is the aggregation key: the name with its tags appended.
	Hash string `json:"-"`
	// Value is the last value set in the interval.
	Value float64

	// Labels are the tags the value was emitted with.
	Labels []Tag `json:"-"`
	// DisplayLabels is the serializable form of Labels,
	// populated by DisplayMetrics.
	DisplayLabels map[string]string `json:"Labels"`
}

// PointValue provides point value.
//
// It is unused: EmitKey is not part of the Sink interface, because Prometheus
// has no type that retains an arbitrary number of values. The type is retained
// for API compatibility only.
type PointValue struct {
	Name   string
	Points []float64
}

// SampledValue provides sample value.
//
// The embedded AggregateSample is a pointer shared with the interval it was
// read from, so Mean and Stddev are snapshots taken when the value was
// formatted.
type SampledValue struct {
	// Name is the metric name, without the flattened tags.
	Name string
	// Hash is the aggregation key: the name with its tags appended.
	Hash string `json:"-"`
	*AggregateSample
	// Mean of the aggregated values, computed by DisplayMetrics.
	Mean float64
	// Stddev of the aggregated values, computed by DisplayMetrics.
	Stddev float64

	// Labels are the tags the value was emitted with.
	Labels []Tag `json:"-"`
	// DisplayLabels is the serializable form of Labels,
	// populated by DisplayMetrics.
	DisplayLabels map[string]string `json:"Labels"`
}

// DisplayMetrics returns a summary of the metrics from the most recent
// finished interval, with the values sorted by aggregation key.
//
// It returns an error when no interval has been recorded yet. It reads the
// live intervals rather than a snapshot, so it races with concurrent
// emissions; see FINDINGS.md #2.
func (i *InmemSink) DisplayMetrics() (*Summary, error) {
	data := i.Data()

	var interval *IntervalMetrics
	n := len(data)
	switch n {
	case 0:
		return nil, errors.New("no metric intervals have been initialized yet")
	case 1:
		// Show the current interval if it's all we have
		interval = i.intervals[0]
	default:
		// Show the most recent finished interval if we have one
		interval = i.intervals[n-2]
	}

	summary := Summary{
		Timestamp: interval.Interval.Round(time.Second).UTC().String(),
		Gauges:    make([]GaugeValue, 0, len(interval.Gauges)),
	}

	// Format and sort the output of each metric type, so it gets displayed in a
	// deterministic order.
	for hash, value := range interval.Gauges {
		value.Hash = hash
		value.DisplayLabels = make(map[string]string)
		for _, label := range value.Labels {
			value.DisplayLabels[label.Name] = label.Value
		}
		value.Labels = nil

		summary.Gauges = append(summary.Gauges, value)
	}
	slices.SortFunc(summary.Gauges, func(a, b GaugeValue) int {
		return strings.Compare(a.Hash, b.Hash)
	})

	summary.Counters = formatSamples(interval.Counters)
	summary.Samples = formatSamples(interval.Samples)

	return &summary, nil
}

// formatSamples converts the aggregation map into a sorted slice,
// resolving the derived Mean and Stddev and the displayable labels.
func formatSamples(source map[string]SampledValue) []SampledValue {
	output := make([]SampledValue, 0, len(source))
	for hash, sample := range source {
		displayLabels := make(map[string]string, len(sample.Labels))
		for _, label := range sample.Labels {
			displayLabels[label.Name] = label.Value
		}

		output = append(output, SampledValue{
			Name:            sample.Name,
			Hash:            hash,
			AggregateSample: sample.AggregateSample,
			Mean:            sample.AggregateSample.Mean(),
			Stddev:          sample.AggregateSample.Stddev(),
			DisplayLabels:   displayLabels,
		})
	}
	slices.SortFunc(output, func(a, b SampledValue) int {
		return strings.Compare(a.Hash, b.Hash)
	})

	return output
}
