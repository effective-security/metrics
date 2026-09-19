package cloudwatch

import (
	"context"
	"os"
	"slices"
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
	maxDimensions = 30
	// shutdownFlushTimeout bounds the final flush after the context ends.
	shutdownFlushTimeout = 10 * time.Second
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
// PublishInterval and MetricsExpiry fall back to their defaults when they are
// not positive. It only builds the client; call Run to start publishing.
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

	// a non-positive duration is a misconfiguration, not a request to publish
	// continuously: PublishInterval reaches time.NewTicker, which panics on
	// one, and a negative expiry would drop every datum on the next flush
	if sink.cloudWatchPublishInterval <= 0 {
		sink.cloudWatchPublishInterval = defaultPublishInterval
	}
	if sink.expiration <= 0 {
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
			// ctx is already done, so publishing with it would fail before the
			// request is sent; keep its values and give the last flush its own
			// deadline
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownFlushTimeout)
			err := p.Flush(flushCtx)
			cancel()
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

// limitTags drops the tags beyond what CloudWatch accepts as dimensions, and
// reports which ones. It runs before the aggregation key is built, so that two
// emissions differing only in a dropped tag aggregate into the one datum they
// are published as, instead of fighting over the same series from two map
// entries.
//
// Losing a dimension is better than failing the whole batch, and far better
// than panicking in the goroutine that happened to emit.
func limitTags(key string, labels []metrics.Tag) []metrics.Tag {
	if len(labels) <= maxDimensions {
		return labels
	}

	logger.KV(xlog.ERROR,
		"reason", "too_many_dimensions",
		"metric", key,
		"allowed", maxDimensions,
		"provided", len(labels),
		"dropped", tagNames(labels[maxDimensions:]),
	)
	return labels[:maxDimensions]
}

// dimensions converts the tags into CloudWatch dimensions.
// The tags must already have been limited by limitTags.
func dimensions(labels []metrics.Tag) []types.Dimension {
	ds := make([]types.Dimension, len(labels))
	for idx, v := range labels {
		ds[idx] = types.Dimension{
			Name:  aws.String(v.Name),
			Value: aws.String(v.Value),
		}
	}

	return ds
}

// tagNames lists the tag names, for the error reporting the dropped ones.
func tagNames(labels []metrics.Tag) []string {
	names := make([]string, len(labels))
	for idx, v := range labels {
		names[idx] = v.Name
	}
	return names
}

const (
	oneVal               = float64(1)
	storageResolutionVal = int32(60)
)

// SetGauge retains the last value it is set to, until it is published.
func (p *Sink) SetGauge(key string, val float64, tags []metrics.Tag) {
	tags = limitTags(key, tags)

	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	key, hash := p.flattenKey(key, tags)
	p.updates[hash] = now
	g, ok := p.gauges[hash]
	if !ok {
		g = &types.MetricDatum{
			Unit:              types.StandardUnitCount,
			MetricName:        aws.String(key),
			Timestamp:         aws.Time(now),
			Dimensions:        dimensions(tags),
			Value:             aws.Float64(val),
			StorageResolution: aws.Int32(storageResolutionVal),
		}
		p.gauges[hash] = g
	} else {
		*g.Value = val
		g.Timestamp = aws.Time(now)
	}
}

// AddSample records an observation into the statistic set (min, max, sum,
// count) published for the metric.
//
// Individual values are not retained, so CloudWatch reports Minimum, Maximum,
// Sum, SampleCount and Average for the metric, but cannot compute percentiles
// from it. Set Config.WithSampleCount to publish the count, sum and average as
// metrics of their own.
func (p *Sink) AddSample(key string, val float64, tags []metrics.Tag) {
	tags = limitTags(key, tags)

	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	key, hash := p.flattenKey(key, tags)
	p.updates[hash] = now
	g, ok := p.samples[hash]
	if !ok {
		// one pointer per statistic: they are updated independently below
		g = &types.MetricDatum{
			Unit:              types.StandardUnitCount,
			MetricName:        aws.String(key),
			Timestamp:         aws.Time(now),
			Dimensions:        dimensions(tags),
			StorageResolution: aws.Int32(storageResolutionVal),
			StatisticValues: &types.StatisticSet{
				Minimum:     aws.Float64(val),
				Maximum:     aws.Float64(val),
				Sum:         aws.Float64(val),
				SampleCount: aws.Float64(oneVal),
			},
		}
		p.samples[hash] = g
	} else {
		stats := g.StatisticValues
		if val < *stats.Minimum {
			*stats.Minimum = val
		}
		if val > *stats.Maximum {
			*stats.Maximum = val
		}
		*stats.SampleCount++
		*stats.Sum += val
		g.Timestamp = aws.Time(now)
	}
}

// IncrCounter accumulates values until they are published.
func (p *Sink) IncrCounter(key string, val float64, tags []metrics.Tag) {
	tags = limitTags(key, tags)

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
		*g.Value += val
		g.Timestamp = aws.Time(now)
	}
}

// Data returns collected metrics and allows us to enforce our expiration
// logic to clean up ephemeral metrics if their value haven't been set for a
// duration exceeding our allowed expiration time.
//
// When Config.WithSampleCount is set, each sample also yields _count, _sum and
// _avg metrics. When Config.WithCleanup is set, the returned data is removed
// from the sink. The returned data is a deep copy: it shares nothing with the
// sink, so it stays stable while Publish serializes it and other goroutines
// keep emitting, and a caller may modify it freely.
func (p *Sink) Data() []types.MetricDatum {
	p.mu.Lock()
	defer p.mu.Unlock()

	data := make([]types.MetricDatum, 0, len(p.counters)+len(p.gauges)+len(p.samples))

	expire := p.expiration > 0
	now := time.Now()
	for k, v := range p.gauges {
		last := p.updates[k]
		if expire && last.Add(p.expiration).Before(now) {
			delete(p.updates, k)
			delete(p.gauges, k)
		} else {
			data = append(data, cloneDatum(v))
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
			// derive the extra metrics from the snapshot, not from the live
			// datum, so all four report the same statistic set
			datum := cloneDatum(v)
			data = append(data, datum)
			if p.withCleanup {
				delete(p.updates, k)
				delete(p.samples, k)
			}
			if p.withSampleCount && datum.StatisticValues != nil {
				stats := datum.StatisticValues
				data = append(data,
					derivedDatum(&datum, sampleCountSuffix, *stats.SampleCount),
					derivedDatum(&datum, sampleSumSuffix, *stats.Sum),
					derivedDatum(&datum, sampleAvgSuffix, *stats.Sum / *stats.SampleCount),
				)
			}
		}
	}
	for k, v := range p.counters {
		last := p.updates[k]
		if expire && last.Add(p.expiration).Before(now) {
			delete(p.updates, k)
			delete(p.counters, k)
		} else {
			data = append(data, cloneDatum(v))
			if p.withCleanup {
				delete(p.updates, k)
				delete(p.counters, k)
			}
		}
	}
	return data
}

// Suffixes of the metrics derived from a sample when Config.WithSampleCount
// is set.
const (
	sampleCountSuffix = "_count"
	sampleSumSuffix   = "_sum"
	sampleAvgSuffix   = "_avg"
)

// derivedDatum builds one of the WithSampleCount metrics from a sample datum
// that is already a private copy. The result is a private copy too, so a caller
// editing one datum of the returned batch does not edit its siblings.
func derivedDatum(sample *types.MetricDatum, suffix string, value float64) types.MetricDatum {
	datum := cloneDatum(sample)
	datum.MetricName = aws.String(*sample.MetricName + suffix)
	datum.StatisticValues = nil
	datum.Value = aws.Float64(value)
	return datum
}

// cloneDatum returns a copy of the datum that shares nothing with the sink:
// every pointer field is duplicated, so neither a later emission nor a caller
// writing through the returned data can change what the other sees.
func cloneDatum(v *types.MetricDatum) types.MetricDatum {
	datum := types.MetricDatum{
		Unit:              v.Unit,
		MetricName:        clonePtr(v.MetricName),
		Value:             clonePtr(v.Value),
		Timestamp:         clonePtr(v.Timestamp),
		StorageResolution: clonePtr(v.StorageResolution),
		Values:            slices.Clone(v.Values),
		Counts:            slices.Clone(v.Counts),
	}

	if v.StatisticValues != nil {
		datum.StatisticValues = &types.StatisticSet{
			Minimum:     clonePtr(v.StatisticValues.Minimum),
			Maximum:     clonePtr(v.StatisticValues.Maximum),
			Sum:         clonePtr(v.StatisticValues.Sum),
			SampleCount: clonePtr(v.StatisticValues.SampleCount),
		}
	}

	if len(v.Dimensions) > 0 {
		datum.Dimensions = make([]types.Dimension, len(v.Dimensions))
		for idx, d := range v.Dimensions {
			datum.Dimensions[idx] = types.Dimension{
				Name:  clonePtr(d.Name),
				Value: clonePtr(d.Value),
			}
		}
	}

	return datum
}

// clonePtr duplicates the pointed-to value, keeping a nil pointer nil.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
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
			// the batch holds up to maxMetricsPerRequest datums; log its size,
			// not its content
			logger.KV(xlog.ERROR,
				"reason", "publish",
				"namespace", p.cloudWatchNamespace,
				"count", len(data),
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
