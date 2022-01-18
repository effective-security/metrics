package prometheus_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/effective-security/metrics"
	"github.com/effective-security/metrics/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/assert"
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

func Test_SetProviderPrometheus(t *testing.T) {
	prometheus.DefaultPrometheusOpts = prometheus.Opts{
		Expiration: 2 * time.Second,
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
	promhttp.Handler().ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()
	assert.Contains(t, body, "es_test_metrics_since_count")
	assert.Contains(t, body, `env_tag="test_value"`)

	// let them expire
	time.Sleep(prometheus.DefaultPrometheusOpts.Expiration + time.Second)

	w = httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)

	body = w.Body.String()
	assert.NotContains(t, body, "es_test_metrics_since_count")
	assert.NotContains(t, body, `env_tag="test_value"`)
}
