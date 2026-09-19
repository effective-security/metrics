package cloudwatch

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/cockroachdb/errors"
	"github.com/effective-security/metrics"
	"github.com/effective-security/x/values"
	"github.com/effective-security/xlog"
)

var logger = xlog.NewPackageLogger("github.com/effective-security/metrics", "cloudwatch")

const (
	// defaultPublishInterval is used when Config.PublishInterval is not set.
	defaultPublishInterval = 30 * time.Second
	// defaultMetricsExpiry is used when Config.MetricsExpiry is not set.
	defaultMetricsExpiry = 60 * time.Minute
	// maxMetricsPerRequest is the maximum number of metric data items
	// accepted by a single PutMetricData call.
	maxMetricsPerRequest = 1000
	// maxDimensions is the number of dimensions AWS accepts per metric.
	maxDimensions = 10
	// labelKeySizeHint is the assumed size of one ";name=value" pair,
	// used to pre-size the hash buffer.
	labelKeySizeHint = 24
)

// Publisher provides interface to publish metrics.
// It is the subset of the CloudWatch client the Sink uses, so tests and
// wrappers can substitute their own implementation.
type Publisher interface {
	PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

// Config defines configuration options
type Config struct {
	// AwsRegion is the required AWS Region to use.
	// Falls back to the AWS_REGION and AWS_DEFAULT_REGION environment variables.
	AwsRegion string

	// AwsEndpoint is the optional AWS endpoint to use,
	// for example a local CloudWatch emulator.
	AwsEndpoint string

	// Namespace specifies the namespace under which metrics should be published.
	// It is required.
	Namespace string

	// PublishInterval specifies the frequency with which metrics should be published to Cloudwatch.
	// Defaults to 30s.
	PublishInterval time.Duration

	// MetricsExpiry is the period after which the metrics will be deleted from reporting if not used.
	// Defaults to 60m.
	MetricsExpiry time.Duration

	// WithSampleCount specifies to create additional _count, _sum and _avg metrics for sample
	WithSampleCount bool

	// WithCleanup specifies to clean up published metrics,
	// so each publish reports only the activity of the last interval.
	WithCleanup bool
}

// Sink provides a metrics.Sink that publishes to AWS CloudWatch.
//
// Emissions are aggregated in memory and sent by Run or Flush.
// It is safe for concurrent emission.
type Sink struct {
	// Publisher is the CloudWatch client used to publish.
	// Replace it before Run is started to publish through a custom client.
	Publisher

	mu                        sync.Mutex
	cloudWatchPublishInterval time.Duration
	cloudWatchNamespace       string
	expiration                time.Duration
	withSampleCount           bool
	withCleanup               bool
	gauges                    map[string]*types.MetricDatum
	samples                   map[string]*types.MetricDatum
	counters                  map[string]*types.MetricDatum
	updates                   map[string]time.Time
}

// NewSink initializes and returns a pointer to a CloudWatch Sink using the
// supplied configuration, or an error if there is a problem with the configuration.
//
// It only builds the client; call Run to start publishing.
func NewSink(c *Config) (*Sink, error) {
	sink := &Sink{
		gauges:                    make(map[string]*types.MetricDatum),
		samples:                   make(map[string]*types.MetricDatum),
		counters:                  make(map[string]*types.MetricDatum),
		updates:                   make(map[string]time.Time),
		expiration:                c.MetricsExpiry,
		cloudWatchPublishInterval: c.PublishInterval,
		cloudWatchNamespace:       c.Namespace,
		withSampleCount:           c.WithSampleCount,
		withCleanup:               c.WithCleanup,
	}

	if sink.cloudWatchPublishInterval == 0 {
		sink.cloudWatchPublishInterval = defaultPublishInterval
	}
	if sink.expiration == 0 {
		sink.expiration = defaultMetricsExpiry
	}

	var err error
	sink.Publisher, err = newPublisher(c)
	if err != nil {
		return nil, err
	}

	return sink, nil
}

// Run starts a loop that will push metrics to Cloudwatch at the configured interval.
// Accepts a context.Context to support cancellation.
//
// It returns when ctx is cancelled, after a final flush, or when publishing
// fails with expired or missing credentials, which cannot succeed on a retry.
func (p *Sink) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cloudWatchPublishInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.KV(xlog.DEBUG, "reason", "stopping")
			err := p.Flush(ctx)
			if err != nil {
				logger.KV(xlog.ERROR, "reason", "Flush", "err", err)
			}
			return
		case <-ticker.C:
			logger.KV(xlog.DEBUG, "status", "flush")

			err := p.Flush(ctx)
			if err != nil {
				logger.KV(xlog.ERROR, "reason", "flush", "err", err)
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

// Flush publishes the aggregated data to CloudWatch,
// in batches of at most maxMetricsPerRequest data items.
func (p *Sink) Flush(ctx context.Context) error {
	data := p.Data()
	total := len(data)

	for len(data) > maxMetricsPerRequest {
		put := data[0:maxMetricsPerRequest]
		err := p.Publish(ctx, put)
		if err != nil {
			return err
		}
		data = data[maxMetricsPerRequest:]
	}

	if len(data) > 0 {
		err := p.Publish(ctx, data)
		if err != nil {
			return err
		}
	}
	if total > 0 {
		logger.KV(xlog.DEBUG, "status", "published", "count", total)
	}

	return nil
}

// flattenKey returns the metric name, which is published as-is, and the hash
// that identifies the aggregated datum: the name with its tags appended.
func (p *Sink) flattenKey(key string, labels []metrics.Tag) (string, string) {
	if len(labels) == 0 {
		return key, key
	}

	buf := new(strings.Builder)
	buf.Grow(len(key) + len(labels)*labelKeySizeHint)
	buf.WriteString(key)
	for _, label := range labels {
		buf.WriteByte(';')
		buf.WriteString(label.Name)
		buf.WriteByte('=')
		buf.WriteString(label.Value)
	}

	return key, buf.String()
}

// dimensions converts the tags into CloudWatch dimensions.
// It panics when there are more than maxDimensions tags, see FINDINGS.md #8.
func dimensions(labels []metrics.Tag) []types.Dimension {
	ds := make([]types.Dimension, len(labels))
	for idx, v := range labels {
		ds[idx] = types.Dimension{
			Name:  aws.String(v.Name),
			Value: aws.String(v.Value),
		}
	}

	if len(ds) > maxDimensions {
		logger.Panicf("AWS does not support more than %d dimensions: %v", maxDimensions, ds)
	}
	return ds
}

const (
	oneVal               = float64(1)
	storageResolutionVal = int32(60)
)

// SetGauge retains the last value it is set to, until it is published.
func (p *Sink) SetGauge(key string, val float64, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	key, hash := p.flattenKey(key, tags)
	p.updates[hash] = now
	g, ok := p.gauges[hash]
	if !ok {
		g = &types.MetricDatum{
			Unit:              types.StandardUnitCount,
			MetricName:        &key,
			Timestamp:         aws.Time(now),
			Dimensions:        dimensions(tags),
			Value:             aws.Float64(val),
			StorageResolution: aws.Int32(storageResolutionVal),
		}
		p.gauges[hash] = g
	} else {
		g.Value = aws.Float64(val)
		g.Timestamp = aws.Time(now)
	}
}

// AddSample records an observation into the statistic set (min, max, sum,
// count) published for the metric. CloudWatch derives the quantiles it offers
// from that set, so individual values are not retained.
func (p *Sink) AddSample(key string, val float64, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	valPtr := aws.Float64(val)
	key, hash := p.flattenKey(key, tags)
	p.updates[hash] = now
	g, ok := p.samples[hash]
	if !ok {
		g = &types.MetricDatum{
			Unit:              types.StandardUnitCount,
			MetricName:        aws.String(key),
			Timestamp:         aws.Time(now),
			Dimensions:        dimensions(tags),
			StorageResolution: aws.Int32(storageResolutionVal),
			StatisticValues: &types.StatisticSet{
				Minimum:     valPtr,
				Maximum:     valPtr,
				Sum:         valPtr,
				SampleCount: aws.Float64(oneVal),
			},
		}
		p.samples[hash] = g
	} else {
		if val < *g.StatisticValues.Minimum {
			g.StatisticValues.Minimum = valPtr
		}
		if val > *g.StatisticValues.Maximum {
			g.StatisticValues.Maximum = valPtr
		}
		g.StatisticValues.SampleCount = aws.Float64(*g.StatisticValues.SampleCount + 1)
		g.StatisticValues.Sum = aws.Float64(*g.StatisticValues.Sum + val)
		g.Timestamp = aws.Time(now)
	}
}

// IncrCounter accumulates values until they are published.
func (p *Sink) IncrCounter(key string, val float64, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	key, hash := p.flattenKey(key, tags)
	p.updates[hash] = now
	g, ok := p.counters[hash]
	if !ok {
		g = &types.MetricDatum{
			Unit:              types.StandardUnitCount,
			MetricName:        aws.String(key),
			Timestamp:         aws.Time(now),
			Dimensions:        dimensions(tags),
			StorageResolution: aws.Int32(storageResolutionVal),
			Value:             aws.Float64(val),
		}
		p.counters[hash] = g
	} else {
		g.Value = aws.Float64(*g.Value + val)
		g.Timestamp = aws.Time(now)
	}
}

// Data returns collected metrics and allows us to enforce our expiration
// logic to clean up ephemeral metrics if their value haven't been set for a
// duration exceeding our allowed expiration time.
//
// When Config.WithSampleCount is set, each sample also yields _count, _sum and
// _avg metrics. When Config.WithCleanup is set, the returned data is removed
// from the sink. The returned data shares the statistic sets with the sink,
// so concurrent emissions can still change it; see FINDINGS.md #6.
func (p *Sink) Data() []types.MetricDatum {
	p.mu.Lock()
	defer p.mu.Unlock()

	data := make([]types.MetricDatum, 0, len(p.counters)+len(p.gauges)+len(p.samples))

	expire := p.expiration != 0
	now := time.Now()
	for k, v := range p.gauges {
		last := p.updates[k]
		if expire && last.Add(p.expiration).Before(now) {
			delete(p.updates, k)
			delete(p.gauges, k)
		} else {
			data = append(data, *v)
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
			data = append(data, *v)
			if p.withCleanup {
				delete(p.updates, k)
				delete(p.samples, k)
			}
			if p.withSampleCount {
				data = append(data, types.MetricDatum{
					Unit:              v.Unit,
					MetricName:        aws.String(*v.MetricName + "_count"),
					Timestamp:         v.Timestamp,
					Dimensions:        v.Dimensions,
					StorageResolution: v.StorageResolution,
					Value:             v.StatisticValues.SampleCount,
				})
				data = append(data, types.MetricDatum{
					Unit:              v.Unit,
					MetricName:        aws.String(*v.MetricName + "_sum"),
					Timestamp:         v.Timestamp,
					Dimensions:        v.Dimensions,
					StorageResolution: v.StorageResolution,
					Value:             v.StatisticValues.Sum,
				})
				data = append(data, types.MetricDatum{
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
			data = append(data, *v)
			if p.withCleanup {
				delete(p.updates, k)
				delete(p.counters, k)
			}
		}
	}
	return data
}

// Publish sends one batch of metric data to CloudWatch.
// The batch must not exceed maxMetricsPerRequest items.
func (p *Sink) Publish(ctx context.Context, data []types.MetricDatum) error {
	if len(data) > 0 {
		in := &cloudwatch.PutMetricDataInput{
			MetricData: data,
			Namespace:  &p.cloudWatchNamespace,
		}
		_, err := p.PutMetricData(ctx, in)
		if err != nil {
			logger.KV(xlog.ERROR,
				"reason", "publish",
				"data", data,
				"err", err.Error())
			return errors.Wrap(err, "failed to publish metrics")
		}
	}
	return nil
}

// newPublisher builds the CloudWatch client from the configuration,
// the standard AWS credential chain, and the AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN environment variables
// when they are set.
func newPublisher(c *Config) (Publisher, error) {
	if c.Namespace == "" {
		return nil, errors.New("CloudWatchNamespace required")
	}

	region := values.Coalesce(c.AwsRegion, os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"))
	if region == "" {
		return nil, errors.New("CloudWatchRegion required")
	}

	awsops := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}

	if c.AwsEndpoint != "" {
		// https://aws.github.io/aws-sdk-go-v2/docs/configuring-sdk/endpoints/
		awsops = append(awsops, awsconfig.WithBaseEndpoint(c.AwsEndpoint))
	}

	id := os.Getenv("AWS_ACCESS_KEY_ID")
	secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
	token := os.Getenv("AWS_SESSION_TOKEN")
	if id != "" && secret != "" {
		awsops = append(awsops, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(id, secret, token)))
	}

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsops...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	p := cloudwatch.NewFromConfig(cfg)

	return p, nil
}
