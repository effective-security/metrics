package metrics_test

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/effective-security/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func run(p metrics.Provider, times int) {
	for i := 0; i < times; i++ {
		p.SetGauge("test_metrics_gauge", float64(i))
		p.IncrCounter("test_metrics_counter", float64(i))
		p.AddSample("test_metrics_sample", float64(i))
		p.MeasureSince("test_metrics_since", time.Now().Add(time.Duration(i)*time.Second))
	}
}

func Test_Default(t *testing.T) {
	cfg := metrics.DefaultConfig("es")
	im := metrics.NewInmemSink(time.Second, time.Minute)
	prov, err := metrics.New(cfg, im)
	prov.UpdateFilter([]string{"es"}, nil)

	require.NoError(t, err)
	run(prov, 1)

	data := im.Data()
	assert.Len(t, data, 1)
	first := data[0]
	assert.Len(t, first.Counters, 1)
	assert.Len(t, first.Gauges, 1)
	assert.Len(t, first.Samples, 2)

	for k, v := range first.Counters {
		assert.Equal(t, "es_test_metrics_counter", k)
		s := v.String()
		assert.Contains(t, s, "Count:")
	}
	for k, v := range first.Samples {
		assert.Contains(t, k, "es_test_metrics_")
		s := v.String()
		assert.NotEmpty(t, s)
		assert.Contains(t, s, "Count:")
	}
}

func Test_DefaultWithFilter(t *testing.T) {
	cfg := metrics.DefaultConfig("es")
	cfg.FilterDefault = true
	im := metrics.NewInmemSink(time.Second, time.Minute)
	prov, err := metrics.New(cfg, im)
	prov.UpdateFilter(nil, []string{"es"})

	require.NoError(t, err)
	run(prov, 1)

	data := im.Data()
	assert.Len(t, data, 1)
	first := data[0]
	assert.Empty(t, first.Counters)
	assert.Empty(t, first.Gauges)
	assert.Empty(t, first.Samples)

	prov.UpdateFilter([]string{"es"}, nil)
	require.NoError(t, err)
	run(prov, 1)

	data = im.Data()
	assert.Len(t, data, 1)
	first = data[0]
	assert.NotEmpty(t, first.Counters)
	assert.NotEmpty(t, first.Gauges)
	assert.NotEmpty(t, first.Samples)

	cfg.FilterDefault = true
	prov.UpdateFilter(nil, nil)
	require.NoError(t, err)
	run(prov, 1)

	data = im.Data()
	assert.Len(t, data, 1)
	first = data[0]
	assert.NotEmpty(t, first.Counters)
	assert.NotEmpty(t, first.Gauges)
	assert.NotEmpty(t, first.Samples)
}

func Test_DefaultWithCustom(t *testing.T) {
	cfg := metrics.DefaultConfig("es")
	cfg.EnableHostname = true
	cfg.EnableHostnameLabel = false
	cfg.GlobalPrefix = "global"
	cfg.GlobalTags = []metrics.Tag{
		{Name: "type", Value: "global"},
	}
	im := metrics.NewInmemSink(time.Second, time.Minute)
	prov, err := metrics.New(cfg, im)
	require.NoError(t, err)
	run(prov, 1)
	data := im.Data()
	assert.Len(t, data, 1)
	first := data[0]
	assert.Len(t, first.Counters, 1)
	assert.Len(t, first.Gauges, 1)
	assert.Len(t, first.Samples, 2)

	for k, v := range first.Counters {
		assert.Equal(t, fmt.Sprintf("global_es_%s_test_metrics_counter;type=global", cfg.HostName), k)
		assert.NotEmpty(t, v.Labels)
		s := v.String()
		assert.Contains(t, s, "Count:")
	}
}

func Test_SetProvider_NewInmemSink(t *testing.T) {
	im := metrics.NewInmemSink(time.Second, time.Minute)
	prov, err := metrics.New(&metrics.Config{
		FilterDefault: true,
	}, im)
	require.NoError(t, err)
	run(prov, 10)
	data := im.Data()
	assert.NotEmpty(t, data)
}

func Test_SetProvider_BlackWholeSink(t *testing.T) {
	im := &metrics.BlackholeSink{}
	prov, err := metrics.New(&metrics.Config{
		FilterDefault: true,
	}, im)
	require.NoError(t, err)
	run(prov, 10)
}

// Mock
type mockedSink struct {
	t *testing.T
	mock.Mock
}

func (m *mockedSink) SetGauge(key string, val float64, labels []metrics.Tag) {
	m.t.Logf("SetGauge key=%v", key)
	m.Called(key, val, labels)
}

func (m *mockedSink) IncrCounter(key string, val float64, labels []metrics.Tag) {
	m.t.Logf("IncrCounter key=%v", key)
	m.Called(key, val, labels)
}

func (m *mockedSink) AddSample(key string, val float64, labels []metrics.Tag) {
	m.t.Logf("AddSample key=%v", key)
	m.Called(key, val, labels)
}

func Test_Emit(t *testing.T) {
	t.Run("default config", func(t *testing.T) {
		mocked := &mockedSink{t: t}

		// setup expectations
		mocked.AssertNotCalled(t, "SetGauge", mock.Anything, mock.Anything, mock.Anything)
		mocked.AssertNotCalled(t, "IncrCounter", mock.Anything, mock.Anything, mock.Anything)
		mocked.AssertNotCalled(t, "AddSample", mock.Anything, mock.Anything, mock.Anything)

		prov, err := metrics.New(&metrics.Config{}, mocked)
		require.NoError(t, err)

		run(prov, 1)

		// assert that the expectations were met
		mocked.AssertExpectations(t)
	})

	t.Run("enabled config", func(t *testing.T) {
		mocked := &mockedSink{t: t}

		// setup expectations
		mocked.On("SetGauge", mock.Anything, mock.Anything, mock.Anything).Times(0)
		mocked.On("IncrCounter", mock.Anything, mock.Anything, mock.Anything).Times(1)
		mocked.On("AddSample", mock.Anything, mock.Anything, mock.Anything).Times(2)

		prov, err := metrics.New(&metrics.Config{
			ServiceName:    "effective-security",
			EnableHostname: true,
			FilterDefault:  true,
		}, mocked)
		require.NoError(t, err)

		run(prov, 1)

		// assert that the expectations were met
		mocked.AssertExpectations(t)
	})
}

func Test_FanoutSink(t *testing.T) {
	mocked := &mockedSink{t: t}
	fan := metrics.NewFanoutSink(mocked, mocked, metrics.NewInmemSink(time.Minute, time.Minute*5))

	// setup expectations
	mocked.On("SetGauge", mock.Anything, mock.Anything, mock.Anything).Times(0)
	mocked.On("IncrCounter", mock.Anything, mock.Anything, mock.Anything).Times(4)
	mocked.On("AddSample", mock.Anything, mock.Anything, mock.Anything).Times(6)

	prov, err := metrics.New(
		&metrics.Config{
			ServiceName:    "effective-security",
			EnableHostname: true,
			FilterDefault:  true,
		},
		fan)
	require.NoError(t, err)

	run(prov, 1)

	fan.SetGauge("test_metrics_gauge", float64(0), nil)
	fan.IncrCounter("test_metrics_counter", float64(0), nil)
	fan.AddSample("test_metrics_sample", float64(0), nil)

	// assert that the expectations were met
	mocked.AssertExpectations(t)
}

func Test_Global(t *testing.T) {
	cfg := metrics.DefaultConfig("es")
	im := metrics.NewInmemSink(time.Second, time.Minute)

	_, err := metrics.NewGlobal(cfg, im)
	require.NoError(t, err)

	metrics.UpdateFilter([]string{"es"}, nil)
	metrics.SetGauge("test_metrics_gauge", 123)
	metrics.IncrCounter("test_metrics_counter", 123)
	metrics.AddSample("test_metrics_sample", 123)
	metrics.MeasureSince("test_metrics_since", time.Now())

	data := im.Data()
	assert.Len(t, data, 1)
	first := data[0]
	assert.Len(t, first.Counters, 1)
	assert.Len(t, first.Gauges, 1)
	assert.Len(t, first.Samples, 2)

	for k, v := range first.Counters {
		assert.Equal(t, "es_test_metrics_counter", k)
		s := v.String()
		assert.Contains(t, s, "Count:")
	}
	for k, v := range first.Samples {
		assert.Contains(t, k, "es_test_metrics_")
		s := v.String()
		assert.NotEmpty(t, s)
		assert.Contains(t, s, "Count:")
	}
}

func Test_NewMetricSinkFromURL_InMem(t *testing.T) {
	u, err := url.Parse("inmem://localhost?interval=1s&retain=1m")
	require.NoError(t, err)

	im, err := metrics.NewInmemSinkFromURL(u)
	require.NoError(t, err)
	prov, err := metrics.New(&metrics.Config{
		FilterDefault: true,
	}, im)
	require.NoError(t, err)
	run(prov, 1)

	data := im.(*metrics.InmemSink).Data()
	assert.Len(t, data, 1)
	first := data[0]
	assert.Len(t, first.Counters, 1)
	assert.Len(t, first.Gauges, 1)
	assert.Len(t, first.Samples, 2)
}

func Test_StringStartsWithOneOf(t *testing.T) {
	tcases := []struct {
		str    string
		slices []string
		exp    bool
	}{
		{"Daniel", []string{"foo", "bar"}, false},
		{"foo_Daniel", []string{"foo", "el"}, true},
		{"daniel", []string{"foo", "da"}, true},
		{"Daniel", []string{"foo", "Dan"}, true},
	}
	for idx, tc := range tcases {
		res := metrics.StringStartsWithOneOf(tc.str, tc.slices)
		if res != tc.exp {
			t.Errorf("case %d failed", idx)
		}
	}
}

func Test_Describe(t *testing.T) {
	mymetric := metrics.Describe{
		Name:         "test",
		Help:         "test metric",
		RequiredTags: []string{"tag1", "tag2"},
	}
	mymetricNoTags := metrics.Describe{
		Name: "simple",
		Help: "test metric",
	}

	mocked := &mockedSink{t: t}

	// setup expectations
	mocked.On("IncrCounter", mymetric.Name, float64(1), []metrics.Tag{
		{Name: "tag1", Value: "1"},
		{Name: "tag2", Value: "2"},
	}).Times(2)
	mocked.On("AddSample", mymetricNoTags.Name, float64(1), []metrics.Tag(nil)).Times(1)

	mocked.IncrCounter(mymetric.Name, 1, mymetric.Tags("1", "2"))
	mocked.IncrCounter(mymetric.Name, 1, mymetric.Tags("1", "2"))
	mocked.AddSample(mymetricNoTags.Name, 1, mymetricNoTags.Tags())

	// assert that the expectations were met
	mocked.AssertExpectations(t)
	/*
		assert.Panics(t, func() {
			mymetric.IncrCounter(1)
		})

		assert.Panics(t, func() {
			mymetric.IncrCounter(1, "only one")
		})

		assert.Panics(t, func() {
			mymetric.IncrCounter(1, "1", "2", "3")
		})

		assert.Panics(t, func() {
			mymetric.SetGauge(1)
		})

		assert.Panics(t, func() {
			mymetric.SetGauge(1, "only one")
		})

		assert.Panics(t, func() {
			mymetric.SetGauge(1, "1", "2", "3")
		})

		assert.Panics(t, func() {
			mymetric.AddSample(1)
		})

		assert.Panics(t, func() {
			mymetric.AddSample(1, "only one")
		})

		assert.Panics(t, func() {
			mymetric.AddSample(1, "1", "2", "3")
		})

		assert.Panics(t, func() {
			mymetric.MeasureSince(time.Now(), "1", "2", "3")
		})
	*/
}

func Test_DescribeHelp(t *testing.T) {
	list := []*metrics.Describe{
		{
			Type:         metrics.TypeCounter,
			Name:         "test",
			Help:         "test counter metric",
			RequiredTags: []string{"tag1", "tag2"},
		},
		{
			Type: "summary",
			Name: "simple_times",
			Help: "test summary metric",
		},
	}

	cfg := metrics.DefaultConfig("es")
	help := cfg.Help(list)
	require.Len(t, help, 2)
	assert.Equal(t, help["es_test"], "test counter metric")
	assert.Equal(t, help["es_simple_times"], "test summary metric")

	cfg = &metrics.Config{
		ServiceName:          "es", // Use client provided service
		HostName:             "host1",
		EnableHostname:       false, // Enable hostname prefix
		EnableRuntimeMetrics: false, // Enable runtime profiling
		EnableTypePrefix:     true,  // Disable type prefix
		FilterDefault:        true,  // Don't filter metrics by default
		GlobalPrefix:         "global",
		BlockedPrefixes:      []string{"global_es_summary_"},
	}
	help = cfg.Help(list)
	require.Len(t, help, 1)
	assert.Equal(t, help["global_es_counter_test"], "test counter metric")
}
