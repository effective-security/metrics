package metrics

import (
	"fmt"
	"math"
	"time"
)

// AggregateSample is used to hold aggregate metrics
// about a sample.
//
// It is not safe for concurrent use: callers must serialize Ingest against
// readers, which InmemSink does with the interval lock.
type AggregateSample struct {
	Count       int       // The count of emitted pairs
	Rate        float64   // The values rate per time unit (usually 1 second)
	Sum         float64   // The sum of values
	SumSq       float64   `json:"-"` // The sum of squared values
	Min         float64   // Minimum value
	Max         float64   // Maximum value
	LastUpdated time.Time `json:"-"` // When value was last updated
}

// Stddev computes the standard deviation of the ingested values.
// It returns 0 for fewer than two values.
//
// The result is derived from the running sums, which loses precision when the
// values are large relative to their spread.
func (a *AggregateSample) Stddev() float64 {
	num := (float64(a.Count) * a.SumSq) - (a.Sum * a.Sum)
	div := float64(a.Count * (a.Count - 1))
	if div == 0 {
		return 0
	}
	return math.Sqrt(num / div)
}

// Mean computes the mean of the ingested values, or 0 when there are none.
func (a *AggregateSample) Mean() float64 {
	if a.Count == 0 {
		return 0
	}
	return a.Sum / float64(a.Count)
}

// Ingest is used to update a sample with a new value.
// rateDenom is the length of the aggregation interval in the rate time unit,
// and must not be zero.
func (a *AggregateSample) Ingest(v float64, rateDenom float64) {
	a.Count++
	a.Sum += v
	a.SumSq += (v * v)
	if v < a.Min || a.Count == 1 {
		a.Min = v
	}
	if v > a.Max || a.Count == 1 {
		a.Max = v
	}
	a.Rate = a.Sum / rateDenom
	a.LastUpdated = time.Now()
}

// String returns a human readable summary of the sample,
// used by InmemSignal when dumping metrics.
func (a *AggregateSample) String() string {
	if a.Count == 0 {
		return "Count: 0"
	}

	if a.Stddev() == 0 {
		return fmt.Sprintf("Count: %d Sum: %0.3f LastUpdated: %s", a.Count, a.Sum, a.LastUpdated)
	}
	return fmt.Sprintf("Count: %d Min: %0.3f Mean: %0.3f Max: %0.3f Stddev: %0.3f Sum: %0.3f LastUpdated: %s",
		a.Count, a.Min, a.Mean(), a.Max, a.Stddev(), a.Sum, a.LastUpdated)
}
