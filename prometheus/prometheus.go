package prometheus

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/effective-security/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
)

var (
	// DefaultPrometheusOpts is the default set of options used when creating a
	// Sink.
	DefaultPrometheusOpts = Opts{
		Expiration: 60 * time.Second,
		Name:       "default_prometheus_sink",
	}
)

// ObservationMaxAge defines the duration for which an observation stays relevant
// for the summary. Only applies to pre-calculated quantiles, does not
// apply to _sum and _count. Must be positive. The default value is
// DefMaxAge.
const ObservationMaxAge = 10 * time.Minute

// Opts is used to configure the Prometheus Sink
type Opts struct {
	// Expiration is the duration a metric is valid for, after which it will be
	// untracked. If the value is zero, a metric is never expired.
	Expiration time.Duration
	Registerer prometheus.Registerer

	// Gauges, Summaries, and Counters allow us to pre-declare metrics by giving
	// their Name, Help, and ConstLabels to the Sink when it is created.
	// Metrics declared in this way will be initialized at zero and will not be
	// deleted or altered when their expiry is reached.
	//
	// Ex: PrometheusOpts{
	//     Expiration: 10 * time.Second,
	//     Gauges: []GaugeDefinition{
	//         {
	//           Name: []string{ "application", "component", "measurement"},
	//           Help: "application_component_measurement provides an example of how to declare static metrics",
	//           ConstLabels: []metrics.Label{ { Name: "my_label", Value: "does_not_change" }, },
	//         },
	//     },
	// }
	GaugeDefinitions   []GaugeDefinition
	SummaryDefinitions []SummaryDefinition
	CounterDefinitions []CounterDefinition
	Name               string
}

// Sink provides a MetricSink that can be used
// with a prometheus server.
type Sink struct {
	// If these will ever be copied, they should be converted to *sync.Map values and initialized appropriately
	gauges     sync.Map
	summaries  sync.Map
	counters   sync.Map
	expiration time.Duration
	help       map[string]string
	name       string
}

// GaugeDefinition can be provided to PrometheusOpts to declare a constant gauge that is not deleted on expiry.
type GaugeDefinition struct {
	Name      []string
	ConstTags []metrics.Tag
	Help      string
}

type gauge struct {
	prometheus.Gauge
	updatedAt time.Time
	// canDelete is set if the metric is created during runtime so we know it's ephemeral and can delete it on expiry.
	canDelete bool
}

// SummaryDefinition can be provided to PrometheusOpts to declare a constant summary that is not deleted on expiry.
type SummaryDefinition struct {
	Name      []string
	ConstTags []metrics.Tag
	Help      string
}

type summary struct {
	prometheus.Summary
	updatedAt time.Time
	canDelete bool
}

// CounterDefinition can be provided to PrometheusOpts to declare a constant counter that is not deleted on expiry.
type CounterDefinition struct {
	Name      []string
	ConstTags []metrics.Tag
	Help      string
}

type counter struct {
	prometheus.Counter
	updatedAt time.Time
	canDelete bool
}

// NewSink creates a new Sink using the default options.
func NewSink() (*Sink, error) {
	return NewSinkFrom(DefaultPrometheusOpts)
}

// NewSinkFrom creates a new Sink using the passed options.
func NewSinkFrom(opts Opts) (*Sink, error) {
	name := opts.Name
	if name == "" {
		name = "default_prometheus_sink"
	}
	sink := &Sink{
		gauges:     sync.Map{},
		summaries:  sync.Map{},
		counters:   sync.Map{},
		expiration: opts.Expiration,
		help:       make(map[string]string),
		name:       name,
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
	p.gauges.Range(func(k, v interface{}) bool {
		if v == nil {
			return true
		}
		g := v.(*gauge)
		lastUpdate := g.updatedAt
		if expire && lastUpdate.Add(p.expiration).Before(t) {
			if g.canDelete {
				p.gauges.Delete(k)
				return true
			}
		}
		g.Collect(c)
		return true
	})
	p.summaries.Range(func(k, v interface{}) bool {
		if v == nil {
			return true
		}
		s := v.(*summary)
		lastUpdate := s.updatedAt
		if expire && lastUpdate.Add(p.expiration).Before(t) {
			if s.canDelete {
				p.summaries.Delete(k)
				return true
			}
		}
		s.Collect(c)
		return true
	})
	p.counters.Range(func(k, v interface{}) bool {
		if v == nil {
			return true
		}
		count := v.(*counter)
		lastUpdate := count.updatedAt
		if expire && lastUpdate.Add(p.expiration).Before(t) {
			if count.canDelete {
				p.counters.Delete(k)
				return true
			}
		}
		count.Collect(c)
		return true
	})
}

func initGauges(m *sync.Map, gauges []GaugeDefinition, help map[string]string) {
	for _, g := range gauges {
		key, hash := flattenKey(g.Name, g.ConstTags)
		help["gauge."+key] = g.Help
		pG := prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        key,
			Help:        g.Help,
			ConstLabels: prometheusLabels(g.ConstTags),
		})
		m.Store(hash, &gauge{Gauge: pG})
	}
}

func initSummaries(m *sync.Map, summaries []SummaryDefinition, help map[string]string) {
	for _, s := range summaries {
		key, hash := flattenKey(s.Name, s.ConstTags)
		help["summary."+key] = s.Help
		pS := prometheus.NewSummary(prometheus.SummaryOpts{
			Name:        key,
			Help:        s.Help,
			MaxAge:      ObservationMaxAge,
			ConstLabels: prometheusLabels(s.ConstTags),
			Objectives:  map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		})
		m.Store(hash, &summary{Summary: pS})
	}
}

func initCounters(m *sync.Map, counters []CounterDefinition, help map[string]string) {
	for _, c := range counters {
		key, hash := flattenKey(c.Name, c.ConstTags)
		help["counter."+key] = c.Help
		pC := prometheus.NewCounter(prometheus.CounterOpts{
			Name:        key,
			Help:        c.Help,
			ConstLabels: prometheusLabels(c.ConstTags),
		})
		m.Store(hash, &counter{Counter: pC})
	}
}

var forbiddenCharsReplacer = strings.NewReplacer(" ", "_", ".", "_", "=", "_", "-", "_", "/", "_")

func flattenKey(parts []string, labels []metrics.Tag) (string, string) {
	key := strings.Join(parts, "_")
	key = forbiddenCharsReplacer.Replace(key)

	hash := key
	for _, label := range labels {
		hash += ";" + label.Name + "=" + label.Value
	}

	return key, hash
}

func prometheusLabels(labels []metrics.Tag) prometheus.Labels {
	l := make(prometheus.Labels)
	for _, label := range labels {
		l[label.Name] = label.Value
	}
	return l
}

// SetGauge should retain the last value it is set to
func (p *Sink) SetGauge(parts []string, val float32, labels []metrics.Tag) {
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
		localGauge.Set(float64(val))
		localGauge.updatedAt = time.Now()
		p.gauges.Store(hash, &localGauge)

		// The gauge does not exist, create the gauge and allow it to be deleted
	} else {
		help := key
		existingHelp, ok := p.help["gauge."+key]
		if ok {
			help = existingHelp
		}
		g := prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        key,
			Help:        help,
			ConstLabels: prometheusLabels(labels),
		})
		g.Set(float64(val))
		pg = &gauge{
			Gauge:     g,
			updatedAt: time.Now(),
			canDelete: true,
		}
		p.gauges.Store(hash, pg)
	}
}

// AddSample is for timing information, where quantiles are used
func (p *Sink) AddSample(parts []string, val float32, labels []metrics.Tag) {
	key, hash := flattenKey(parts, labels)
	ps, ok := p.summaries.Load(hash)

	// Does the summary already exist for this sample type?
	if ok {
		localSummary := *ps.(*summary)
		localSummary.Observe(float64(val))
		localSummary.updatedAt = time.Now()
		p.summaries.Store(hash, &localSummary)

		// The summary does not exist, create the Summary and allow it to be deleted
	} else {
		help := key
		existingHelp, ok := p.help["summary."+key]
		if ok {
			help = existingHelp
		}
		s := prometheus.NewSummary(prometheus.SummaryOpts{
			Name:        key,
			Help:        help,
			MaxAge:      ObservationMaxAge,
			ConstLabels: prometheusLabels(labels),
			Objectives:  map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		})
		s.Observe(float64(val))
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
// func (p *Sink) EmitKey(key []string, val float32, labels []metrics.Tag) {
// }

// IncrCounter should accumulate values
func (p *Sink) IncrCounter(parts []string, val float32, labels []metrics.Tag) {
	key, hash := flattenKey(parts, labels)
	pc, ok := p.counters.Load(hash)

	// Does the counter exist?
	if ok {
		localCounter := *pc.(*counter)
		localCounter.Add(float64(val))
		localCounter.updatedAt = time.Now()
		p.counters.Store(hash, &localCounter)

		// The counter does not exist yet, create it and allow it to be deleted
	} else {
		help := key
		existingHelp, ok := p.help["counter."+key]
		if ok {
			help = existingHelp
		}
		c := prometheus.NewCounter(prometheus.CounterOpts{
			Name:        key,
			Help:        help,
			ConstLabels: prometheusLabels(labels),
		})
		c.Add(float64(val))
		pc = &counter{
			Counter:   c,
			updatedAt: time.Now(),
			canDelete: true,
		}
		p.counters.Store(hash, pc)
	}
}

// PushSink wraps a normal prometheus sink and provides an address and facilities to export it to an address
// on an interval.
type PushSink struct {
	*Sink
	pusher       *push.Pusher
	address      string
	pushInterval time.Duration
	stopChan     chan struct{}
}

// NewPushSink creates a PrometheusPushSink by taking an address, interval, and destination name.
func NewPushSink(address string, pushInterval time.Duration, name string) (*PushSink, error) {
	promSink := &Sink{
		gauges:     sync.Map{},
		summaries:  sync.Map{},
		counters:   sync.Map{},
		expiration: 60 * time.Second,
		name:       "default_prometheus_sink",
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

func (s *PushSink) flushMetrics() {
	ticker := time.NewTicker(s.pushInterval)

	go func() {
		for {
			select {
			case <-ticker.C:
				err := s.pusher.Push()
				if err != nil {
					log.Printf("[ERR] Error pushing to Prometheus! Err: %s", err)
				}
			case <-s.stopChan:
				ticker.Stop()
				return
			}
		}
	}()
}

// Shutdown tears down the PrometheusPushSink, and blocks while flushing metrics to the backend.
func (s *PushSink) Shutdown() {
	close(s.stopChan)
	// Closing the channel only stops the running goroutine that pushes metrics.
	// To minimize the chance of data loss pusher.Push is called one last time.
	s.pusher.Push()
}
