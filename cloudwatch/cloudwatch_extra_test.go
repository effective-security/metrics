package cloudwatch_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	awscloudwatch "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/effective-security/metrics"
	"github.com/effective-security/metrics/cloudwatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncPublisher records the published data and is safe for concurrent use.
type syncPublisher struct {
	mu   sync.Mutex
	data []types.MetricDatum
	err  error
}

func (m *syncPublisher) PutMetricData(_ context.Context, in *awscloudwatch.PutMetricDataInput, _ ...func(*awscloudwatch.Options)) (*awscloudwatch.PutMetricDataOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	m.data = append(m.data, in.MetricData...)
	return &awscloudwatch.PutMetricDataOutput{}, nil
}

func (m *syncPublisher) published() []types.MetricDatum {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]types.MetricDatum(nil), m.data...)
}

// newTestSink builds a sink that publishes into the returned publisher.
func newTestSink(t *testing.T, cfg *cloudwatch.Config) (*cloudwatch.Sink, *syncPublisher) {
	t.Helper()
	if cfg.AwsRegion == "" {
		cfg.AwsRegion = "us-west-2"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "es"
	}
	sink, err := cloudwatch.NewSink(cfg)
	require.NoError(t, err)

	pub := &syncPublisher{}
	sink.Publisher = pub
	return sink, pub
}

func Test_Data_IsSnapshot(t *testing.T) {
	sink, _ := newTestSink(t, &cloudwatch.Config{
		PublishInterval: time.Hour,
		MetricsExpiry:   time.Hour,
		WithSampleCount: true,
	})

	tags := []metrics.Tag{{Name: "tag1", Value: "val1"}}
	sink.AddSample("test_sample", 10, tags)
	sink.IncrCounter("test_counter", 10, tags)
	sink.SetGauge("test_gauge", 10, tags)

	snapshot := sink.Data()
	require.NotEmpty(t, snapshot)

	// emissions after the snapshot must not change what it holds
	sink.AddSample("test_sample", 90, tags)
	sink.IncrCounter("test_counter", 90, tags)
	sink.SetGauge("test_gauge", 90, tags)

	byName := map[string]types.MetricDatum{}
	for _, d := range snapshot {
		byName[*d.MetricName] = d
	}

	sample := byName["test_sample"]
	require.NotNil(t, sample.StatisticValues)
	assert.Equal(t, float64(10), *sample.StatisticValues.Sum)
	assert.Equal(t, float64(1), *sample.StatisticValues.SampleCount)
	assert.Equal(t, float64(10), *sample.StatisticValues.Maximum)

	// the derived metrics report the same statistic set
	assert.Equal(t, float64(1), *byName["test_sample_count"].Value)
	assert.Equal(t, float64(10), *byName["test_sample_sum"].Value)
	assert.Equal(t, float64(10), *byName["test_sample_avg"].Value)

	assert.Equal(t, float64(10), *byName["test_counter"].Value)
	assert.Equal(t, float64(10), *byName["test_gauge"].Value)

	// while the sink itself accumulated them
	next := sink.Data()
	for _, d := range next {
		switch *d.MetricName {
		case "test_sample":
			assert.Equal(t, float64(100), *d.StatisticValues.Sum)
		case "test_counter":
			assert.Equal(t, float64(100), *d.Value)
		case "test_gauge":
			assert.Equal(t, float64(90), *d.Value)
		}
	}
}

func Test_Dimensions_TooMany(t *testing.T) {
	sink, _ := newTestSink(t, &cloudwatch.Config{
		PublishInterval: time.Hour,
		MetricsExpiry:   time.Hour,
	})

	tags := make([]metrics.Tag, 35)
	for i := range tags {
		tags[i] = metrics.Tag{Name: fmt.Sprintf("tag%02d", i), Value: "v"}
	}

	// more dimensions than AWS accepts must not panic
	require.NotPanics(t, func() {
		sink.SetGauge("test_gauge", 1, tags)
	})

	data := sink.Data()
	require.Len(t, data, 1)
	assert.Len(t, data[0].Dimensions, 30, "dimensions are truncated to the AWS limit")
	assert.Equal(t, "tag00", *data[0].Dimensions[0].Name)
	assert.Equal(t, "tag29", *data[0].Dimensions[29].Name)
}

func Test_Run_FlushesOnShutdown(t *testing.T) {
	// the publish interval never elapses, so only the shutdown flush can publish
	sink, pub := newTestSink(t, &cloudwatch.Config{
		PublishInterval: time.Hour,
		MetricsExpiry:   time.Hour,
		WithCleanup:     true,
	})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		sink.Run(ctx)
	}()

	tags := []metrics.Tag{{Name: "tag1", Value: "val1"}}
	sink.IncrCounter("test_counter", 3, tags)

	cancel()
	<-stopped

	published := pub.published()
	require.Len(t, published, 1, "the last interval must be published after the context ends")
	assert.Equal(t, "test_counter", *published[0].MetricName)
	assert.Equal(t, float64(3), *published[0].Value)
}

func Test_Dimensions_TruncationIsConsistentWithAggregation(t *testing.T) {
	sink, _ := newTestSink(t, &cloudwatch.Config{
		PublishInterval: time.Hour,
		MetricsExpiry:   time.Hour,
	})

	// two emissions that differ only in a tag CloudWatch cannot carry publish
	// as the same series, so they must aggregate into one datum
	base := make([]metrics.Tag, 30)
	for i := range base {
		base[i] = metrics.Tag{Name: fmt.Sprintf("tag%02d", i), Value: "v"}
	}
	first := append(append([]metrics.Tag(nil), base...), metrics.Tag{Name: "dropped", Value: "one"})
	second := append(append([]metrics.Tag(nil), base...), metrics.Tag{Name: "dropped", Value: "two"})

	sink.IncrCounter("test_counter", 1, first)
	sink.IncrCounter("test_counter", 1, second)

	data := sink.Data()
	require.Len(t, data, 1, "the dropped tag must not create a second series")
	assert.Equal(t, float64(2), *data[0].Value)
	assert.Len(t, data[0].Dimensions, 30)
}

func Test_Data_IsDeepCopy(t *testing.T) {
	sink, _ := newTestSink(t, &cloudwatch.Config{
		PublishInterval: time.Hour,
		MetricsExpiry:   time.Hour,
	})

	tags := []metrics.Tag{{Name: "tag1", Value: "val1"}}
	sink.AddSample("test_sample", 10, tags)
	sink.SetGauge("test_gauge", 10, tags)

	data := sink.Data()
	require.Len(t, data, 2)

	// a caller writing through the returned pointers must not reach the sink
	for _, d := range data {
		*d.MetricName = "mutated"
		*d.Timestamp = time.Unix(0, 0)
		*d.StorageResolution = 1
		if d.Value != nil {
			*d.Value = -1
		}
		if d.StatisticValues != nil {
			*d.StatisticValues.Sum = -1
			*d.StatisticValues.Minimum = -1
			*d.StatisticValues.Maximum = -1
			*d.StatisticValues.SampleCount = -1
		}
		for _, dim := range d.Dimensions {
			*dim.Name = "mutated"
			*dim.Value = "mutated"
		}
	}

	next := sink.Data()
	require.Len(t, next, 2)
	for _, d := range next {
		assert.Contains(t, []string{"test_sample", "test_gauge"}, *d.MetricName)
		assert.Equal(t, int32(60), *d.StorageResolution)
		assert.Equal(t, "tag1", *d.Dimensions[0].Name)
		assert.Equal(t, "val1", *d.Dimensions[0].Value)
		if d.Value != nil {
			assert.Equal(t, float64(10), *d.Value)
		}
		if d.StatisticValues != nil {
			assert.Equal(t, float64(10), *d.StatisticValues.Sum)
			assert.Equal(t, float64(1), *d.StatisticValues.SampleCount)
		}
	}
}

func Test_NewSink_NormalizesDurations(t *testing.T) {
	sink, pub := newTestSink(t, &cloudwatch.Config{
		PublishInterval: -time.Second,
		MetricsExpiry:   -time.Second,
	})

	tags := []metrics.Tag{{Name: "tag1", Value: "val1"}}
	sink.IncrCounter("test_counter", 1, tags)

	// a negative expiry would drop every datum as soon as it is read
	require.Len(t, sink.Data(), 1)

	// and a negative interval would panic inside time.NewTicker
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	require.NotPanics(t, func() {
		go func() {
			defer close(stopped)
			sink.Run(ctx)
		}()
	})

	cancel()
	<-stopped
	assert.Len(t, pub.published(), 1)
}
