package statsd

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/effective-security/metrics"
	"github.com/effective-security/xlog"
)

var logger = xlog.NewPackageLogger("github.com/effective-security/metrics", "statsd")

const (
	// statsdMaxLen is the maximum size of a packet
	// to send to statsd
	statsdMaxLen = 1400

	// We force flush the statsite metrics after this period of
	// inactivity. Prevents stats from getting stuck in a buffer
	// forever.
	flushInterval = 100 * time.Millisecond
)

// Sink provides a MetricSink that can be used
// with a statsite or statsd metrics server. It uses
// only UDP packets, while StatsiteSink uses TCP.
type Sink struct {
	addr        string
	metricQueue chan string
}

// NewSinkFromURL creates an StatsdSink from a URL. It is used
// (and tested) from NewMetricSinkFromURL.
func NewSinkFromURL(u *url.URL) (metrics.Sink, error) {
	return NewSink(u.Host)
}

// NewSink is used to create a new StatsdSink
func NewSink(addr string) (*Sink, error) {
	s := &Sink{
		addr:        addr,
		metricQueue: make(chan string, 4096),
	}
	go s.flushMetrics()
	return s, nil
}

// Shutdown is used to stop flushing to statsd
func (s *Sink) Shutdown() {
	close(s.metricQueue)
}

// SetGauge should retain the last value it is set to
func (s *Sink) SetGauge(key []string, val float32, tags []metrics.Tag) {
	flatKey := s.flattenKeyLabels(key, tags)
	s.pushMetric(fmt.Sprintf("%s:%f|g\n", flatKey, val))
}

// IncrCounter should accumulate values
func (s *Sink) IncrCounter(key []string, val float32, tags []metrics.Tag) {
	flatKey := s.flattenKeyLabels(key, tags)
	s.pushMetric(fmt.Sprintf("%s:%f|c\n", flatKey, val))
}

// AddSample is for timing information, where quantiles are used
func (s *Sink) AddSample(key []string, val float32, tags []metrics.Tag) {
	flatKey := s.flattenKeyLabels(key, tags)
	s.pushMetric(fmt.Sprintf("%s:%f|ms\n", flatKey, val))
}

// Flattens the key for formatting, removes spaces
func (s *Sink) flattenKey(parts []string) string {
	joined := strings.Join(parts, ".")
	return strings.Map(func(r rune) rune {
		switch r {
		case ':':
			fallthrough
		case ' ':
			return '_'
		default:
			return r
		}
	}, joined)
}

// Flattens the key along with labels for formatting, removes spaces
func (s *Sink) flattenKeyLabels(parts []string, labels []metrics.Tag) string {
	for _, label := range labels {
		parts = append(parts, label.Value)
	}
	return s.flattenKey(parts)
}

// Does a non-blocking push to the metrics queue
func (s *Sink) pushMetric(m string) {
	select {
	case s.metricQueue <- m:
	default:
	}
}

// Flushes metrics
func (s *Sink) flushMetrics() {
	var sock net.Conn
	var err error
	var wait <-chan time.Time
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

CONNECT:
	// Create a buffer
	buf := bytes.NewBuffer(nil)

	// Attempt to connect
	sock, err = net.Dial("udp", s.addr)
	if err != nil {
		logger.KV(xlog.ERROR, "reason", "connecting", "err", err)
		goto WAIT
	}

	for {
		select {
		case metric, ok := <-s.metricQueue:
			// Get a metric from the queue
			if !ok {
				goto QUIT
			}

			// Check if this would overflow the packet size
			if len(metric)+buf.Len() > statsdMaxLen {
				_, err := sock.Write(buf.Bytes())
				buf.Reset()
				if err != nil {
					logger.KV(xlog.ERROR, "reason", "writing", "err", err)
					goto WAIT
				}
			}

			// Append to the buffer
			buf.WriteString(metric)

		case <-ticker.C:
			if buf.Len() == 0 {
				continue
			}

			_, err := sock.Write(buf.Bytes())
			buf.Reset()
			if err != nil {
				logger.KV(xlog.ERROR, "reason", "flushing", "err", err)
				goto WAIT
			}
		}
	}

WAIT:
	// Wait for a while
	wait = time.After(time.Duration(5) * time.Second)
	for {
		select {
		// Dequeue the messages to avoid backlog
		case _, ok := <-s.metricQueue:
			if !ok {
				goto QUIT
			}
		case <-wait:
			goto CONNECT
		}
	}
QUIT:
	s.metricQueue = nil
}
