package prometheus

import (
	"strings"
	"sync"
	"time"

	"github.com/effective-security/metrics"
	"github.com/effective-security/xlog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
)

var logger = xlog.NewPackageLogger("github.com/effective-security/metrics", "prom")

// defaultSinkName is used when Opts.Name is empty.
const defaultSinkName = "default_prometheus_sink"

// DefaultPrometheusOpts is the default set of options used when creating a
// Sink with NewSink.
//
// It is a package-level variable so that it can be adjusted before the sink is
// created; changing it afterwards has no effect.
var DefaultPrometheusOpts = Opts{
	Expiration: 60 * time.Second,
	Name:       defaultSinkName,
}

// ObservationMaxAge defines the duration for which an observation stays relevant
// for the summary. Only applies to pre-calculated quantiles, does not
// apply to _sum and _count. Must be positive. The default value is
// DefMaxAge.
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
	// Expiration is the duration a metric is valid for, after which it will be
	// untracked. If the value is zero, a metric is never expired.
	// Counters are never expired, regardless of this setting.
	Expiration time.Duration

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
	// The map is retained and written to by the pre-declared definitions.
	Help map[string]string
}

// Sink provides a metrics.Sink that can be used
// with a prometheus server.
//
// It implements prometheus.Collector: it is registered on construction and
// scraped through a prometheus registry. Series are created on first use and,
// unless pre-declared, expired after Opts.Expiration without an update.
type Sink struct {
	// If these will ever be copied, they should be converted to *sync.Map values and initialized appropriately
	gauges     sync.Map
	summaries  sync.Map
	counters   sync.Map
	expiration time.Duration
	help       map[string]string
	name       string
}

// GaugeDefinition can be provided to Opts to declare a constant gauge that is not deleted on expiry.
type GaugeDefinition struct {
	Name      string
	ConstTags []metrics.Tag
	Help      string
}

type gauge struct {
	prometheus.Gauge
	updatedAt time.Time
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
	updatedAt time.Time
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
	updatedAt time.Time
}

// NewSink creates a new Sink using DefaultPrometheusOpts.
func NewSink() (*Sink, error) {
	return NewSinkFrom(DefaultPrometheusOpts)
}

// NewSinkFrom creates a new Sink using the passed options and registers it
// with opts.Registerer, or with prometheus.DefaultRegisterer when unset.
//
// It returns an error when a collector with the same name is already
// registered with that registerer.
func NewSinkFrom(opts Opts) (*Sink, error) {
	name := opts.Name
	if name == "" {
		name = defaultSinkName
	}
	sink := &Sink{
		gauges:     sync.Map{},
		summaries:  sync.Map{},
		counters:   sync.Map{},
		expiration: opts.Expiration,
		help:       opts.Help,
		name:       name,
	}
	if sink.help == nil {
		sink.help = make(map[string]string)
	}

	initGauges(&sink.gauges, opts.GaugeDefinitions, sink.help)
	initSummaries(&sink.summaries, opts.SummaryDefinitions, sink.help)
	initCounters(&sink.counters, opts.CounterDefinitions, sink.help)

	reg := opts.Registerer
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	return sink, reg.Register(sink)
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
	expire := p.expiration != 0
	deleted := 0
	p.gauges.Range(func(k, v any) bool {
		if v == nil {
			return true
		}
		g := v.(*gauge)
		lastUpdate := g.updatedAt
		if expire && lastUpdate.Add(p.expiration).Before(t) {
			if g.canDelete {
				p.gauges.Delete(k)
				deleted++
				return true
			}
		}
		g.Collect(c)
		return true
	})
	p.summaries.Range(func(k, v any) bool {
		if v == nil {
			return true
		}
		s := v.(*summary)
		lastUpdate := s.updatedAt
		if expire && lastUpdate.Add(p.expiration).Before(t) {
			if s.canDelete {
				p.summaries.Delete(k)
				deleted++
				return true
			}
		}
		s.Collect(c)
		return true
	})
	p.counters.Range(func(_, v any) bool {
		if v == nil {
			return true
		}
		count := v.(*counter)
		// Counters are never deleted: removing one would reset the series and
		// break rate() over the scrape gap.
		count.Collect(c)
		return true
	})
	if deleted > 0 {
		logger.KV(xlog.DEBUG, "deleted_expired", deleted)
	}
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

// SetGauge retains the last value it is set to.
// The gauge is created on first use and expires after Opts.Expiration.
func (p *Sink) SetGauge(parts string, val float64, labels []metrics.Tag) {
	key, hash := flattenKey(parts, labels)
	pg, ok := p.gauges.Load(hash)

	// The sync.Map underlying gauges stores pointers to our structs. If we need to make updates,
	// rather than modifying the underlying value directly, which would be racy, we make a local
	// copy by dereferencing the pointer we get back, making the appropriate changes, and then
	// storing a pointer to our local copy. The underlying Prometheus types are threadsafe,
	// so there's no issues there. It's possible for racy updates to occur to the updatedAt
	// value, but since we're always setting it to time.Now(), it doesn't really matter.
	if ok {
		localGauge := *pg.(*gauge)
		localGauge.Set(val)
		localGauge.updatedAt = time.Now()
		p.gauges.Store(hash, &localGauge)

		// The gauge does not exist, create the gauge and allow it to be deleted
	} else {
		g := prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        key,
			Help:        p.helpFor(key),
			ConstLabels: prometheusLabels(labels),
		})
		g.Set(val)
		pg = &gauge{
			Gauge:     g,
			updatedAt: time.Now(),
			canDelete: true,
		}
		p.gauges.Store(hash, pg)
	}
}

// AddSample records an observation in a summary,
// exported as the configured quantiles plus _sum and _count.
// The summary is created on first use and expires after Opts.Expiration.
func (p *Sink) AddSample(parts string, val float64, labels []metrics.Tag) {
	key, hash := flattenKey(parts, labels)
	ps, ok := p.summaries.Load(hash)

	// Does the summary already exist for this sample type?
	if ok {
		localSummary := *ps.(*summary)
		localSummary.Observe(val)
		localSummary.updatedAt = time.Now()
		p.summaries.Store(hash, &localSummary)

		// The summary does not exist, create the Summary and allow it to be deleted
	} else {
		s := prometheus.NewSummary(prometheus.SummaryOpts{
			Name:        key,
			Help:        p.helpFor(key),
			MaxAge:      ObservationMaxAge,
			ConstLabels: prometheusLabels(labels),
			Objectives:  summaryObjectives,
		})
		s.Observe(val)
		ps = &summary{
			Summary:   s,
			updatedAt: time.Now(),
			canDelete: true,
		}
		p.summaries.Store(hash, ps)
	}
}

// EmitKey is not implemented. Prometheus doesn’t offer a type for which an
// arbitrary number of values is retained, as Prometheus works with a pull
// model, rather than a push model.

// IncrCounter accumulates values.
// The counter is created on first use and is never expired, so every tag
// combination emitted is retained for the lifetime of the process.
func (p *Sink) IncrCounter(parts string, val float64, labels []metrics.Tag) {
	key, hash := flattenKey(parts, labels)
	pc, ok := p.counters.Load(hash)

	// Does the counter exist?
	if ok {
		localCounter := *pc.(*counter)
		localCounter.Add(val)
		localCounter.updatedAt = time.Now()
		p.counters.Store(hash, &localCounter)

		// The counter does not exist yet, create it
	} else {
		c := prometheus.NewCounter(prometheus.CounterOpts{
			Name:        key,
			Help:        p.helpFor(key),
			ConstLabels: prometheusLabels(labels),
		})
		c.Add(val)
		pc = &counter{
			Counter:   c,
			updatedAt: time.Now(),
		}
		p.counters.Store(hash, pc)
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
}

// NewPushSink creates a PushSink by taking an address, interval, and destination name,
// and starts the background push loop.
//
// The wrapped Sink is not registered with any prometheus.Registerer and uses a
// fixed 60s expiration, so the options of NewSinkFrom do not apply. Call
// Shutdown to stop pushing. The error is always nil and exists for API
// compatibility. pushInterval must be greater than zero.
func NewPushSink(address string, pushInterval time.Duration, name string) (*PushSink, error) {
	promSink := &Sink{
		gauges:     sync.Map{},
		summaries:  sync.Map{},
		counters:   sync.Map{},
		expiration: 60 * time.Second,
		name:       defaultSinkName,
	}

	pusher := push.New(address, name).Collector(promSink)

	sink := &PushSink{
		promSink,
		pusher,
		address,
		pushInterval,
		make(chan struct{}),
	}

	sink.flushMetrics()
	return sink, nil
}

// flushMetrics starts the goroutine that pushes on the configured interval.
func (s *PushSink) flushMetrics() {
	ticker := time.NewTicker(s.pushInterval)

	go func() {
		for {
			select {
			case <-ticker.C:
				err := s.pusher.Push()
				if err != nil {
					logger.KV(xlog.ERROR, "reason", "push", "address", s.address, "err", err.Error())
				}
			case <-s.stopChan:
				ticker.Stop()
				return
			}
		}
	}()
}

// Shutdown tears down the PushSink, and blocks while flushing metrics to the backend.
// It must be called at most once.
func (s *PushSink) Shutdown() {
	close(s.stopChan)
	// Closing the channel only stops the running goroutine that pushes metrics.
	// To minimize the chance of data loss pusher.Push is called one last time.
	_ = s.pusher.Push()
}
