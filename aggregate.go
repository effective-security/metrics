package metrics

import (
	"fmt"
	"math"
	"time"
)

// AggregateSample is used to hold aggregate metrics
// about a sample
type AggregateSample struct {
	Count       int       // The count of emitted pairs
	Rate        float64   // The values rate per time unit (usually 1 second)
	Sum         float64   // The sum of values
	SumSq       float64   `json:"-"` // The sum of squared values
	Min         float64   // Minimum value
	Max         float64   // Maximum value
	LastUpdated time.Time `json:"-"` // When value was last updated
}

// Stddev computes a Stddev of the values
func (a *AggregateSample) Stddev() float64 {
	num := (float64(a.Count) * a.SumSq) - math.Pow(a.Sum, 2)
	div := float64(a.Count * (a.Count - 1))
	if div == 0 {
		return 0
	}
	return math.Sqrt(num / div)
}

// Mean computes a mean of the values
func (a *AggregateSample) Mean() float64 {
	if a.Count == 0 {
		return 0
	}
	return a.Sum / float64(a.Count)
}

// Ingest is used to update a sample
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
	a.Rate = float64(a.Sum) / rateDenom
	a.LastUpdated = time.Now()
}

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
