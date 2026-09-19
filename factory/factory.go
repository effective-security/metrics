package factory

import (
	"net/url"

	"github.com/cockroachdb/errors"
	"github.com/effective-security/metrics"
)

// sinkURLFactoryFunc is a generic interface around the *SinkFromURL() function provided
// by each sink type
type sinkURLFactoryFunc func(*url.URL) (metrics.Sink, error)

// sinkRegistry supports the generic NewMetricSinkFromURL function by mapping URL
// schemes to metric sink factory functions
var sinkRegistry = map[string]sinkURLFactoryFunc{
	"inmem": metrics.NewInmemSinkFromURL,
}

// NewMetricSinkFromURL allows a generic URL input to configure any of the
// supported sinks. The scheme of the URL identifies the type of the sink, and
// the query parameters are used to set options.
//
// Supported schemes:
//
//	"inmem://" - Initializes an InmemSink. The host and port are ignored. The
//	"interval" and "retain" query parameters must be specified with valid
//	durations, see metrics.NewInmemSink for details.
//
// The Prometheus and CloudWatch sinks are not available here: they need
// options, such as a registerer or an AWS namespace, that do not map onto a URL.
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
