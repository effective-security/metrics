package prometheus_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/effective-security/metrics"
	"github.com/effective-security/metrics/prometheus"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/assert"
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

func Test_SetProviderPrometheus(t *testing.T) {
	reg := prom.NewRegistry()
	prometheus.DefaultPrometheusOpts = prometheus.Opts{
		Expiration: 2 * time.Second,
		Registerer: reg,
		CounterDefinitions: []prometheus.CounterDefinition{
			{
				Name: "eses_test_metrics_countercounter",
				Help: "counter.es_test_metrics_counter provides test count",
			},
		},
		GaugeDefinitions: []prometheus.GaugeDefinition{
			{
				Name: "es_test_metrics_gauge",
				Help: "gauge.es_test_metrics_gauge provides test gauge",
			},
		},
		SummaryDefinitions: []prometheus.SummaryDefinition{
			{
				Name: "es_test_metrics_sample",
				Help: "sample.es_test_metrics_sample provides test sample",
			},
		},
	}
	d, err := prometheus.NewSink()
	require.NoError(t, err)

	prov, err := metrics.New(&metrics.Config{
		FilterDefault: true,
		GlobalTags: []metrics.Tag{
			{Name: "env_tag", Value: "test_value"},
		},
		GlobalPrefix: "es",
	}, d)
	require.NoError(t, err)

	run(prov, 1)

	r, err := http.NewRequest(http.MethodGet, "/stats", nil)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(w, r)

	body := w.Body.String()
	require.Equal(t, http.StatusOK, w.Code, "Error: "+body)
	assert.Contains(t, body, "es_test_metrics_since_count")
	assert.Contains(t, body, `env_tag="test_value"`)

	// let them expire
	time.Sleep(prometheus.DefaultPrometheusOpts.Expiration + time.Second)

	w = httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(w, r)
	body = w.Body.String()
	require.Equal(t, http.StatusOK, w.Code, "Error: "+body)
	assert.NotContains(t, body, "es_test_metrics_since_count")
	assert.NotContains(t, body, `env_tag="test_value"`)

	//check if register has a sink by unregistering it.
	ok := reg.Unregister(d)
	assert.True(t, ok)
}
