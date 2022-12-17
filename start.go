package metrics

import (
	"os"
	"sync/atomic"
	"time"
)

// Config is used to configure metrics settings
type Config struct {
	ServiceName          string        // Prefixed with keys to separate services
	HostName             string        // Hostname to use. If not provided and EnableHostname, it will be os.Hostname
	EnableHostname       bool          // Enable prefixing gauge values with hostname
	EnableHostnameLabel  bool          // Enable adding hostname to labels
	EnableServiceLabel   bool          // Enable adding service to labels
	EnableRuntimeMetrics bool          // Enables profiling of runtime metrics (GC, Goroutines, Memory)
	EnableTypePrefix     bool          // Prefixes key with a type ("counter", "gauge", "timer")
	TimerGranularity     time.Duration // Granularity of timers.
	ProfileInterval      time.Duration // Interval to profile runtime metrics
	GlobalTags           []Tag         // Tags to add to every metric
	GlobalPrefix         string        // Prefix to add to every metric

	AllowedPrefixes []string // A list of the first metric prefixes to allow
	BlockedPrefixes []string // A list of the first metric prefixes to block
	FilterDefault   bool     // Whether to allow metrics by default
}

// Metrics represents an instance of a metrics sink that can
// be used to emit
type Metrics struct {
	Config
	lastNumGC uint32
	sink      Sink
}

// Shared global metrics instance
var globalMetrics atomic.Value // *Metrics

func init() {
	// Initialize to a blackhole sink to avoid errors
	globalMetrics.Store(&Metrics{sink: &BlackholeSink{}})
}

// DefaultConfig provides a sane default configuration
func DefaultConfig(serviceName string) *Config {
	// Try to get the hostname
	name, _ := os.Hostname()

	c := &Config{
		ServiceName:          serviceName, // Use client provided service
		HostName:             name,
		EnableHostname:       false,            // Enable hostname prefix
		EnableRuntimeMetrics: true,             // Enable runtime profiling
		EnableTypePrefix:     false,            // Disable type prefix
		TimerGranularity:     time.Millisecond, // Timers are in milliseconds
		ProfileInterval:      time.Second,      // Poll runtime every second
		FilterDefault:        true,             // Don't filter metrics by default
	}
	return c
}

// New is used to create a new instance of Metrics
func New(conf *Config, sink Sink) (*Metrics, error) {
	met := &Metrics{}
	met.Config = *conf
	met.sink = sink
	met.UpdateFilter(conf.AllowedPrefixes, conf.BlockedPrefixes)

	if met.Config.TimerGranularity == 0 {
		met.Config.TimerGranularity = time.Millisecond
	}
	if met.Config.ProfileInterval == 0 {
		met.Config.ProfileInterval = time.Second
	}

	// Start the runtime collector
	if conf.EnableRuntimeMetrics {
		go met.collectStats()
	}
	return met, nil
}

// NewGlobal is the same as New, but it assigns the metrics object to be
// used globally as well as returning it.
func NewGlobal(conf *Config, sink Sink) (*Metrics, error) {
	metrics, err := New(conf, sink)
	if err == nil {
		globalMetrics.Store(metrics)
	}
	return metrics, err
}

// Proxy all the methods to the globalMetrics instance

// SetGauge should retain the last value it is set to
func SetGauge(key []string, val float32, tags ...Tag) {
	globalMetrics.Load().(*Metrics).SetGauge(key, val, tags...)
}

// IncrCounter should accumulate values
func IncrCounter(key []string, val float32, tags ...Tag) {
	globalMetrics.Load().(*Metrics).IncrCounter(key, val, tags...)
}

// AddSample is for timing information, where quantiles are used
func AddSample(key []string, val float32, tags ...Tag) {
	globalMetrics.Load().(*Metrics).AddSample(key, val, tags...)
}

// MeasureSince is for timing information
func MeasureSince(key []string, start time.Time, tags ...Tag) {
	globalMetrics.Load().(*Metrics).MeasureSince(key, start, tags...)
}

// UpdateFilter updates filters
func UpdateFilter(allow, block []string) {
	globalMetrics.Load().(*Metrics).UpdateFilter(allow, block)
}
