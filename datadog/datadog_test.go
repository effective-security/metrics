package datadog_test

import (
	"testing"
	"time"

	"github.com/effective-security/metrics"
	"github.com/effective-security/metrics/datadog"
	"github.com/stretchr/testify/require"
)

func run(p metrics.Provider, times int) {
	for i := 0; i < times; i++ {
		p.SetGauge([]string{"sanitize", "metrics:gauge"}, float32(i))
		p.SetGauge([]string{"sanitize", "metrics gauge"}, float32(i))
		p.SetGauge([]string{"test", "metrics", "gauge"}, float32(i))
		p.IncrCounter([]string{"test", "metrics", "counter"}, float32(i))
		p.AddSample([]string{"test", "metrics", "sample"}, float32(i))
		p.MeasureSince([]string{"test", "metrics", "since"}, time.Now().Add(time.Duration(i)*time.Second))
	}
}

func Test_SetProviderDatadog(t *testing.T) {
	d, err := datadog.NewDogStatsdSink("127.0.0.1:8125", "es")
	require.NoError(t, err)
	d.SetTags([]string{"datadog"})
	d.EnableHostNamePropagation()

	prov, err := metrics.New(&metrics.Config{
		FilterDefault: true,
	}, d)
	require.NoError(t, err)
	run(prov, 1)
}
