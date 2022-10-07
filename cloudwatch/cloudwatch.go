package cloudwatch

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/effective-security/metrics"
	"github.com/effective-security/xlog"
	"github.com/pkg/errors"
)

var logger = xlog.NewPackageLogger("github.com/effective-security/metrics", "cloudwatch")

// Config defines configuration options
type Config struct {
	// Required. The AWS Region to use
	AwsRegion string

	// Required. The CloudWatch namespace under which metrics should be published
	CloudWatchNamespace string

	// The frequency with which metrics should be published to Cloudwatch.
	CloudWatchPublishInterval time.Duration

	// Timeout for sending metrics to Cloudwatch.
	CloudWatchPublishTimeout time.Duration
}

// Sink provides a MetricSink that can be used
// with a prometheus server.
type Sink struct {
	mu                        sync.Mutex
	cloudWatchPublishInterval time.Duration
	cloudWatchNamespace       string
	cw                        *cloudwatch.CloudWatch
	expiration                time.Duration

	gauges   map[string]*cloudwatch.MetricDatum
	samples  map[string]*cloudwatch.MetricDatum
	counters map[string]*cloudwatch.MetricDatum
	updates  map[string]time.Time
}

// NewSink initializes and returns a pointer to a CloudWatch Sink using the
// supplied configuration, or an error if there is a problem with the configuration
func NewSink(c *Config) (*Sink, error) {
	if c.CloudWatchNamespace == "" {
		return nil, errors.New("CloudWatchNamespace required")
	}

	region := c.AwsRegion
	if region == "" {
		region, _ = os.LookupEnv("AWS_DEFAULT_REGION")
	}

	if region == "" {
		return nil, errors.New("CloudWatchRegion required")
	}

	sink := &Sink{
		gauges:   make(map[string]*cloudwatch.MetricDatum),
		samples:  make(map[string]*cloudwatch.MetricDatum),
		counters: make(map[string]*cloudwatch.MetricDatum),
		updates:  make(map[string]time.Time),
		//expiration: opts.Expiration,
		cloudWatchNamespace: c.CloudWatchNamespace,
	}

	if c.CloudWatchPublishInterval > 0 {
		sink.cloudWatchPublishInterval = c.CloudWatchPublishInterval
	} else {
		sink.cloudWatchPublishInterval = 30 * time.Second
	}

	var client = http.DefaultClient

	if c.CloudWatchPublishTimeout > 0 {
		client.Timeout = c.CloudWatchPublishTimeout
	} else {
		client.Timeout = 6 * time.Second
	}

	config := aws.NewConfig().WithHTTPClient(client).WithRegion(region)
	sess, err := session.NewSession(config)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	sink.cw = cloudwatch.New(sess)
	return sink, nil
}

// Run starts a loop that will push metrics to Cloudwatch at the configured interval.
// Accepts a context.Context to support cancellation
func (p *Sink) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cloudWatchPublishInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.KV(xlog.DEBUG, "reason", "stopping")
			err := p.Flush()
			if err != nil {
				logger.KV(xlog.ERROR, "reason", "Flush", "err", err)
			}
			return
		case <-ticker.C:
			err := p.Flush()
			if err != nil {
				logger.KV(xlog.ERROR, "reason", "publish", "err", err)
				msg := err.Error()
				// do not retry on expired or missing creds
				if strings.Contains(msg, "expired") ||
					strings.Contains(msg, "NoCredentialProviders") {
					return
				}
			}
		}
	}
}

// Flush the data to CloudWatch
func (p *Sink) Flush() error {
	data := p.Data()
	total := len(data)

	// 20 is the max metrics per request
	for len(data) > 20 {
		put := data[0:20]
		err := p.publishMetricsToCloudWatch(put)
		if err != nil {
			return err
		}
		data = data[20:]
	}

	if len(data) > 0 {
		err := p.publishMetricsToCloudWatch(data)
		if err != nil {
			return err
		}
	}
	if total > 0 {
		logger.KV(xlog.DEBUG, "status", "published", "count", total)
	}

	return nil
}

func (p *Sink) publishMetricsToCloudWatch(data []*cloudwatch.MetricDatum) error {
	if len(data) > 0 {
		in := &cloudwatch.PutMetricDataInput{
			MetricData: data,
			Namespace:  &p.cloudWatchNamespace,
		}
		req, _ := p.cw.PutMetricDataRequest(in)
		req.Handlers.Build.PushBack(compressPayload)
		return req.Send()
	}
	return nil
}

// Compresses the payload before sending it to the API.
// According to the documentation:
// "Each PutMetricData request is limited to 40 KB in size for HTTP POST requests.
// You can send a payload compressed by gzip."
func compressPayload(r *request.Request) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.Copy(zw, r.GetBody()); err != nil {
		logger.KV(xlog.ERROR, "reason", "gzip_copy", "err", err.Error())
		return
	}
	if err := zw.Close(); err != nil {
		logger.KV(xlog.ERROR, "reason", "gzip_close", "err", err.Error())
		return
	}
	r.SetBufferBody(buf.Bytes())
	r.HTTPRequest.Header.Set("Content-Encoding", "gzip")
}

var forbiddenChars = regexp.MustCompile(`[ .=\-/]`)

func (p *Sink) flattenKey(parts []string, labels []metrics.Tag) (string, string) {
	key := strings.Join(parts, "_")
	key = forbiddenChars.ReplaceAllString(key, "_")

	hash := key
	for _, label := range labels {
		hash += fmt.Sprintf(";%s=%s", label.Name, label.Value)
	}

	return key, hash
}

func dimensions(labels []metrics.Tag) []*cloudwatch.Dimension {
	ds := make([]*cloudwatch.Dimension, len(labels))
	for idx, v := range labels {
		ds[idx] = &cloudwatch.Dimension{
			Name:  aws.String(v.Name),
			Value: aws.String(v.Value),
		}
	}

	if len(ds) > 10 {
		logger.Panicf("AWS does not support more than 10 dimentions: %v", ds)
	}
	return ds
}

// SetGauge should retain the last value it is set to
func (p *Sink) SetGauge(parts []string, val float32, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	key, hash := p.flattenKey(parts, tags)
	g, ok := p.gauges[hash]
	if !ok {
		g = &cloudwatch.MetricDatum{
			Unit:       aws.String(cloudwatch.StandardUnitCount),
			MetricName: &key,
			Timestamp:  aws.Time(now),
			Dimensions: dimensions(tags),
			Value:      aws.Float64(float64(val)),
		}
		p.gauges[hash] = g
	} else {
		g.Value = aws.Float64(float64(val))
	}
	p.updates[hash] = time.Now()
}

const oneVal = float64(1)

// AddSample is for timing information, where quantiles are used
func (p *Sink) AddSample(parts []string, val float32, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	val64 := float64(val)
	valPtr := aws.Float64(val64)
	key, hash := p.flattenKey(parts, tags)
	g, ok := p.samples[hash]
	if !ok {
		g = &cloudwatch.MetricDatum{
			Unit:       aws.String(cloudwatch.StandardUnitCount),
			MetricName: &key,
			Timestamp:  aws.Time(now),
			Dimensions: dimensions(tags),
			StatisticValues: &cloudwatch.StatisticSet{
				Minimum:     valPtr,
				Maximum:     valPtr,
				Sum:         valPtr,
				SampleCount: aws.Float64(oneVal),
			},
		}
		p.samples[hash] = g
	} else {
		if val64 < *g.StatisticValues.Minimum {
			g.StatisticValues.Minimum = valPtr
		}
		if val64 > *g.StatisticValues.Maximum {
			g.StatisticValues.Maximum = valPtr
		}
		g.StatisticValues.SampleCount = aws.Float64(*g.StatisticValues.SampleCount + 1)
		g.StatisticValues.Sum = aws.Float64(*g.StatisticValues.Sum + val64)
	}
	p.updates[hash] = time.Now()
}

// IncrCounter should accumulate values
func (p *Sink) IncrCounter(parts []string, val float32, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	key, hash := p.flattenKey(parts, tags)
	g, ok := p.counters[hash]
	if !ok {
		g = &cloudwatch.MetricDatum{
			Unit:       aws.String(cloudwatch.StandardUnitCount),
			MetricName: &key,
			Timestamp:  aws.Time(now),
			Dimensions: dimensions(tags),
			Value:      aws.Float64(float64(val)),
		}
		p.counters[hash] = g
	} else {
		g.Value = aws.Float64(*g.Value + float64(val))
	}
	p.updates[hash] = time.Now()
}

// Data returns collected metrics and allows us to enforce our expiration
// logic to clean up ephemeral metrics if their value haven't been set for a
// duration exceeding our allowed expiration time.
func (p *Sink) Data() []*cloudwatch.MetricDatum {
	p.mu.Lock()
	defer p.mu.Unlock()

	data := make([]*cloudwatch.MetricDatum, 0, len(p.counters)+len(p.gauges)+len(p.samples))

	expire := p.expiration != 0
	now := time.Now()
	for k, v := range p.gauges {
		last := p.updates[k]
		if expire && last.Add(p.expiration).Before(now) {
			delete(p.updates, k)
			delete(p.gauges, k)
		} else {
			data = append(data, v)
		}
	}
	for k, v := range p.samples {
		last := p.updates[k]
		if expire && last.Add(p.expiration).Before(now) {
			delete(p.updates, k)
			delete(p.samples, k)
		} else {
			data = append(data, v)
		}
	}
	for k, v := range p.counters {
		last := p.updates[k]
		if expire && last.Add(p.expiration).Before(now) {
			delete(p.updates, k)
			delete(p.counters, k)
		} else {
			data = append(data, v)
		}
	}
	return data
}
