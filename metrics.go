package metrics

import (
	"runtime"
	"slices"
	"strings"
	"time"
)

// SetGauge emits a gauge, which should retain the last value it is set to.
// The metric is dropped when the configured filters block the key.
func (m *Metrics) SetGauge(key string, val float64, tags ...Tag) {
	allowed, keys, labels := m.Prepare(TypeGauge, key, tags...)
	if !allowed {
		return
	}
	m.sink.SetGauge(keys, val, labels)
}

// IncrCounter emits a counter increment, which should accumulate values.
// The metric is dropped when the configured filters block the key.
func (m *Metrics) IncrCounter(key string, val float64, tags ...Tag) {
	allowed, keys, labels := m.Prepare(TypeCounter, key, tags...)
	if !allowed {
		return
	}
	m.sink.IncrCounter(keys, val, labels)
}

// AddSample emits a sample, for timing information where quantiles are used.
// The metric is dropped when the configured filters block the key.
func (m *Metrics) AddSample(key string, val float64, tags ...Tag) {
	allowed, keys, labels := m.Prepare(TypeSample, key, tags...)
	if !allowed {
		return
	}
	m.sink.AddSample(keys, val, labels)
}

// MeasureSince emits the time elapsed since start as a sample,
// in units of Config.TimerGranularity (milliseconds by default).
//
// It is typically deferred at the top of the measured call:
//
//	defer m.MeasureSince("handler_duration", time.Now())
func (m *Metrics) MeasureSince(key string, start time.Time, tags ...Tag) {
	elapsed := time.Since(start)
	msec := float64(elapsed.Nanoseconds()) / float64(m.TimerGranularity)

	allowed, keys, labels := m.Prepare(TypeSample, key, tags...)
	if !allowed {
		return
	}
	m.sink.AddSample(keys, msec, labels)
}

// Prepare returns the final metric name and tags to emit, and whether the
// metric passes the filters currently in effect.
//
// It assembles the key exactly like Config.Prepare, but takes the filter
// verdict from the rules installed by UpdateFilter rather than from the
// embedded Config, which keeps the rules it was constructed with.
func (m *Metrics) Prepare(typ string, key string, tags ...Tag) (bool, string, []Tag) {
	key, out := m.prepare(typ, key, tags)
	return m.AllowMetric(key), key, out
}

// Help returns the help text of the described metrics, keyed by the final
// metric name, omitting the metrics blocked by the filters currently in effect.
// It differs from Config.Help only after UpdateFilter has been called.
func (m *Metrics) Help(providers ...[]*Describe) map[string]string {
	return describeHelp(m.Prepare, providers)
}

// AllowMetric reports whether the key passes the filters currently in effect.
func (m *Metrics) AllowMetric(key string) bool {
	if f := m.filters.Load(); f != nil {
		return f.allow(key)
	}
	return m.Config.AllowMetric(key)
}

// UpdateFilter overwrites the existing filter with the given rules.
// It is safe to call while other goroutines emit metrics: the rules are
// replaced as a whole, so an emission sees either the old or the new set.
//
// The rules are copied. They do not update the embedded Config, which keeps
// the values Metrics was constructed with; Metrics.AllowMetric and
// Metrics.Prepare report the rules in effect.
func (m *Metrics) UpdateFilter(allow, block []string) {
	m.filters.Store(&metricFilters{
		allowed:       slices.Clone(allow),
		blocked:       slices.Clone(block),
		filterDefault: m.FilterDefault,
	})
}

// Close stops the runtime metrics collector started by New.
// It is idempotent and safe for concurrent use. Emission keeps working after
// Close; only the background collector is stopped.
func (m *Metrics) Close() {
	if m.stopCh == nil {
		return
	}
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
}

// collectStats periodically collects runtime stats to publish, until Close.
func (m *Metrics) collectStats() {
	ticker := time.NewTicker(m.ProfileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.emitRuntimeStats()
		}
	}
}

// maxGCPauses is the number of GC pause samples retained by runtime.MemStats.
const maxGCPauses = 256

// emitRuntimeStats emits various runtime statistics.
// runtime.ReadMemStats stops the world, so ProfileInterval should stay
// well above the collection cost; see FINDINGS.md #14.
func (m *Metrics) emitRuntimeStats() {
	// Export number of Goroutines
	numRoutines := runtime.NumGoroutine()
	m.SetGauge("runtime_num_goroutines", float64(numRoutines))

	// Export memory stats
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	m.SetGauge("runtime_alloc_bytes", float64(stats.Alloc))
	m.SetGauge("runtime_sys_bytes", float64(stats.Sys))
	m.SetGauge("runtime_malloc_count", float64(stats.Mallocs))
	m.SetGauge("runtime_free_count", float64(stats.Frees))
	m.SetGauge("runtime_heap_objects", float64(stats.HeapObjects))
	m.SetGauge("runtime_total_gc_pause_ns", float64(stats.PauseTotalNs))
	m.SetGauge("runtime_total_gc_runs", float64(stats.NumGC))

	// Export info about the last few GC runs
	num := stats.NumGC

	// Handle wrap around
	if num < m.lastNumGC {
		m.lastNumGC = 0
	}

	// Ensure we don't scan more than the retained pauses
	if num-m.lastNumGC >= maxGCPauses {
		m.lastNumGC = num - (maxGCPauses - 1)
	}

	for i := m.lastNumGC; i < num; i++ {
		pause := stats.PauseNs[i%maxGCPauses]
		m.AddSample("runtime_gc_pause_ns", float64(pause))
	}
	m.lastNumGC = num
}

// StringStartsWithOneOf returns true if one of items slice is a prefix of specified value.
func StringStartsWithOneOf(value string, items []string) bool {
	for _, x := range items {
		if strings.HasPrefix(value, x) {
			return true
		}
	}
	return false
}
