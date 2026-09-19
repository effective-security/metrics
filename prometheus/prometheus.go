package prometheus

import (
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/cockroachdb/errors"
	"github.com/effective-security/metrics"
	"github.com/effective-security/xlog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	"github.com/prometheus/common/model"
)

var logger = xlog.NewPackageLogger("github.com/effective-security/metrics", "prom")

const (
	// defaultSinkName is used when Opts.Name is empty.
	defaultSinkName = "default_prometheus_sink"
	// defaultExpiration is the expiration used by DefaultPrometheusOpts
	// and NewPushSink.
	defaultExpiration = 60 * time.Second
	// reservedLabelPrefix marks the label names the client library keeps for
	// itself; a series using one has an erroring descriptor.
	reservedLabelPrefix = "__"
)

// DefaultPrometheusOpts is the default set of options used when creating a
// Sink with NewSink.
//
// It is a package-level variable so that it can be adjusted before the sink is
// created; changing it afterwards has no effect.
var DefaultPrometheusOpts = Opts{
	Expiration: defaultExpiration,
	Name:       defaultSinkName,
}

// ObservationMaxAge is the duration for which an observation stays relevant
// for the quantiles of a summary. It does not apply to _sum and _count, and is
// independent of Opts.Expiration.
const ObservationMaxAge = 10 * time.Minute

// summaryObjectives are the quantiles reported for every summary,
// mapped to their allowed rank error.
var summaryObjectives = map[float64]float64{
	0.5:  0.05,
	0.9:  0.01,
	0.99: 0.001,
}

// Opts is used to configure the Prometheus Sink.
type Opts struct {
	// Expiration is the duration a gauge or summary is valid for, after which
	// it will be untracked. If the value is not positive, they are never
	// expired. Counters are controlled by CounterExpiration instead.
	Expiration time.Duration

	// CounterExpiration is the duration after which a counter created at
	// runtime is removed, when it has not been updated. The zero value, which
	// is the default, keeps every counter for the lifetime of the process, as
	// does a negative one.
	//
	// Deleting a counter resets the series, which makes rate() over the gap
	// wrong, so only set this when the label cardinality is unbounded and a
	// reset is the lesser problem.
	CounterExpiration time.Duration

	// Registerer the Sink registers itself with.
	// Defaults to prometheus.DefaultRegisterer.
	Registerer prometheus.Registerer

	// GaugeDefinitions pre-declares gauges by Name, Help and ConstTags.
	// Metrics declared in this way will be initialized at zero and will not be
	// deleted or altered when their expiry is reached.
	//
	// Ex: Opts{
	//     Expiration: 10 * time.Second,
	//     GaugeDefinitions: []GaugeDefinition{
	//         {
	//           Name: "application_component_measurement",
	//           Help: "application_component_measurement provides an example of how to declare static metrics",
	//           ConstTags: []metrics.Tag{ { Name: "my_label", Value: "does_not_change" } },
	//         },
	//     },
	// }
	GaugeDefinitions []GaugeDefinition
	// SummaryDefinitions pre-declares summaries, see GaugeDefinitions.
	SummaryDefinitions []SummaryDefinition
	// CounterDefinitions pre-declares counters, see GaugeDefinitions.
	CounterDefinitions []CounterDefinition

	// Name of the sink, used as the descriptor exposed by Describe.
	// Two sinks registered with the same Registerer must have different names.
	Name string

	// Help of the metrics, keyed by the final metric name.
	// Use metrics.Config.Help to build it from the metric descriptions, so the
	// exported HELP text matches the name the configuration produces.
	// The map is retained and written to by the pre-declared definitions; it
	// must not be modified once the sink is constructed.
	Help map[string]string
}

// Sink provides a metrics.Sink that can be used
// with a prometheus server.
//
// It implements prometheus.Collector: it is registered on construction and
// scraped through a prometheus registry. Series are created on first use and,
// unless pre-declared, expired after Opts.Expiration without an update.
// It is safe for concurrent emission and concurrent scrapes.
type Sink struct {
	// If these will ever be copied, they should be converted to *sync.Map values and initialized appropriately
	gauges            sync.Map
	summaries         sync.Map
	counters          sync.Map
	expiration        time.Duration
	counterExpiration time.Duration
	help              map[string]string
	name              string
}

// tombstone marks an entry the collector has taken out of the sink. It is out
// of the range of any real timestamp, so it can never be mistaken for one.
const tombstone = math.MinInt64

// lastUpdate records when a metric was last written to, and whether it is still
// part of the sink.
//
// It is mutated in place, so an emission does not have to copy the metric and
// replace the map entry. Concurrent updates may order arbitrarily, which is
// harmless: every one of them stores approximately the current time. The
// tombstone closes the window between the collector deciding that a series has
// expired and removing it, in which an emission would otherwise update a metric
// that is about to be dropped, losing the value.
type lastUpdate struct {
	unixNano atomic.Int64
}

// touch records t as the time of the last update, and reports whether the entry
// is still live. It fails when the collector has retired the entry, in which
// case the value the caller applied landed on a metric nothing collects and has
// to be applied again to a new one.
//
// It is the commit step of an emission, and runs after the metric itself is
// updated: an emission that is retired mid-flight is then detected here rather
// than silently lost.
//
// The stored time only moves forward. A slow emission must not write its own,
// older timestamp over a newer one, which would make a just-updated series look
// idle to the collector and expire it early.
//
// Every successful touch still changes the stored value, so a retire that was
// decided on the value read before it always fails. Together with the ordering
// above that leaves one way for an emission not to be exported: the series is
// retired on a timestamp this very emission wrote, which means it stayed idle
// for a full expiration afterwards, so it expired before anything scraped it.
func (l *lastUpdate) touch(t time.Time) bool {
	next := t.UnixNano()
	for {
		current := l.unixNano.Load()
		if current == tombstone {
			return false
		}
		if current >= next {
			// a concurrent emission recorded a later time; move by the
			// smallest step instead, rather than backwards
			next = current + 1
		}
		if l.unixNano.CompareAndSwap(current, next) {
			return true
		}
		next = t.UnixNano()
	}
}

// idleSince returns the stored timestamp and whether the metric has been idle
// for expiration at time t. A retired entry reports the tombstone and is
// always idle.
//
// The comparison subtracts the timestamps instead of adding the expiration to
// one of them: a valid expiration can be large enough for the sum to overflow,
// which would make every series look idle.
func (l *lastUpdate) idleSince(expiration time.Duration, t time.Time) (int64, bool) {
	seen := l.unixNano.Load()
	if seen == tombstone {
		return seen, true
	}
	return seen, time.Duration(t.UnixNano()-seen) > expiration
}

// retire marks the entry as removed, unless it was updated since idleSince
// reported it at seen. A failed retire means an emission got there first and
// the entry must be kept.
func (l *lastUpdate) retire(seen int64) bool {
	return l.unixNano.CompareAndSwap(seen, tombstone)
}

// GaugeDefinition can be provided to Opts to declare a constant gauge that is not deleted on expiry.
type GaugeDefinition struct {
	Name      string
	ConstTags []metrics.Tag
	Help      string
}

type gauge struct {
	prometheus.Gauge
	lastUpdate
	// canDelete is set if the metric is created during runtime so we know it's ephemeral and can delete it on expiry.
	canDelete bool
}

// SummaryDefinition can be provided to Opts to declare a constant summary that is not deleted on expiry.
type SummaryDefinition struct {
	Name      string
	ConstTags []metrics.Tag
	Help      string
}

type summary struct {
	prometheus.Summary
	lastUpdate
	canDelete bool
}

// CounterDefinition can be provided to Opts to declare a constant counter that is not deleted on expiry.
type CounterDefinition struct {
	Name      string
	ConstTags []metrics.Tag
	Help      string
}

type counter struct {
	prometheus.Counter
	lastUpdate
	canDelete bool
}

// NewSink creates a new Sink using DefaultPrometheusOpts.
func NewSink() (*Sink, error) {
	return NewSinkFrom(DefaultPrometheusOpts)
}

// NewSinkFrom creates a new Sink using the passed options and registers it
// with opts.Registerer, or with prometheus.DefaultRegisterer when unset.
//
// It returns an error, and no sink, when a collector with the same name is
// already registered with that registerer. The cause is preserved, so
// errors.As with prometheus.AlreadyRegisteredError still works.
func NewSinkFrom(opts Opts) (*Sink, error) {
	sink := newSink(opts)

	reg := opts.Registerer
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	if err := reg.Register(sink); err != nil {
		return nil, errors.WithMessage(err, "unable to register sink")
	}
	return sink, nil
}

// newSink builds the sink and its pre-declared series, without registering it.
func newSink(opts Opts) *Sink {
	name := opts.Name
	if name == "" {
		name = defaultSinkName
	}
	sink := &Sink{
		gauges:            sync.Map{},
		summaries:         sync.Map{},
		counters:          sync.Map{},
		expiration:        opts.Expiration,
		counterExpiration: opts.CounterExpiration,
		help:              opts.Help,
		name:              name,
	}
	if sink.help == nil {
		sink.help = make(map[string]string)
	}

	initGauges(&sink.gauges, opts.GaugeDefinitions, sink.help)
	initSummaries(&sink.summaries, opts.SummaryDefinitions, sink.help)
	initCounters(&sink.counters, opts.CounterDefinitions, sink.help)

	return sink
}

// Describe sends a Collector.Describe value from the descriptor created around Sink.Name
// Note that we cannot describe all the metrics (gauges, counters, summaries) in the sink as
// metrics can be added at any point during the lifecycle of the sink, which does not respect
// the idempotency aspect of the Collector.Describe() interface
func (p *Sink) Describe(c chan<- *prometheus.Desc) {
	// dummy value to be able to register and unregister "empty" sinks
	// Note this is not actually retained in the Sink so this has no side effects
	// on the caller's sink. So it shouldn't show up to any of its consumers.
	prometheus.NewGauge(prometheus.GaugeOpts{Name: p.name, Help: p.name}).Describe(c)
}

// Collect meets the collection interface and allows us to enforce our expiration
// logic to clean up ephemeral metrics if their value haven't been set for a
// duration exceeding our allowed expiration time.
func (p *Sink) Collect(c chan<- prometheus.Metric) {
	p.collectAtTime(c, time.Now())
}

// collectAtTime allows internal testing of the expiry based logic here without
// mocking clocks or making tests timing sensitive.
func (p *Sink) collectAtTime(c chan<- prometheus.Metric, t time.Time) {
	// a negative expiration is a misconfiguration; treat it like the zero
	// value, which means "never expire", instead of dropping every series on
	// the first scrape
	expire := p.expiration > 0
	expireCounters := p.counterExpiration > 0
	deleted := 0
	p.gauges.Range(func(k, v any) bool {
		g := v.(*gauge)
		if expire && g.canDelete && retire(&p.gauges, k, v, &g.lastUpdate, p.expiration, t) {
			deleted++
			return true
		}
		g.Collect(c)
		return true
	})
	p.summaries.Range(func(k, v any) bool {
		s := v.(*summary)
		if expire && s.canDelete && retire(&p.summaries, k, v, &s.lastUpdate, p.expiration, t) {
			deleted++
			return true
		}
		s.Collect(c)
		return true
	})
	p.counters.Range(func(k, v any) bool {
		count := v.(*counter)
		// Counters are kept unless Opts.CounterExpiration asks otherwise:
		// removing one resets the series and breaks rate() over the gap.
		if expireCounters && count.canDelete && retire(&p.counters, k, v, &count.lastUpdate, p.counterExpiration, t) {
			deleted++
			return true
		}
		count.Collect(c)
		return true
	})
	if deleted > 0 {
		logger.KV(xlog.DEBUG, "deleted_expired", deleted)
	}
}

// retire removes an expired entry from m, and reports whether the entry is
// gone and must not be collected.
//
// The removal is conditional twice over: the entry is tombstoned only if it has
// not been updated since it was read as idle, and it is deleted from the map
// only if it is still the entry that was tombstoned. An emission that raced
// with either step sees the tombstone and registers a fresh series instead of
// updating a metric nothing collects.
//
// An entry that is already tombstoned was retired by a concurrent scrape, or is
// about to be removed by the emission that found it retired; it is skipped
// without being counted as a retirement of this scrape.
func retire(m *sync.Map, key, value any, updated *lastUpdate, expiration time.Duration, t time.Time) bool {
	seen, idle := updated.idleSince(expiration, t)
	if !idle {
		return false
	}
	if seen != tombstone && !updated.retire(seen) {
		return false
	}
	m.CompareAndDelete(key, value)
	return true
}

// initGauges pre-declares the gauges and records their help text.
func initGauges(m *sync.Map, gauges []GaugeDefinition, help map[string]string) {
	for _, g := range gauges {
		key, hash := flattenKey(g.Name, g.ConstTags)
		help[key] = g.Help
		pG := prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        key,
			Help:        g.Help,
			ConstLabels: prometheusLabels(g.ConstTags),
		})
		m.Store(hash, &gauge{Gauge: pG})
	}
}

// initSummaries pre-declares the summaries and records their help text.
func initSummaries(m *sync.Map, summaries []SummaryDefinition, help map[string]string) {
	for _, s := range summaries {
		key, hash := flattenKey(s.Name, s.ConstTags)
		help[key] = s.Help
		pS := prometheus.NewSummary(prometheus.SummaryOpts{
			Name:        key,
			Help:        s.Help,
			MaxAge:      ObservationMaxAge,
			ConstLabels: prometheusLabels(s.ConstTags),
			Objectives:  summaryObjectives,
		})
		m.Store(hash, &summary{Summary: pS})
	}
}

// initCounters pre-declares the counters and records their help text.
func initCounters(m *sync.Map, counters []CounterDefinition, help map[string]string) {
	for _, c := range counters {
		key, hash := flattenKey(c.Name, c.ConstTags)
		help[key] = c.Help
		pC := prometheus.NewCounter(prometheus.CounterOpts{
			Name:        key,
			Help:        c.Help,
			ConstLabels: prometheusLabels(c.ConstTags),
		})
		m.Store(hash, &counter{Counter: pC})
	}
}

// forbiddenCharsReplacer maps the characters that are not valid in a
// Prometheus metric name onto underscores.
var forbiddenCharsReplacer = strings.NewReplacer(" ", "_", ".", "_", "=", "_", "-", "_", "/", "_")

// labelKeySizeHint is the assumed size of one ";name=value" pair,
// used to pre-size the hash buffer.
const labelKeySizeHint = 24

// flattenKey returns the sanitized metric name and the hash that identifies the
// series: the name with its labels appended. Label names are not sanitized.
func flattenKey(parts string, labels []metrics.Tag) (string, string) {
	key := forbiddenCharsReplacer.Replace(parts)
	if len(labels) == 0 {
		return key, key
	}

	buf := new(strings.Builder)
	buf.Grow(len(key) + len(labels)*labelKeySizeHint)
	buf.WriteString(key)
	for _, label := range labels {
		buf.WriteByte(';')
		buf.WriteString(label.Name)
		buf.WriteByte('=')
		buf.WriteString(label.Value)
	}

	return key, buf.String()
}

// validSeries reports whether the client library accepts the metric name and
// the labels, applying the same rules as prometheus.NewDesc, and logs the
// rejected series.
//
// A rejected name does not fail at construction: the metric gets an erroring
// descriptor, and one such series fails the whole scrape with HTTP 500 until it
// expires. Dropping the emission keeps the endpoint serving the rest.
// It runs only when a series is created; an existing entry was validated then.
func validSeries(key string, labels []metrics.Tag) bool {
	reason := ""
	switch {
	case !model.UTF8Validation.IsValidMetricName(key):
		reason = "invalid_metric_name"
	default:
		for _, label := range labels {
			if !model.UTF8Validation.IsValidLabelName(label.Name) ||
				strings.HasPrefix(label.Name, reservedLabelPrefix) {
				reason = "invalid_label_name"
				break
			}
			if !utf8.ValidString(label.Value) {
				reason = "invalid_label_value"
				break
			}
		}
	}
	if reason == "" {
		return true
	}
	logger.KV(xlog.ERROR,
		"reason", reason,
		"metric", key,
		"labels", labels,
	)
	return false
}

// prometheusLabels converts the tags into the const labels of a metric.
func prometheusLabels(labels []metrics.Tag) prometheus.Labels {
	l := make(prometheus.Labels, len(labels))
	for _, label := range labels {
		l[label.Name] = label.Value
	}
	return l
}

// helpFor returns the configured help text of the metric, or the key itself.
func (p *Sink) helpFor(key string) string {
	if h, ok := p.help[key]; ok {
		return h
	}
	return key
}

// The emit methods retry until the value is committed to a live series. Every
// turn of the loop that does not return is caused by another goroutine having
// completed its own step, so the loop cannot spin without the sink making
// progress: either the collector retired the entry, which the emission then
// removes itself before trying again, or a concurrent emission published the
// series first, which the next turn finds and updates. A bounded loop would
// instead have to drop the value when the bound is reached, silently.

// SetGauge retains the last value it is set to.
// The gauge is created on first use and expires after Opts.Expiration.
// An emission the client library would reject is dropped and reported.
func (p *Sink) SetGauge(parts string, val float64, labels []metrics.Tag) {
	key, hash := flattenKey(parts, labels)

	for {
		if v, ok := p.gauges.Load(hash); ok {
			g := v.(*gauge)
			g.Set(val)
			if g.touch(time.Now()) {
				return
			}
			// the collector retired the series while the value was applied, so
			// it landed on a gauge nothing collects: drop the entry and set it
			// on a new one
			p.gauges.CompareAndDelete(hash, v)
			continue
		}

		if !validSeries(key, labels) {
			return
		}

		// The gauge does not exist, create it and allow it to be deleted
		pg := &gauge{
			Gauge: prometheus.NewGauge(prometheus.GaugeOpts{
				Name:        key,
				Help:        p.helpFor(key),
				ConstLabels: prometheusLabels(labels),
			}),
			canDelete: true,
		}
		pg.Set(val)
		pg.touch(time.Now())

		// publish only once the value is in, so the collector never sees the
		// series without it; a concurrent emission may have created it first,
		// in which case the value belongs on that one instead
		if _, loaded := p.gauges.LoadOrStore(hash, pg); !loaded {
			return
		}
	}
}

// AddSample records an observation in a summary,
// exported as the configured quantiles plus _sum and _count.
// The summary is created on first use and expires after Opts.Expiration.
// An emission the client library would reject is dropped and reported.
func (p *Sink) AddSample(parts string, val float64, labels []metrics.Tag) {
	key, hash := flattenKey(parts, labels)

	for {
		if v, ok := p.summaries.Load(hash); ok {
			s := v.(*summary)
			s.Observe(val)
			if s.touch(time.Now()) {
				return
			}
			// the collector retired the series while the observation was
			// applied, so it landed on a summary nothing collects: drop the
			// entry and observe on a new one
			p.summaries.CompareAndDelete(hash, v)
			continue
		}

		if !validSeries(key, labels) {
			return
		}

		// The summary does not exist, create it and allow it to be deleted
		ps := &summary{
			Summary: prometheus.NewSummary(prometheus.SummaryOpts{
				Name:        key,
				Help:        p.helpFor(key),
				MaxAge:      ObservationMaxAge,
				ConstLabels: prometheusLabels(labels),
				Objectives:  summaryObjectives,
			}),
			canDelete: true,
		}
		ps.Observe(val)
		ps.touch(time.Now())

		// publish only once the observation is in; the summary created here is
		// discarded when another emission won the race, and the next turn
		// observes on the registered one
		if _, loaded := p.summaries.LoadOrStore(hash, ps); !loaded {
			return
		}
	}
}

// EmitKey is not implemented. Prometheus doesn’t offer a type for which an
// arbitrary number of values is retained, as Prometheus works with a pull
// model, rather than a push model.

// IncrCounter accumulates values.
// The counter is created on first use and, unless Opts.CounterExpiration is
// set, is retained for the lifetime of the process.
// An emission the client library would reject is dropped and reported.
func (p *Sink) IncrCounter(parts string, val float64, labels []metrics.Tag) {
	key, hash := flattenKey(parts, labels)

	for {
		if v, ok := p.counters.Load(hash); ok {
			c := v.(*counter)
			c.Add(val)
			if c.touch(time.Now()) {
				return
			}
			// the collector retired the series while the increment was applied,
			// so it landed on a counter nothing collects: drop the entry and
			// add it to a new one
			p.counters.CompareAndDelete(hash, v)
			continue
		}

		if !validSeries(key, labels) {
			return
		}

		// The counter does not exist yet, create it
		pc := &counter{
			Counter: prometheus.NewCounter(prometheus.CounterOpts{
				Name:        key,
				Help:        p.helpFor(key),
				ConstLabels: prometheusLabels(labels),
			}),
			canDelete: true,
		}
		pc.Add(val)
		pc.touch(time.Now())

		// publish only once the increment is in; the counter created here is
		// discarded when another emission won the race, and the next turn adds
		// to the registered one
		if _, loaded := p.counters.LoadOrStore(hash, pc); !loaded {
			return
		}
	}
}

// PushSink wraps a normal prometheus sink and provides an address and facilities to export it to an address
// on an interval.
//
// Use it for processes that Prometheus cannot scrape, such as batch jobs, by
// pushing to a Pushgateway.
type PushSink struct {
	*Sink
	pusher       *push.Pusher
	address      string
	pushInterval time.Duration
	stopChan     chan struct{}
	doneChan     chan struct{}
	stopOnce     sync.Once
}

// NewPushSink creates a PushSink by taking an address, interval, and destination name,
// and starts the background push loop.
//
// The wrapped Sink is not registered with any prometheus.Registerer and uses a
// 60s expiration. Use NewPushSinkFrom to configure it. Call Shutdown to stop
// pushing. pushInterval must be greater than zero.
func NewPushSink(address string, pushInterval time.Duration, name string) (*PushSink, error) {
	return NewPushSinkFrom(address, pushInterval, name, Opts{
		Expiration: defaultExpiration,
		Name:       defaultSinkName,
	})
}

// NewPushSinkFrom creates a PushSink with the given options and starts the
// background push loop. name is the job name reported to the Pushgateway.
//
// The wrapped Sink is registered only when opts.Registerer is set, since a
// pushed sink is usually not scraped as well. pushInterval must be greater
// than zero.
func NewPushSinkFrom(address string, pushInterval time.Duration, name string, opts Opts) (*PushSink, error) {
	if pushInterval <= 0 {
		return nil, errors.Errorf("invalid pushInterval: must be positive, got %s", pushInterval)
	}

	promSink := newSink(opts)
	if opts.Registerer != nil {
		if err := opts.Registerer.Register(promSink); err != nil {
			return nil, errors.WithMessage(err, "unable to register sink")
		}
	}

	sink := &PushSink{
		Sink:         promSink,
		pusher:       push.New(address, name).Collector(promSink),
		address:      address,
		pushInterval: pushInterval,
		stopChan:     make(chan struct{}),
		doneChan:     make(chan struct{}),
	}

	sink.flushMetrics()
	return sink, nil
}

// flushMetrics starts the goroutine that pushes on the configured interval.
// It closes doneChan on the way out, so Shutdown can wait for it.
func (s *PushSink) flushMetrics() {
	ticker := time.NewTicker(s.pushInterval)

	go func() {
		defer close(s.doneChan)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.push()
			case <-s.stopChan:
				return
			}
		}
	}()
}

// push sends the sink to the Pushgateway and logs a failure. There is nothing
// to return it to: the loop and Shutdown carry on regardless.
func (s *PushSink) push() {
	if err := s.pusher.Push(); err != nil {
		logger.KV(xlog.ERROR, "reason", "push", "address", s.address, "err", err.Error())
	}
}

// Shutdown tears down the PushSink, and blocks while flushing metrics to the backend.
// It is idempotent, and a concurrent call returns once the teardown is complete.
//
// It waits for the background loop to finish before the final push: a push
// already in flight must not run concurrently with it, since the two would
// share one push.Pusher.
func (s *PushSink) Shutdown() {
	s.stopOnce.Do(func() {
		close(s.stopChan)
		<-s.doneChan
		// Stopping the loop does not publish what was emitted since its last
		// push, so the sink is pushed one last time.
		s.push()
	})
}
