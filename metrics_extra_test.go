package metrics_test

import (
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/effective-security/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingSink records how many values it received.
type countingSink struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingSink() *countingSink {
	return &countingSink{counts: make(map[string]int)}
}

func (s *countingSink) add(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[key]++
}

func (s *countingSink) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.counts {
		n += v
	}
	return n
}

func (s *countingSink) SetGauge(key string, _ float64, _ []metrics.Tag)    { s.add(key) }
func (s *countingSink) IncrCounter(key string, _ float64, _ []metrics.Tag) { s.add(key) }
func (s *countingSink) AddSample(key string, _ float64, _ []metrics.Tag)   { s.add(key) }

func Test_AllowMetric(t *testing.T) {
	tcases := []struct {
		name          string
		allowed       []string
		blocked       []string
		filterDefault bool
		key           string
		exp           bool
	}{
		{name: "no rules, default allow", filterDefault: true, key: "es_metric", exp: true},
		{name: "no rules, default block", filterDefault: false, key: "es_metric", exp: false},
		{name: "allowed match", allowed: []string{"es_"}, key: "es_metric", exp: true},
		{name: "allowed match overrides default block", allowed: []string{"es_"}, filterDefault: false, key: "es_metric", exp: true},
		{name: "allowed miss falls to default block", allowed: []string{"es_"}, filterDefault: false, key: "other_metric", exp: false},
		{name: "allowed miss falls to default allow", allowed: []string{"es_"}, filterDefault: true, key: "other_metric", exp: true},
		{name: "blocked match", blocked: []string{"es_"}, filterDefault: true, key: "es_metric", exp: false},
		{name: "blocked wins over allowed", allowed: []string{"es_"}, blocked: []string{"es_noisy_"}, filterDefault: true, key: "es_noisy_metric", exp: false},
		{name: "blocked miss falls to allowed", allowed: []string{"es_"}, blocked: []string{"es_noisy_"}, filterDefault: false, key: "es_metric", exp: true},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &metrics.Config{
				AllowedPrefixes: tc.allowed,
				BlockedPrefixes: tc.blocked,
				FilterDefault:   tc.filterDefault,
			}
			assert.Equal(t, tc.exp, cfg.AllowMetric(tc.key))

			// the instance must agree with the config it was built from
			prov, err := metrics.New(cfg, &metrics.BlackholeSink{})
			require.NoError(t, err)
			defer prov.Close()
			assert.Equal(t, tc.exp, prov.AllowMetric(tc.key))
		})
	}
}

func Test_AllowMetric_OnlyAllowedEmitted(t *testing.T) {
	sink := newCountingSink()
	prov, err := metrics.New(&metrics.Config{
		AllowedPrefixes: []string{"allowed_"},
		FilterDefault:   false,
	}, sink)
	require.NoError(t, err)
	defer prov.Close()

	prov.IncrCounter("allowed_metric", 1)
	prov.IncrCounter("denied_metric", 1)

	assert.Equal(t, 1, sink.total())
	assert.Equal(t, 1, sink.counts["allowed_metric"])
	assert.NotContains(t, sink.counts, "denied_metric")
}

func Test_Prepare_DoesNotMutateCallerTags(t *testing.T) {
	t.Run("number prefix", func(t *testing.T) {
		cfg := &metrics.Config{FilterDefault: true, NumberLabelPrefix: "_"}
		caller := []metrics.Tag{{Name: "org", Value: "676220136511767142"}}

		_, _, out := cfg.Prepare(metrics.TypeCounter, "k", caller...)

		assert.Equal(t, "676220136511767142", caller[0].Value, "caller slice must not be modified")
		require.Len(t, out, 1)
		assert.Equal(t, "_676220136511767142", out[0].Value)
	})

	t.Run("global tags do not alias the caller array", func(t *testing.T) {
		cfg := &metrics.Config{
			FilterDefault: true,
			GlobalTags:    []metrics.Tag{{Name: "env", Value: "test"}},
		}
		// a caller slice with spare capacity, as a reused buffer would have
		caller := make([]metrics.Tag, 1, 4)
		caller[0] = metrics.Tag{Name: "a", Value: "1"}

		_, _, out := cfg.Prepare(metrics.TypeCounter, "k", caller...)

		// the caller keeps using its buffer
		caller = append(caller, metrics.Tag{Name: "b", Value: "2"})

		require.Len(t, out, 2)
		assert.Equal(t, []metrics.Tag{
			{Name: "a", Value: "1"},
			{Name: "env", Value: "test"},
		}, out, "returned tags must not be overwritten by the caller")
		assert.Len(t, caller, 2)
	})

	t.Run("emitted tags reach the sink unchanged", func(t *testing.T) {
		cfg := &metrics.Config{
			FilterDefault:     true,
			NumberLabelPrefix: "_",
			GlobalTags:        []metrics.Tag{{Name: "env", Value: "test"}},
		}
		im := metrics.NewInmemSink(time.Hour, 2*time.Hour)
		prov, err := metrics.New(cfg, im)
		require.NoError(t, err)
		defer prov.Close()

		shared := []metrics.Tag{{Name: "org", Value: "676220136511767142"}}
		prov.IncrCounter("k", 1, shared...)
		prov.IncrCounter("k", 1, shared...)

		assert.Equal(t, "676220136511767142", shared[0].Value)

		data := im.Data()
		require.NotEmpty(t, data)
		current := data[len(data)-1]
		require.Len(t, current.Counters, 1, "both emissions must share one key: %v", current.Counters)
		agg, ok := current.Counters["k;org=_676220136511767142;env=test"]
		require.True(t, ok, "unexpected keys: %v", current.Counters)
		assert.Equal(t, 2, agg.Count)
	})
}

func Test_UpdateFilter_WhileEmitting(t *testing.T) {
	prov, err := metrics.New(&metrics.Config{
		FilterDefault:        true,
		EnableRuntimeMetrics: true,
		ProfileInterval:      time.Millisecond,
	}, newCountingSink())
	require.NoError(t, err)
	defer prov.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					prov.IncrCounter("test_metrics_counter", 1)
				}
			}
		}()
	}

	for range 100 {
		prov.UpdateFilter([]string{"test_"}, nil)
		prov.UpdateFilter(nil, []string{"test_"})
	}
	close(stop)
	wg.Wait()

	// the last update is in effect, and is the one AllowMetric reports
	prov.UpdateFilter(nil, []string{"test_"})
	assert.False(t, prov.AllowMetric("test_metrics_counter"))
	prov.UpdateFilter([]string{"test_"}, nil)
	assert.True(t, prov.AllowMetric("test_metrics_counter"))
}

func Test_Close_StopsRuntimeCollector(t *testing.T) {
	sink := newCountingSink()
	prov, err := metrics.New(&metrics.Config{
		FilterDefault:        true,
		EnableRuntimeMetrics: true,
		ProfileInterval:      5 * time.Millisecond,
	}, sink)
	require.NoError(t, err)

	// wait for the collector to emit at least once
	require.Eventually(t, func() bool {
		return sink.total() > 0
	}, time.Second, 5*time.Millisecond)

	prov.Close()

	// a collection already in flight may still land, so wait for the count to
	// stop moving; while the collector runs it never does
	var after int
	require.Eventually(t, func() bool {
		total := sink.total()
		if total == after {
			return true
		}
		after = total
		return false
	}, 2*time.Second, 25*time.Millisecond, "the runtime collector kept emitting after Close")

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, after, sink.total(), "no runtime metrics may be emitted after Close")

	// Close is idempotent, and emitting still works
	prov.Close()
	prov.IncrCounter("test_metrics_counter", 1)
	assert.Equal(t, after+1, sink.total())
}

func Test_NewInmemSink_InvalidDurations(t *testing.T) {
	tcases := []struct {
		name     string
		interval time.Duration
		retain   time.Duration
	}{
		{name: "zero interval", interval: 0, retain: time.Minute},
		{name: "negative interval", interval: -time.Second, retain: time.Minute},
		{name: "retain shorter than interval", interval: time.Minute, retain: time.Second},
		{name: "retain equal to interval", interval: time.Minute, retain: time.Minute},
		{name: "zero retain", interval: time.Second, retain: 0},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			im := metrics.NewInmemSink(tc.interval, tc.retain)
			require.NotNil(t, im)

			im.SetGauge("test_metrics_gauge", 1, nil)
			im.IncrCounter("test_metrics_counter", 1, nil)
			im.AddSample("test_metrics_sample", 1, nil)

			data := im.Data()
			require.NotEmpty(t, data, "the clamped sink must retain an interval")

			summary, err := im.DisplayMetrics()
			require.NoError(t, err)
			require.NotNil(t, summary)
		})
	}
}

func Test_NewInmemSinkFromURL_UnusableDurations(t *testing.T) {
	_, err := metrics.NewInmemSinkFromURL(mustParseURL(t, "inmem://localhost?interval=0s&retain=1m"))
	assert.EqualError(t, err, "bad 'interval' param: must be positive, got 0s")

	_, err = metrics.NewInmemSinkFromURL(mustParseURL(t, "inmem://localhost?interval=1m&retain=1s"))
	assert.EqualError(t, err, "bad 'retain' param: must be at least 2m0s for interval 1m0s, got 1s")
}

func Test_InmemSink_DataIsSnapshot(t *testing.T) {
	im := metrics.NewInmemSink(time.Hour, 2*time.Hour)
	im.IncrCounter("test_metrics_counter", 10, nil)
	im.AddSample("test_metrics_sample", 10, nil)

	data := im.Data()
	require.NotEmpty(t, data)
	current := data[len(data)-1]
	counter := current.Counters["test_metrics_counter"]
	sample := current.Samples["test_metrics_sample"]
	require.NotNil(t, counter.AggregateSample)
	require.NotNil(t, sample.AggregateSample)

	// emissions after the snapshot must not change it
	im.IncrCounter("test_metrics_counter", 90, nil)
	im.AddSample("test_metrics_sample", 90, nil)

	assert.Equal(t, float64(10), counter.Sum)
	assert.Equal(t, 1, counter.Count)
	assert.Equal(t, float64(10), sample.Sum)
	assert.Equal(t, float64(10), sample.Max)

	// while the sink itself did accumulate them
	next := im.Data()
	require.NotEmpty(t, next)
	assert.Equal(t, float64(100), next[len(next)-1].Counters["test_metrics_counter"].Sum)
}

func Test_InmemSink_DisplayMetricsWhileEmitting(t *testing.T) {
	im := metrics.NewInmemSink(10*time.Millisecond, time.Second)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				im.IncrCounter("test_metrics_counter", 1, []metrics.Tag{{Name: "t", Value: "v"}})
				im.AddSample("test_metrics_sample", 1, nil)
			}
		}
	}()

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		summary, err := im.DisplayMetrics()
		require.NoError(t, err)
		require.NotNil(t, summary)
	}
	close(stop)
	wg.Wait()
}

func Test_Describe_HelpMatchesEmittedName(t *testing.T) {
	list := []*metrics.Describe{
		{
			Type: "summary", // alias of TypeSample
			Name: "simple_times",
			Help: "test summary metric",
		},
		{
			Type: "bogus",
			Name: "unknown_type",
			Help: "metric with an invalid type",
		},
	}

	cfg := &metrics.Config{
		ServiceName:      "es",
		EnableTypePrefix: true,
		FilterDefault:    true,
	}
	help := cfg.Help(list)

	// the help key is the name the sample is emitted under
	_, key, _ := cfg.Prepare(metrics.TypeSample, "simple_times")
	assert.Equal(t, "es_sample_simple_times", key)
	assert.Equal(t, "test summary metric", help[key])

	// an unknown type is reported and used as given
	assert.Equal(t, "metric with an invalid type", help["es_bogus_unknown_type"])
}

// mustParseURL parses a sink URL for the test.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func Test_New_NormalizesDurations(t *testing.T) {
	tcases := []struct {
		name        string
		granularity time.Duration
		profile     time.Duration
	}{
		{name: "zero", granularity: 0, profile: 0},
		{name: "negative", granularity: -time.Second, profile: -time.Second},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			sink := newCountingSink()
			var prov *metrics.Metrics
			var err error
			// a non-positive ProfileInterval reaches time.NewTicker
			require.NotPanics(t, func() {
				prov, err = metrics.New(&metrics.Config{
					FilterDefault:        true,
					EnableRuntimeMetrics: true,
					TimerGranularity:     tc.granularity,
					ProfileInterval:      tc.profile,
				}, sink)
			})
			require.NoError(t, err)
			defer prov.Close()

			assert.Equal(t, time.Millisecond, prov.TimerGranularity)
			assert.Equal(t, time.Second, prov.ProfileInterval)

			// the collector runs on the defaulted interval instead of crashing
			prov.MeasureSince("test_metrics_since", time.Now())
			assert.Equal(t, 1, sink.total())
		})
	}
}

func Test_New_CopiesConfigSlices(t *testing.T) {
	cfg := &metrics.Config{
		FilterDefault:   true,
		GlobalTags:      []metrics.Tag{{Name: "env", Value: "test"}},
		AllowedPrefixes: []string{"allowed_"},
		BlockedPrefixes: []string{"blocked_"},
	}
	im := metrics.NewInmemSink(time.Hour, 2*time.Hour)
	prov, err := metrics.New(cfg, im)
	require.NoError(t, err)
	defer prov.Close()

	// the caller keeps using the config it passed in
	cfg.GlobalTags[0] = metrics.Tag{Name: "env", Value: "mutated"}
	cfg.AllowedPrefixes[0] = "mutated_"
	cfg.BlockedPrefixes[0] = "mutated_"

	assert.Equal(t, []metrics.Tag{{Name: "env", Value: "test"}}, prov.GlobalTags)
	assert.Equal(t, []string{"allowed_"}, prov.AllowedPrefixes)
	assert.Equal(t, []string{"blocked_"}, prov.BlockedPrefixes)

	prov.IncrCounter("test_metrics_counter", 1)
	data := im.Data()
	require.NotEmpty(t, data)
	assert.Contains(t, data[len(data)-1].Counters, "test_metrics_counter;env=test")
}

func Test_NumberLabelPrefix_CountsDigitsNotSign(t *testing.T) {
	cfg := &metrics.Config{FilterDefault: true, NumberLabelPrefix: "_"}

	tcases := []struct {
		value string
		exp   string
	}{
		{value: "676220136511767142", exp: "_676220136511767142"},   // 18 digits
		{value: "-676220136511767142", exp: "_-676220136511767142"}, // 18 digits, signed
		{value: "123456789012345", exp: "_123456789012345"},         // exactly 15 digits
		{value: "12345678901234", exp: "12345678901234"},            // 14 digits
		{value: "-12345678901234", exp: "-12345678901234"},          // 14 digits, signed
		{value: "-1234567890123x", exp: "-1234567890123x"},          // not a number
	}
	for _, tc := range tcases {
		t.Run(tc.value, func(t *testing.T) {
			_, _, out := cfg.Prepare(metrics.TypeCounter, "k", metrics.Tag{Name: "org", Value: tc.value})
			require.Len(t, out, 1)
			assert.Equal(t, tc.exp, out[0].Value)
		})
	}
}

func Test_InmemSink_FinishedIntervalsAreCopies(t *testing.T) {
	im := metrics.NewInmemSink(10*time.Millisecond, time.Second)
	im.IncrCounter("test_metrics_counter", 1, nil)

	// cross a bucket boundary, so the first interval is a finished one
	require.Eventually(t, func() bool {
		im.IncrCounter("test_metrics_counter", 1, nil)
		return len(im.Data()) > 1
	}, time.Second, 5*time.Millisecond)

	first := im.Data()
	second := im.Data()
	require.Greater(t, len(first), 1)
	require.Equal(t, len(first), len(second))

	for idx := range first {
		assert.NotSame(t, first[idx], second[idx],
			"every interval must be an independent copy, including finished ones")
		if agg, ok := first[idx].Counters["test_metrics_counter"]; ok {
			other := second[idx].Counters["test_metrics_counter"]
			assert.NotSame(t, agg.AggregateSample, other.AggregateSample)
			assert.Equal(t, agg.Sum, other.Sum)
		}
	}
}

func Test_Metrics_HelpUsesFiltersInEffect(t *testing.T) {
	list := []*metrics.Describe{
		{Type: metrics.TypeCounter, Name: "charge_total", Help: "charge_total provides the number of charges"},
		{Type: metrics.TypeGauge, Name: "runtime_gauge", Help: "runtime_gauge provides noise"},
	}
	cfg := &metrics.Config{
		ServiceName:   "es",
		FilterDefault: true,
	}
	prov, err := metrics.New(cfg, &metrics.BlackholeSink{})
	require.NoError(t, err)
	defer prov.Close()

	// before any update the instance and its config agree
	assert.Equal(t, cfg.Help(list), prov.Help(list))
	assert.Len(t, prov.Help(list), 2)

	// after an update only the instance reports the rules in effect
	prov.UpdateFilter(nil, []string{"es_runtime_"})
	help := prov.Help(list)
	assert.Equal(t, map[string]string{
		"es_charge_total": "charge_total provides the number of charges",
	}, help)
	assert.Len(t, cfg.Help(list), 2, "Config.Help keeps the construction-time rules")
}

func Test_InmemSink_RetentionIsBounded(t *testing.T) {
	const (
		interval = 10 * time.Millisecond
		retain   = 3 * interval
	)
	im := metrics.NewInmemSink(interval, retain)

	// keep emitting across many bucket rollovers
	deadline := time.Now().Add(10 * retain)
	for time.Now().Before(deadline) {
		im.IncrCounter("test_metrics_counter", 1, nil)
		time.Sleep(interval / 4)
	}

	data := im.Data()
	assert.LessOrEqual(t, len(data), int(retain/interval), "old intervals must be evicted")
	for idx := 1; idx < len(data); idx++ {
		assert.True(t, data[idx].Interval.After(data[idx-1].Interval), "intervals are oldest first")
	}
}
