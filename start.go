package metrics

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/effective-security/xlog"
)

var logger = xlog.NewPackageLogger("github.com/effective-security/metrics", "metrics")

// Config is used to configure metrics settings.
//
// The zero value emits nothing: FilterDefault must be set to true, or an
// explicit AllowedPrefixes list must be provided, for metrics to reach the sink.
// See Prepare for how the fields combine into the final metric key.
type Config struct {
	// ServiceName is prefixed to keys to separate services,
	// or emitted as the "service" label when EnableServiceLabel is set.
	ServiceName string
	// HostName to use. If not provided and EnableHostname is set,
	// DefaultConfig uses os.Hostname.
	HostName string
	// EnableHostname enables prefixing keys with HostName.
	// Ignored when EnableHostnameLabel is set.
	EnableHostname bool
	// EnableHostnameLabel enables adding HostName as the "host" tag.
	// Takes precedence over EnableHostname.
	EnableHostnameLabel bool
	// EnableServiceLabel enables adding ServiceName as the "service" tag
	// instead of prefixing it to the key.
	EnableServiceLabel bool
	// EnableRuntimeMetrics enables profiling of runtime metrics
	// (GC, Goroutines, Memory). New starts a background goroutine for it that
	// runs for the lifetime of the process.
	EnableRuntimeMetrics bool
	// EnableTypePrefix prefixes the key with the metric type:
	// TypeCounter, TypeGauge or TypeSample.
	EnableTypePrefix bool
	// TimerGranularity is the unit reported by MeasureSince.
	// Defaults to time.Millisecond.
	TimerGranularity time.Duration
	// ProfileInterval is the interval at which runtime metrics are collected,
	// when EnableRuntimeMetrics is set. Defaults to time.Second.
	ProfileInterval time.Duration
	// GlobalTags are added to every metric.
	GlobalTags []Tag
	// GlobalPrefix is prepended to every metric key, before all other prefixes.
	GlobalPrefix string
	// NumberLabelPrefix is prepended to tag values that are 64-bit numbers,
	// to keep backends from coercing large IDs into floats.
	// A value is treated as a number when it has at least 15 digits.
	NumberLabelPrefix string

	// AllowedPrefixes is a list of metric key prefixes to allow.
	AllowedPrefixes []string
	// BlockedPrefixes is a list of metric key prefixes to block.
	// A blocked prefix always wins over an allowed one.
	BlockedPrefixes []string
	// FilterDefault is the verdict for keys that no prefix rule decides:
	// true allows them, false drops them.
	FilterDefault bool
}

// Metrics represents an instance of a metrics sink that can
// be used to emit.
//
// A Metrics value is safe for concurrent emission. Reconfiguring it after
// construction, with UpdateFilter or by writing to the embedded Config,
// is not synchronized against concurrent emission.
type Metrics struct {
	Config

	lastNumGC uint32
	sink      Sink
}

// globalMetrics is the shared instance used by the package-level emit functions.
var globalMetrics atomic.Pointer[Metrics]

func init() {
	// Initialize to a blackhole sink to avoid errors
	globalMetrics.Store(&Metrics{sink: &BlackholeSink{}})
}

// DefaultConfig provides a sane default configuration:
// runtime metrics enabled, millisecond timers, no filtering,
// and the local hostname resolved but not emitted.
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

// New is used to create a new instance of Metrics.
//
// The configuration is copied, so later changes to conf do not affect the
// returned instance. When conf.EnableRuntimeMetrics is set, New starts a
// collector goroutine that cannot be stopped; create at most one such instance
// per process. The error is always nil and exists for API compatibility.
func New(conf *Config, sink Sink) (*Metrics, error) {
	met := &Metrics{}
	met.Config = *conf
	met.sink = sink
	met.UpdateFilter(conf.AllowedPrefixes, conf.BlockedPrefixes)

	if met.TimerGranularity == 0 {
		met.TimerGranularity = time.Millisecond
	}
	if met.ProfileInterval == 0 {
		met.ProfileInterval = time.Second
	}

	// Start the runtime collector
	if conf.EnableRuntimeMetrics {
		go met.collectStats()
	}
	return met, nil
}

// NewGlobal is the same as New, but it assigns the metrics object to be
// used globally as well as returning it.
//
// The instance previously installed as global, and its runtime collector,
// are not shut down.
func NewGlobal(conf *Config, sink Sink) (*Metrics, error) {
	metrics, err := New(conf, sink)
	if err == nil {
		globalMetrics.Store(metrics)
	}
	return metrics, err
}

// Proxy all the methods to the globalMetrics instance

// SetGauge emits a gauge on the global instance, which should retain
// the last value it is set to.
func SetGauge(key string, val float64, tags ...Tag) {
	globalMetrics.Load().SetGauge(key, val, tags...)
}

// IncrCounter emits a counter increment on the global instance.
func IncrCounter(key string, val float64, tags ...Tag) {
	globalMetrics.Load().IncrCounter(key, val, tags...)
}

// AddSample emits a sample on the global instance,
// for timing information where quantiles are used.
func AddSample(key string, val float64, tags ...Tag) {
	globalMetrics.Load().AddSample(key, val, tags...)
}

// MeasureSince emits the time elapsed since start as a sample on the
// global instance, in units of Config.TimerGranularity.
func MeasureSince(key string, start time.Time, tags ...Tag) {
	globalMetrics.Load().MeasureSince(key, start, tags...)
}

// UpdateFilter replaces the prefix filters of the global instance.
// It is not safe to call while other goroutines emit metrics.
func UpdateFilter(allow, block []string) {
	globalMetrics.Load().UpdateFilter(allow, block)
}

// maxNumberlen is the digit count from which a tag value is considered
// a 64-bit number: the JavaScript max safe integer (9007199254740991) has 16
// digits, so 15 digits is the shortest value worth protecting from float
// coercion. See Config.NumberLabelPrefix.
const maxNumberlen = 15

// Prepare returns the final metric name and tags to emit, and whether the
// metric passes the configured filters.
//
// The key is assembled from the configured parts, in this order:
//
//	<GlobalPrefix>_<ServiceName>_<typ>_<HostName>_<key>
//
// and tags are extended with GlobalTags plus the optional "host" and "service"
// labels. Callers must treat the tags they pass in as owned by Prepare: it
// appends to the given slice and rewrites values in place when
// NumberLabelPrefix is set.
func (m *Config) Prepare(typ string, key string, tags ...Tag) (bool, string, []Tag) {
	if len(m.GlobalTags) > 0 {
		tags = append(tags, m.GlobalTags...)
	}
	if m.HostName != "" {
		if m.EnableHostnameLabel {
			tags = append(tags, Tag{"host", m.HostName})
		} else if m.EnableHostname {
			key = m.HostName + "_" + key
		}
	}
	if m.EnableTypePrefix {
		key = typ + "_" + key
	}
	if m.ServiceName != "" {
		if m.EnableServiceLabel {
			tags = append(tags, Tag{"service", m.ServiceName})
		} else {
			key = m.ServiceName + "_" + key
		}
	}

	if m.GlobalPrefix != "" {
		key = m.GlobalPrefix + "_" + key
	}

	if m.NumberLabelPrefix != "" {
		for idx, tag := range tags {
			if isBigNumber(tag.Value) {
				tags[idx].Value = m.NumberLabelPrefix + tag.Value
			}
		}
	}

	return m.AllowMetric(key), key, tags
}

// isBigNumber reports whether s is an optionally signed decimal integer
// of at least maxNumberlen digits.
func isBigNumber(s string) bool {
	if len(s) < maxNumberlen {
		return false
	}
	for idx, ch := range s {
		if idx == 0 && ch == '-' {
			continue
		}
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// AllowMetric returns whether the metric should be allowed based on the
// configured prefix filters.
//
// A key matching BlockedPrefixes is always dropped. Any other key currently
// resolves to FilterDefault: AllowedPrefixes does not restrict emission.
// See FINDINGS.md #1 before relying on an allow-list.
func (m *Config) AllowMetric(key string) bool {
	if len(m.BlockedPrefixes) > 0 {
		if StringStartsWithOneOf(key, m.BlockedPrefixes) {
			return false
		}
	}
	if len(m.AllowedPrefixes) > 0 {
		if !StringStartsWithOneOf(key, m.AllowedPrefixes) {
			return true
		}
	}

	return m.FilterDefault
}

// Metric types, used as the type prefix when Config.EnableTypePrefix is set,
// and as Describe.Type.
const (
	TypeCounter = "counter"
	TypeSample  = "sample"
	TypeGauge   = "gauge"
)

// Describe provides metric description.
//
// Declaring metrics as package-level Describe values keeps the name, help text
// and tag names in one place, and validates on every emission that the caller
// supplies exactly the expected tag values. Its emit methods always go to the
// global instance.
type Describe struct {
	// Type of the metric: counter|gauge|summary
	Type string
	// Name is the metric name
	Name string
	// Help provides description
	Help string
	// RequiredTags is a list of metric tags
	RequiredTags []string
}

// Tags constructs tags. The size and order of the vals must match the ones in
// the description.
//
// On mismatch it logs an error and returns a single "invalid_tags" tag, so a
// miscounted call is visible in the backend instead of silently emitting under
// the wrong label set.
func (d *Describe) Tags(vals ...string) []Tag {
	required := len(d.RequiredTags)
	provided := len(vals)
	if provided != required {
		logger.KV(xlog.ERROR,
			"reason", "invalid_tags",
			"metric", d.Name,
			"required", required,
			"provided", provided,
		)
		return []Tag{{Name: "invalid_tags", Value: fmt.Sprintf("%d", provided)}}
	}
	if required == 0 {
		return nil
	}

	tags := make([]Tag, required)
	for i, val := range vals {
		tags[i] = Tag{
			Name:  d.RequiredTags[i],
			Value: val,
		}
	}
	return tags
}

// SetGauge emits the described gauge on the global instance.
// The values must match Describe.RequiredTags in size and order.
func (d *Describe) SetGauge(val float64, tags ...string) {
	SetGauge(d.Name, val, d.Tags(tags...)...)
}

// IncrCounter emits the described counter increment on the global instance.
// The values must match Describe.RequiredTags in size and order.
func (d *Describe) IncrCounter(val float64, tags ...string) {
	IncrCounter(d.Name, val, d.Tags(tags...)...)
}

// AddSample emits the described sample on the global instance.
// The values must match Describe.RequiredTags in size and order.
func (d *Describe) AddSample(val float64, tags ...string) {
	AddSample(d.Name, val, d.Tags(tags...)...)
}

// MeasureSince emits the time elapsed since start as the described sample
// on the global instance.
// The values must match Describe.RequiredTags in size and order.
func (d *Describe) MeasureSince(start time.Time, tags ...string) {
	MeasureSince(d.Name, start, d.Tags(tags...)...)
}

// Help returns the help text of the described metrics, keyed by the final
// metric name that this configuration produces.
//
// The result is meant for prometheus.Opts.Help, so the exported HELP line
// matches the metric. Metrics blocked by the filters are omitted. The key is
// derived from Describe.Type, so a Describe must use the TypeCounter,
// TypeGauge and TypeSample constants for the key to match the emitted one when
// Config.EnableTypePrefix is set.
func (m *Config) Help(providers ...[]*Describe) map[string]string {
	h := make(map[string]string)

	for _, descs := range providers {
		for _, d := range descs {
			allowed, key, _ := m.Prepare(d.Type, d.Name)
			if allowed {
				h[key] = d.Help
			}
		}
	}
	return h
}
