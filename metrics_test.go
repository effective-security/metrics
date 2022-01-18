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
		p.SetGauge([]string{"test", "metrics", "gauge"}, float32(i))
		p.IncrCounter([]string{"test", "metrics", "counter"}, float32(i))
		p.AddSample([]string{"test", "metrics", "sample"}, float32(i))
		p.MeasureSince([]string{"test", "metrics", "since"}, time.Now().Add(time.Duration(i)*time.Second))
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
		assert.Equal(t, "es.test.metrics.counter", k)
		s := v.String()
		assert.Contains(t, s, "Count:")
	}
	for k, v := range first.Samples {
		assert.Contains(t, k, "es.test.metrics.")
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
	prov.UpdateFilter(nil, []string{"es.test.metrics.since", "es.test.metrics.sample"})

	require.NoError(t, err)
	run(prov, 1)

	data := im.Data()
	assert.Len(t, data, 1)
	first := data[0]
	assert.NotEmpty(t, first.Counters)
	assert.NotEmpty(t, first.Gauges)
	assert.Empty(t, first.Samples)
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
		assert.Equal(t, fmt.Sprintf("global.es.%s.test.metrics.counter;type=global", cfg.HostName), k)
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

//
// Mock
//
type mockedSink struct {
	t *testing.T
	mock.Mock
}

func (m *mockedSink) SetGauge(key []string, val float32, labels []metrics.Tag) {
	m.t.Logf("SetGauge key=%v", key)
	m.Called(key, val, labels)
}

func (m *mockedSink) IncrCounter(key []string, val float32, labels []metrics.Tag) {
	m.t.Logf("IncrCounter key=%v", key)
	m.Called(key, val, labels)
}

func (m *mockedSink) AddSample(key []string, val float32, labels []metrics.Tag) {
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
			ServiceName:    "dolly",
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
			ServiceName:    "dolly",
			EnableHostname: true,
			FilterDefault:  true,
		},
		fan)
	require.NoError(t, err)

	run(prov, 1)

	fan.SetGauge([]string{"test", "metrics", "gauge"}, float32(0), nil)
	fan.IncrCounter([]string{"test", "metrics", "counter"}, float32(0), nil)
	fan.AddSample([]string{"test", "metrics", "sample"}, float32(0), nil)

	// assert that the expectations were met
	mocked.AssertExpectations(t)
}

func Test_Global(t *testing.T) {
	cfg := metrics.DefaultConfig("es")
	im := metrics.NewInmemSink(time.Second, time.Minute)

	_, err := metrics.NewGlobal(cfg, im)
	require.NoError(t, err)

	metrics.UpdateFilter([]string{"es"}, nil)
	metrics.UpdateFilterAndLabels([]string{"es"}, nil, nil, nil)
	metrics.SetGauge([]string{"test", "metrics", "gauge"}, 123)
	metrics.IncrCounter([]string{"test", "metrics", "counter"}, 123)
	metrics.AddSample([]string{"test", "metrics", "sample"}, 123)
	metrics.MeasureSince([]string{"test", "metrics", "since"}, time.Now())

	data := im.Data()
	assert.Len(t, data, 1)
	first := data[0]
	assert.Len(t, first.Counters, 1)
	assert.Len(t, first.Gauges, 1)
	assert.Len(t, first.Samples, 2)

	for k, v := range first.Counters {
		assert.Equal(t, "es.test.metrics.counter", k)
		s := v.String()
		assert.Contains(t, s, "Count:")
	}
	for k, v := range first.Samples {
		assert.Contains(t, k, "es.test.metrics.")
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
