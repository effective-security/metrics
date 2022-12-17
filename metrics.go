package metrics

import (
	"runtime"
	"time"

	"github.com/effective-security/porto/x/slices"
)

func (m *Metrics) prepare(typ string, key []string, tags ...Tag) (bool, []string, []Tag) {
	if len(m.GlobalTags) > 0 {
		tags = append(tags, m.GlobalTags...)
	}
	if m.HostName != "" {
		if m.EnableHostnameLabel {
			tags = append(tags, Tag{"host", m.HostName})
		} else if m.EnableHostname {
			key = insert(0, m.HostName, key)
		}
	}
	if m.EnableTypePrefix {
		key = insert(0, typ, key)
	}
	if m.ServiceName != "" {
		if m.EnableServiceLabel {
			tags = append(tags, Tag{"service", m.ServiceName})
		} else {
			key = insert(0, m.ServiceName, key)
		}
	}
	if m.GlobalPrefix != "" {
		key = insert(0, m.GlobalPrefix, key)
	}

	return m.allowMetric(key), key, tags
}

// SetGauge should retain the last value it is set to
func (m *Metrics) SetGauge(key []string, val float32, tags ...Tag) {
	allowed, keys, labels := m.prepare("gauge", key, tags...)
	if !allowed {
		return
	}
	m.sink.SetGauge(keys, val, labels)
}

// IncrCounter should accumulate values
func (m *Metrics) IncrCounter(key []string, val float32, tags ...Tag) {
	allowed, keys, labels := m.prepare("counter", key, tags...)
	if !allowed {
		return
	}
	m.sink.IncrCounter(keys, val, labels)
}

// AddSample is for timing information, where quantiles are used
func (m *Metrics) AddSample(key []string, val float32, tags ...Tag) {
	allowed, keys, labels := m.prepare("sample", key, tags...)
	if !allowed {
		return
	}
	m.sink.AddSample(keys, val, labels)
}

// MeasureSince is for timing information
func (m *Metrics) MeasureSince(key []string, start time.Time, tags ...Tag) {
	elapsed := time.Since(start)
	msec := float32(elapsed.Nanoseconds()) / float32(m.TimerGranularity)

	allowed, keys, labels := m.prepare("timer", key, tags...)
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

// Returns whether the metric should be allowed based on configured prefix filters
// Also return the applicable tags
func (m *Metrics) allowMetric(key []string) bool {
	if len(m.BlockedPrefixes) > 0 {
		if slices.ContainsString(m.BlockedPrefixes, key[0]) {
			return false
		}
	}
	if len(m.AllowedPrefixes) > 0 {
		if !slices.ContainsString(m.BlockedPrefixes, key[0]) {
			return true
		}
	}

	return m.Config.FilterDefault
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
	m.SetGauge([]string{"runtime", "num_goroutines"}, float32(numRoutines))

	// Export memory stats
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	m.SetGauge([]string{"runtime", "alloc_bytes"}, float32(stats.Alloc))
	m.SetGauge([]string{"runtime", "sys_bytes"}, float32(stats.Sys))
	m.SetGauge([]string{"runtime", "malloc_count"}, float32(stats.Mallocs))
	m.SetGauge([]string{"runtime", "free_count"}, float32(stats.Frees))
	m.SetGauge([]string{"runtime", "heap_objects"}, float32(stats.HeapObjects))
	m.SetGauge([]string{"runtime", "total_gc_pause_ns"}, float32(stats.PauseTotalNs))
	m.SetGauge([]string{"runtime", "total_gc_runs"}, float32(stats.NumGC))

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
		m.AddSample([]string{"runtime", "gc_pause_ns"}, float32(pause))
	}
	m.lastNumGC = num
}

// Inserts a string value at an index into the slice
func insert(i int, v string, s []string) []string {
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}
