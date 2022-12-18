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
	// AwsRegion is the required AWS Region to use
	AwsRegion string

	// Namespace specifies the namespace under which metrics should be published.
	Namespace string

	// PublishInterval specifies the frequency with which metrics should be published to Cloudwatch.
	PublishInterval time.Duration

	// PublishTimeout is the timeout for sending metrics to Cloudwatch.
	PublishTimeout time.Duration

	// MetricsExpiry is the period after wich the metrics will be deleted from reporting if not used.
	MetricsExpiry time.Duration

	// Validate specifies to validate metrics and panic if invalid.
	// Set this option during development
	Validate bool

	// WithSampleCount specifies to create additional _count and _sum metrics for sample
	WithSampleCount bool

	// WithCleanup specifies to clean up published metrics
	WithCleanup bool
}

// Sink provides a MetricSink that can be used
// with a prometheus server.
type Sink struct {
	mu                        sync.Mutex
	cloudWatchPublishInterval time.Duration
	cloudWatchNamespace       string
	cw                        *cloudwatch.CloudWatch
	expiration                time.Duration
	validate                  bool
	withSampleCount           bool
	withCleanup               bool
	gauges                    map[string]*cloudwatch.MetricDatum
	samples                   map[string]*cloudwatch.MetricDatum
	counters                  map[string]*cloudwatch.MetricDatum
	updates                   map[string]time.Time
}

// NewSink initializes and returns a pointer to a CloudWatch Sink using the
// supplied configuration, or an error if there is a problem with the configuration
func NewSink(c *Config) (*Sink, error) {
	if c.Namespace == "" {
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
		gauges:                    make(map[string]*cloudwatch.MetricDatum),
		samples:                   make(map[string]*cloudwatch.MetricDatum),
		counters:                  make(map[string]*cloudwatch.MetricDatum),
		updates:                   make(map[string]time.Time),
		expiration:                c.MetricsExpiry,
		cloudWatchPublishInterval: c.PublishInterval,
		cloudWatchNamespace:       c.Namespace,
		validate:                  c.Validate,
		withSampleCount:           c.WithSampleCount,
		withCleanup:               c.WithCleanup,
	}

	if sink.cloudWatchPublishInterval == 0 {
		sink.cloudWatchPublishInterval = 30 * time.Second
	}
	if sink.expiration == 0 {
		sink.expiration = 60 * time.Minute
	}

	var client = http.DefaultClient
	if c.PublishTimeout > 0 {
		client.Timeout = c.PublishTimeout
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
		err := req.Send()
		if err != nil {
			return errors.Wrap(err, "failed to publish metrics")
		}
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

const (
	oneVal               = float64(1)
	storageResolutionVal = int64(60)
)

// SetGauge should retain the last value it is set to
func (p *Sink) SetGauge(parts []string, val float64, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	key, hash := p.flattenKey(parts, tags)
	p.updates[hash] = now
	g, ok := p.gauges[hash]
	if !ok {
		g = &cloudwatch.MetricDatum{
			Unit:              aws.String(cloudwatch.StandardUnitCount),
			MetricName:        &key,
			Timestamp:         aws.Time(now),
			Dimensions:        dimensions(tags),
			Value:             aws.Float64(float64(val)),
			StorageResolution: aws.Int64(storageResolutionVal),
		}
		if p.validate {
			if err := g.Validate(); err != nil {
				logger.Panicf("validation failed: %s: %s", err.Error(), g.String())
			}
		}
		p.gauges[hash] = g
	} else {
		g.Value = aws.Float64(float64(val))
		g.Timestamp = aws.Time(now)
	}
}

// AddSample is for timing information, where quantiles are used
func (p *Sink) AddSample(parts []string, val float64, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	val64 := float64(val)
	valPtr := aws.Float64(val64)
	key, hash := p.flattenKey(parts, tags)
	p.updates[hash] = now
	g, ok := p.samples[hash]
	if !ok {
		g = &cloudwatch.MetricDatum{
			Unit:              aws.String(cloudwatch.StandardUnitCount),
			MetricName:        aws.String(key),
			Timestamp:         aws.Time(now),
			Dimensions:        dimensions(tags),
			StorageResolution: aws.Int64(storageResolutionVal),
			StatisticValues: &cloudwatch.StatisticSet{
				Minimum:     valPtr,
				Maximum:     valPtr,
				Sum:         valPtr,
				SampleCount: aws.Float64(oneVal),
			},
		}
		if p.validate {
			if err := g.Validate(); err != nil {
				logger.Panicf("validation failed: %s: %s", err.Error(), g.String())
			}
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
		g.Timestamp = aws.Time(now)
	}
}

// IncrCounter should accumulate values
func (p *Sink) IncrCounter(parts []string, val float64, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	key, hash := p.flattenKey(parts, tags)
	p.updates[hash] = now
	g, ok := p.counters[hash]
	if !ok {
		g = &cloudwatch.MetricDatum{
			Unit:              aws.String(cloudwatch.StandardUnitCount),
			MetricName:        aws.String(key),
			Timestamp:         aws.Time(now),
			Dimensions:        dimensions(tags),
			StorageResolution: aws.Int64(storageResolutionVal),
			Value:             aws.Float64(float64(val)),
		}
		if p.validate {
			if err := g.Validate(); err != nil {
				logger.Panicf("validation failed: %s: %s", err.Error(), g.String())
			}
		}
		p.counters[hash] = g
	} else {
		g.Value = aws.Float64(*g.Value + float64(val))
		g.Timestamp = aws.Time(now)
	}
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
			if p.withCleanup {
				delete(p.updates, k)
				delete(p.gauges, k)
			}
		}
	}
	for k, v := range p.samples {
		last := p.updates[k]
		if expire && last.Add(p.expiration).Before(now) {
			delete(p.updates, k)
			delete(p.samples, k)
		} else {
			data = append(data, v)
			if p.withCleanup {
				delete(p.updates, k)
				delete(p.samples, k)
			}
			if p.withSampleCount {
				data = append(data, &cloudwatch.MetricDatum{
					Unit:              v.Unit,
					MetricName:        aws.String(*v.MetricName + "_count"),
					Timestamp:         v.Timestamp,
					Dimensions:        v.Dimensions,
					StorageResolution: v.StorageResolution,
					Value:             v.StatisticValues.SampleCount,
				})
				data = append(data, &cloudwatch.MetricDatum{
					Unit:              v.Unit,
					MetricName:        aws.String(*v.MetricName + "_sum"),
					Timestamp:         v.Timestamp,
					Dimensions:        v.Dimensions,
					StorageResolution: v.StorageResolution,
					Value:             v.StatisticValues.Sum,
				})
				data = append(data, &cloudwatch.MetricDatum{
					Unit:              v.Unit,
					MetricName:        aws.String(*v.MetricName + "_avg"),
					Timestamp:         v.Timestamp,
					Dimensions:        v.Dimensions,
					StorageResolution: v.StorageResolution,
					Value:             aws.Float64(*v.StatisticValues.Sum / *v.StatisticValues.SampleCount),
				})
			}
		}
	}
	for k, v := range p.counters {
		last := p.updates[k]
		if expire && last.Add(p.expiration).Before(now) {
			delete(p.updates, k)
			delete(p.counters, k)
		} else {
			data = append(data, v)
			if p.withCleanup {
				delete(p.updates, k)
				delete(p.counters, k)
			}
		}
	}
	return data
}
