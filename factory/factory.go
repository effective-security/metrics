package factory

import (
	"net/url"

	"github.com/effective-security/metrics"
	"github.com/effective-security/metrics/statsd"
	"github.com/pkg/errors"
)

// sinkURLFactoryFunc is an generic interface around the *SinkFromURL() function provided
// by each sink type
type sinkURLFactoryFunc func(*url.URL) (metrics.Sink, error)

// sinkRegistry supports the generic NewMetricSink function by mapping URL
// schemes to metric sink factory functions
var sinkRegistry = map[string]sinkURLFactoryFunc{
	"statsd": statsd.NewSinkFromURL,
	"inmem":  metrics.NewInmemSinkFromURL,
}

// NewMetricSinkFromURL allows a generic URL input to configure any of the
// supported sinks. The scheme of the URL identifies the type of the sink, the
// and query parameters are used to set options.
//
// "statsd://" - Initializes a StatsdSink. The host and port are passed through
// as the "addr" of the sink
//
// "statsite://" - Initializes a StatsiteSink. The host and port become the
// "addr" of the sink
//
// "inmem://" - Initializes an InmemSink. The host and port are ignored. The
// "interval" and "retain" query parameters must be specified with valid
// durations, see NewInmemSink for details.
func NewMetricSinkFromURL(urlStr string) (metrics.Sink, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	sinkURLFactoryFunc := sinkRegistry[u.Scheme]
	if sinkURLFactoryFunc == nil {
		return nil, errors.Errorf("unrecognized sink name: %q", u.Scheme)
	}

	return sinkURLFactoryFunc(u)
}
