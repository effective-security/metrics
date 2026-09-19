package metrics

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/effective-security/xlog"
)

var logger = xlog.NewPackageLogger("github.com/effective-security/metrics", "metrics")

// Tag names added by Prepare from the configuration.
const (
	tagHost    = "host"
	tagService = "service"
)

// Config is used to configure metrics settings.
//
// The zero value emits nothing: FilterDefault must be set to true, or an
// explicit AllowedPrefixes list must be provided, for metrics to reach the sink.
// See Prepare for how the fields combine into the final metric key.
//
// A Config is read on every emission. Copy it into a Metrics with New and
// change the filters through Metrics.UpdateFilter afterwards; writing to the
// fields of a Config that is in use is not synchronized.
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
	// (GC, Goroutines, Memory). New starts a background goroutine for it,
	// which runs until Metrics.Close is called.
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
	// A key matching one of them is emitted regardless of FilterDefault.
	AllowedPrefixes []string
	// BlockedPrefixes is a list of metric key prefixes to block.
	// A blocked prefix always wins over an allowed one.
	BlockedPrefixes []string
	// FilterDefault is the verdict for keys that no prefix rule matches:
	// true allows them, false drops them.
	FilterDefault bool
}

// Metrics represents an instance of a metrics sink that can
// be used to emit.
//
// A Metrics value is safe for concurrent emission, and for concurrent
// UpdateFilter. It must not be copied.
type Metrics struct {
	Config

	// filters holds the rules in effect, replaced as a whole by UpdateFilter
	// so emissions never read a half-updated rule set.
	filters atomic.Pointer[metricFilters]

	stopCh   chan struct{}
	stopOnce sync.Once

	lastNumGC uint32
	sink      Sink
}

// metricFilters is an immutable snapshot of the prefix rules.
type metricFilters struct {
	allowed       []string
	blocked       []string
	filterDefault bool
}

// allow reports whether the assembled key passes the rules.
func (f *metricFilters) allow(key string) bool {
	return allowMetric(key, f.allowed, f.blocked, f.filterDefault)
}

// allowMetric implements the filter precedence shared by Config and Metrics:
// a blocked prefix wins over an allowed one, an allowed prefix admits the key,
// and a key that matches no rule falls back to filterDefault.
func allowMetric(key string, allowed, blocked []string, filterDefault bool) bool {
	if len(blocked) > 0 && StringStartsWithOneOf(key, blocked) {
		return false
	}
	if len(allowed) > 0 && StringStartsWithOneOf(key, allowed) {
		return true
	}
	return filterDefault
}

// globalMetrics is the shared instance used by the package-level emit functions.
var globalMetrics atomic.Pointer[Metrics]

func init() {
	// Initialize to a blackhole sink to avoid errors
	globalMetrics.Store(newBlackholeMetrics())
}

// newBlackholeMetrics builds the instance used before NewGlobal is called.
// Its zero Config drops every metric, so nothing reaches the sink either way.
func newBlackholeMetrics() *Metrics {
	met := &Metrics{
		stopCh: make(chan struct{}),
		sink:   &BlackholeSink{},
	}
	met.UpdateFilter(nil, nil)
	return met
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
// The configuration is copied, including its tag and prefix slices, so later
// changes to conf do not affect the returned instance; use Metrics.UpdateFilter
// to change the filters. TimerGranularity and ProfileInterval fall back to
// their defaults when they are not positive. When conf.EnableRuntimeMetrics is
// set, New starts a collector goroutine that Metrics.Close stops. The error is
// always nil and exists for API compatibility.
func New(conf *Config, sink Sink) (*Metrics, error) {
	met := &Metrics{
		stopCh: make(chan struct{}),
		sink:   sink,
	}
	met.Config = *conf
	// the assignment above copies slice headers, which would leave the
	// instance reading, and racing on, memory the caller still owns
	met.GlobalTags = slices.Clone(conf.GlobalTags)
	met.AllowedPrefixes = slices.Clone(conf.AllowedPrefixes)
	met.BlockedPrefixes = slices.Clone(conf.BlockedPrefixes)
	// UpdateFilter takes its own copy
	met.UpdateFilter(conf.AllowedPrefixes, conf.BlockedPrefixes)

	// a non-positive duration is a misconfiguration, not a request for a tight
	// loop: ProfileInterval reaches time.NewTicker, which panics on one
	if met.TimerGranularity <= 0 {
		met.TimerGranularity = time.Millisecond
	}
	if met.ProfileInterval <= 0 {
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
// The instance previously installed as global is not closed; close it first if
// it was collecting runtime metrics.
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
// and the returned tags are the given ones followed by GlobalTags plus the
// optional "host" and "service" labels. The caller's slice is never modified:
// when tags have to be added, or rewritten for NumberLabelPrefix, the result
// is a new slice.
func (m *Config) Prepare(typ string, key string, tags ...Tag) (bool, string, []Tag) {
	key, out := m.prepare(typ, key, tags)
	return m.AllowMetric(key), key, out
}

// prepare assembles the key and the tags, without applying the filters.
func (m *Config) prepare(typ string, key string, tags []Tag) (string, []Tag) {
	hostLabel := m.HostName != "" && m.EnableHostnameLabel
	if m.HostName != "" && !m.EnableHostnameLabel && m.EnableHostname {
		key = m.HostName + "_" + key
	}
	if m.EnableTypePrefix {
		key = typ + "_" + key
	}
	serviceLabel := m.ServiceName != "" && m.EnableServiceLabel
	if m.ServiceName != "" && !m.EnableServiceLabel {
		key = m.ServiceName + "_" + key
	}
	if m.GlobalPrefix != "" {
		key = m.GlobalPrefix + "_" + key
	}

	out := tags
	extra := len(m.GlobalTags)
	if hostLabel {
		extra++
	}
	if serviceLabel {
		extra++
	}
	if extra > 0 {
		out = make([]Tag, 0, len(tags)+extra)
		out = append(out, tags...)
		out = append(out, m.GlobalTags...)
		if hostLabel {
			out = append(out, Tag{Name: tagHost, Value: m.HostName})
		}
		if serviceLabel {
			out = append(out, Tag{Name: tagService, Value: m.ServiceName})
		}
	}

	if m.NumberLabelPrefix != "" {
		// copy on write: out may still be the caller's slice
		out = m.prefixNumbers(out, extra > 0)
	}

	return key, out
}

// prefixNumbers applies NumberLabelPrefix to the tag values that look like
// 64-bit numbers. owned tells whether tags is already a slice this package
// allocated; if it is not, the slice is cloned before the first rewrite.
func (m *Config) prefixNumbers(tags []Tag, owned bool) []Tag {
	for idx, tag := range tags {
		if !isBigNumber(tag.Value) {
			continue
		}
		if !owned {
			tags = slices.Clone(tags)
			owned = true
		}
		tags[idx].Value = m.NumberLabelPrefix + tag.Value
	}
	return tags
}

// isBigNumber reports whether s is an optionally signed decimal integer
// of at least maxNumberlen digits. The sign does not count towards the digits.
func isBigNumber(s string) bool {
	digits := strings.TrimPrefix(s, "-")
	if len(digits) < maxNumberlen {
		return false
	}
	for _, ch := range digits {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// AllowMetric returns whether the metric should be allowed based on the
// configured prefix filters.
//
// A key matching BlockedPrefixes is dropped, a key matching AllowedPrefixes is
// emitted, and a key matching neither falls back to FilterDefault.
func (m *Config) AllowMetric(key string) bool {
	return allowMetric(key, m.AllowedPrefixes, m.BlockedPrefixes, m.FilterDefault)
}

// Metric types, used as the type prefix when Config.EnableTypePrefix is set,
// and as Describe.Type.
const (
	TypeCounter = "counter"
	TypeSample  = "sample"
	TypeGauge   = "gauge"

	// typeSummary is accepted as an alias of TypeSample in Describe.Type,
	// because that is how the Prometheus sink exports samples.
	typeSummary = "summary"
)

// Describe provides metric description.
//
// Declaring metrics as package-level Describe values keeps the name, help text
// and tag names in one place, and validates on every emission that the caller
// supplies exactly the expected tag values. Its emit methods always go to the
// global instance.
type Describe struct {
	// Type of the metric: TypeCounter, TypeGauge or TypeSample.
	// "summary" is accepted as an alias of TypeSample.
	Type string
	// Name is the metric name
	Name string
	// Help provides description
	Help string
	// RequiredTags is a list of metric tags
	RequiredTags []string
}

// emitType returns the canonical type prefix for the metric, so that the key
// built by Config.Help matches the one the emit methods produce.
// An unknown type is reported and used as-is.
func (d *Describe) emitType() string {
	switch d.Type {
	case TypeCounter, TypeGauge, TypeSample:
		return d.Type
	case typeSummary:
		return TypeSample
	default:
		logger.KV(xlog.ERROR,
			"reason", "invalid_type",
			"metric", d.Name,
			"type", d.Type,
		)
		return d.Type
	}
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
		return []Tag{{Name: "invalid_tags", Value: strconv.Itoa(provided)}}
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
// matches the metric. Metrics blocked by the configured filters are omitted;
// Metrics.Help applies the filters in effect instead.
func (m *Config) Help(providers ...[]*Describe) map[string]string {
	return describeHelp(m.Prepare, providers)
}

// prepareFunc is the signature shared by Config.Prepare and Metrics.Prepare.
type prepareFunc func(typ string, key string, tags ...Tag) (bool, string, []Tag)

// describeHelp collects the help text of the allowed metrics, keyed by the name
// prepare assembles for them.
func describeHelp(prepare prepareFunc, providers [][]*Describe) map[string]string {
	h := make(map[string]string)

	for _, descs := range providers {
		for _, d := range descs {
			allowed, key, _ := prepare(d.emitType(), d.Name)
			if allowed {
				h[key] = d.Help
			}
		}
	}
	return h
}
