package cloudwatch

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/effective-security/metrics"
	"github.com/effective-security/x/values"
	"github.com/effective-security/xlog"
	"github.com/pkg/errors"
)

var logger = xlog.NewPackageLogger("github.com/effective-security/metrics", "cloudwatch")

// Publisher provides interface to publish metrics
type Publisher interface {
	//Publish(ctx context.Context, data []types.MetricDatum) error
	PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

// Config defines configuration options
type Config struct {
	// AwsRegion is the required AWS Region to use
	AwsRegion string

	// AwsEndpoint is the optional AWS endpoint to use
	AwsEndpoint string

	// Namespace specifies the namespace under which metrics should be published.
	Namespace string

	// PublishInterval specifies the frequency with which metrics should be published to Cloudwatch.
	PublishInterval time.Duration

	// // PublishTimeout is the timeout for sending metrics to Cloudwatch.
	// PublishTimeout time.Duration

	// MetricsExpiry is the period after wich the metrics will be deleted from reporting if not used.
	MetricsExpiry time.Duration

	// WithSampleCount specifies to create additional _count and _sum metrics for sample
	WithSampleCount bool

	// WithCleanup specifies to clean up published metrics
	WithCleanup bool
}

// Sink provides a MetricSink that can be used
// with a prometheus server.
type Sink struct {
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
// supplied configuration, or an error if there is a problem with the configuration
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
		sink.cloudWatchPublishInterval = 30 * time.Second
	}
	if sink.expiration == 0 {
		sink.expiration = 60 * time.Minute
	}

	var err error
	sink.Publisher, err = newPublisher(c)
	if err != nil {
		return nil, err
	}

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

// Flush the data to CloudWatch
func (p *Sink) Flush(ctx context.Context) error {
	data := p.Data()
	total := len(data)

	// 1000 is the max metrics per request
	for len(data) > 1000 {
		put := data[0:1000]
		err := p.Publish(ctx, put)
		if err != nil {
			return err
		}
		data = data[1000:]
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

func (p *Sink) flattenKey(key string, labels []metrics.Tag) (string, string) {
	hash := key
	for _, label := range labels {
		hash += fmt.Sprintf(";%s=%s", label.Name, label.Value)
	}

	return key, hash
}

func dimensions(labels []metrics.Tag) []types.Dimension {
	ds := make([]types.Dimension, len(labels))
	for idx, v := range labels {
		ds[idx] = types.Dimension{
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
	storageResolutionVal = int32(60)
)

// SetGauge should retain the last value it is set to
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
			Value:             aws.Float64(float64(val)),
			StorageResolution: aws.Int32(storageResolutionVal),
		}
		p.gauges[hash] = g
	} else {
		g.Value = aws.Float64(float64(val))
		g.Timestamp = aws.Time(now)
	}
}

// AddSample is for timing information, where quantiles are used
func (p *Sink) AddSample(key string, val float64, tags []metrics.Tag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	val64 := float64(val)
	valPtr := aws.Float64(val64)
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
			Value:             aws.Float64(float64(val)),
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

// Publish metrics
func (p *Sink) Publish(ctx context.Context, data []types.MetricDatum) error {
	if len(data) > 0 {
		in := &cloudwatch.PutMetricDataInput{
			MetricData: data,
			Namespace:  &p.cloudWatchNamespace,
		}
		_, err := p.Publisher.PutMetricData(ctx, in)
		if err != nil {
			return errors.Wrap(err, "failed to publish metrics")
		}
	}
	return nil
}

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
		customResolver := aws.EndpointResolverWithOptionsFunc(func(svc, reg string, options ...any) (aws.Endpoint, error) {
			if svc == cloudwatch.ServiceID && reg == region {
				ep := aws.Endpoint{
					PartitionID:   "aws",
					URL:           c.AwsEndpoint,
					SigningRegion: region,
				}
				return ep, nil
			}
			// returning EndpointNotFoundError will allow the service to fallback to it's default resolution
			return aws.Endpoint{}, &aws.EndpointNotFoundError{}
		})
		awsops = append(awsops, awsconfig.WithEndpointResolverWithOptions(customResolver))
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
