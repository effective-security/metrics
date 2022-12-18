package metrics

import (
	"runtime"
	"strings"
	"time"
)

// SetGauge should retain the last value it is set to
func (m *Metrics) SetGauge(key string, val float64, tags ...Tag) {
	allowed, keys, labels := m.Prepare("gauge", key, tags...)
	if !allowed {
		return
	}
	m.sink.SetGauge(keys, val, labels)
}

// IncrCounter should accumulate values
func (m *Metrics) IncrCounter(key string, val float64, tags ...Tag) {
	allowed, keys, labels := m.Prepare("counter", key, tags...)
	if !allowed {
		return
	}
	m.sink.IncrCounter(keys, val, labels)
}

// AddSample is for timing information, where quantiles are used
func (m *Metrics) AddSample(key string, val float64, tags ...Tag) {
	allowed, keys, labels := m.Prepare("sample", key, tags...)
	if !allowed {
		return
	}
	m.sink.AddSample(keys, val, labels)
}

// MeasureSince is for timing information
func (m *Metrics) MeasureSince(key string, start time.Time, tags ...Tag) {
	elapsed := time.Since(start)
	msec := float64(elapsed.Nanoseconds()) / float64(m.TimerGranularity)

	allowed, keys, labels := m.Prepare("timer", key, tags...)
	if !allowed {
		return
	}
	m.sink.AddSample(keys, msec, labels)
}

// UpdateFilter overwrites the existing filter with the given rules.
func (m *Metrics) UpdateFilter(allow, block []string) {
	m.AllowedPrefixes = allow
	m.BlockedPrefixes = block
}

// Periodically collects runtime stats to publish
func (m *Metrics) collectStats() {
	for {
		time.Sleep(m.ProfileInterval)
		m.emitRuntimeStats()
	}
}

// Emits various runtime statsitics
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

	// Ensure we don't scan more than 256
	if num-m.lastNumGC >= 256 {
		m.lastNumGC = num - 255
	}

	for i := m.lastNumGC; i < num; i++ {
		pause := stats.PauseNs[i%256]
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
