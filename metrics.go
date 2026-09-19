package metrics

import (
	"runtime"
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

// UpdateFilter overwrites the existing filter with the given rules.
// It is not safe to call while other goroutines emit metrics.
func (m *Metrics) UpdateFilter(allow, block []string) {
	m.AllowedPrefixes = allow
	m.BlockedPrefixes = block
}

// collectStats periodically collects runtime stats to publish.
// It runs until the process exits; there is no way to stop it.
func (m *Metrics) collectStats() {
	for {
		time.Sleep(m.ProfileInterval)
		m.emitRuntimeStats()
	}
}

// maxGCPauses is the number of GC pause samples retained by runtime.MemStats.
const maxGCPauses = 256

// emitRuntimeStats emits various runtime statistics.
// runtime.ReadMemStats stops the world, so ProfileInterval should stay
// well above the collection cost.
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
