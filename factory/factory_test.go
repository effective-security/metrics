package factory_test

import (
	"testing"
	"time"

	"github.com/effective-security/metrics"
	"github.com/effective-security/metrics/factory"
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

func Test_NewMetricSinkFromURL_InMem(t *testing.T) {
	im, err := factory.NewMetricSinkFromURL("inmem://localhost?interval=1s&retain=1m")
	require.NoError(t, err)
	prov, err := metrics.New(&metrics.Config{
		FilterDefault: true,
	}, im)
	require.NoError(t, err)
	run(prov, 10)
}

func Test_NewMetricSinkFromURL_InMem_InvalidParams(t *testing.T) {
	_, err := factory.NewMetricSinkFromURL("inmem://localhost?interval=1s")
	assert.EqualError(t, err, "bad 'retain' param: time: invalid duration \"\"")

	_, err = factory.NewMetricSinkFromURL("inmem://localhost?interval=1s&retain=xxx")
	assert.EqualError(t, err, "bad 'retain' param: time: invalid duration \"xxx\"")

	_, err = factory.NewMetricSinkFromURL("inmem://localhost?retain=1s")
	assert.EqualError(t, err, "bad 'interval' param: time: invalid duration \"\"")

	_, err = factory.NewMetricSinkFromURL("inmem://localhost?retain=1s&interval=yyy")
	assert.EqualError(t, err, "bad 'interval' param: time: invalid duration \"yyy\"")

	_, err = factory.NewMetricSinkFromURL("notsupported://localhost?interval=1s")
	assert.EqualError(t, err, "unrecognized sink name: \"notsupported\"")

	_, err = factory.NewMetricSinkFromURL("^notURL::://\x7f")
	assert.EqualError(t, err, "parse \"^notURL::://\\x7f\": net/url: invalid control character in URL")
}
