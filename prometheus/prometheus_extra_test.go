package prometheus_test

import (
	"sync"
	"testing"
	"time"

	"github.com/effective-security/metrics"
	esprom "github.com/effective-security/metrics/prometheus"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_NewPushSink_InvalidInterval(t *testing.T) {
	_, err := esprom.NewPushSink("127.0.0.1:1", 0, "job")
	assert.EqualError(t, err, "invalid pushInterval: must be positive, got 0s")

	_, err = esprom.NewPushSink("127.0.0.1:1", -time.Second, "job")
	assert.EqualError(t, err, "invalid pushInterval: must be positive, got -1s")
}

func Test_NewPushSinkFrom(t *testing.T) {
	reg := prom.NewRegistry()
	sink, err := esprom.NewPushSinkFrom("127.0.0.1:1", time.Hour, "job", esprom.Opts{
		Name:       "push_sink_from",
		Registerer: reg,
		Expiration: time.Minute,
		Help: map[string]string{
			"test_push_counter": "test_push_counter provides a help text",
		},
	})
	require.NoError(t, err)

	// the options are honored: the sink is registered with the given registerer
	sink.IncrCounter("test_push_counter", 1, nil)
	families, err := reg.Gather()
	require.NoError(t, err)
	var found bool
	for _, f := range families {
		if f.GetName() == "test_push_counter" {
			found = true
			assert.Equal(t, "test_push_counter provides a help text", f.GetHelp())
		}
	}
	assert.True(t, found, "the push sink must be collectable through its registerer")

	// registering a second sink under the same name fails instead of panicking
	_, err = esprom.NewPushSinkFrom("127.0.0.1:1", time.Hour, "job", esprom.Opts{
		Name:       "push_sink_from",
		Registerer: reg,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unable to register sink")

	// Shutdown is idempotent
	require.NotPanics(t, func() {
		sink.Shutdown()
		sink.Shutdown()
	})
}

func Test_Sink_ConcurrentEmission(t *testing.T) {
	reg := prom.NewRegistry()
	sink, err := esprom.NewSinkFrom(esprom.Opts{
		Name:       "concurrent_sink",
		Registerer: reg,
		Expiration: time.Minute,
	})
	require.NoError(t, err)

	const (
		goroutines = 8
		iterations = 200
	)
	tags := []metrics.Tag{{Name: "tag", Value: "value"}}

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				sink.IncrCounter("test_metrics_counter", 1, tags)
				sink.SetGauge("test_metrics_gauge", 1, tags)
				sink.AddSample("test_metrics_sample", 1, tags)
			}
		}()
	}
	wg.Wait()

	// every increment must be accounted for, including the ones emitted while
	// the series was being created
	assert.Equal(t, float64(goroutines*iterations), counterTotal(t, reg, "test_metrics_counter"))
}

// counterTotal returns the value of the named counter in the registry.
func counterTotal(t *testing.T, reg *prom.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		require.Len(t, f.GetMetric(), 1)
		return f.GetMetric()[0].GetCounter().GetValue()
	}
	t.Fatalf("counter %q not found", name)
	return 0
}

func Test_Sink_InvalidSeriesDoesNotBreakScrape(t *testing.T) {
	reg := prom.NewRegistry()
	sink, err := esprom.NewSinkFrom(esprom.Opts{
		Name:       "invalid_series_sink",
		Registerer: reg,
		Expiration: time.Minute,
	})
	require.NoError(t, err)

	// a series the client library rejects must be dropped, not registered
	// with an erroring descriptor that fails every scrape until it expires
	sink.SetGauge("", 1, nil)
	sink.IncrCounter("test_metrics_counter", 1, []metrics.Tag{{Name: "__reserved", Value: "v"}})
	sink.AddSample("test_metrics_sample", 1, []metrics.Tag{{Name: "", Value: "v"}})
	sink.IncrCounter("test_metrics_counter", 1, nil)

	families, err := reg.Gather()
	require.NoError(t, err, "one invalid series must not fail the whole scrape")
	require.Len(t, families, 1)
	assert.Equal(t, "test_metrics_counter", families[0].GetName())
	assert.Equal(t, float64(1), counterTotal(t, reg, "test_metrics_counter"))
}

func Test_NewSinkFrom_DuplicateName(t *testing.T) {
	reg := prom.NewRegistry()
	first, err := esprom.NewSinkFrom(esprom.Opts{
		Name:       "duplicate_sink",
		Registerer: reg,
	})
	require.NoError(t, err)
	require.NotNil(t, first)

	// the same name on the same registerer is an error, with no sink and
	// with the cause preserved
	second, err := esprom.NewSinkFrom(esprom.Opts{
		Name:       "duplicate_sink",
		Registerer: reg,
	})
	require.Error(t, err)
	assert.Nil(t, second)
	assert.Contains(t, err.Error(), "unable to register sink")
	var already prom.AlreadyRegisteredError
	assert.ErrorAs(t, err, &already)
	assert.Same(t, first, already.ExistingCollector)
}
