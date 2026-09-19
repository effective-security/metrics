package cloudwatch_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/effective-security/metrics"
	"github.com/effective-security/metrics/cloudwatch"
	"github.com/effective-security/xlog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSinkInterface(t *testing.T) {
	var ps *cloudwatch.Sink
	_ = metrics.Sink(ps)
}

func Test_NewSink_Validation(t *testing.T) {
	xlog.SetGlobalLogLevel(xlog.DEBUG)
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_REGION", "")

	cfg := cloudwatch.Config{}
	_, err := cloudwatch.NewSink(&cfg)
	assert.EqualError(t, err, "CloudWatchNamespace required")

	cfg = cloudwatch.Config{
		Namespace: "es",
	}
	_, err = cloudwatch.NewSink(&cfg)
	assert.EqualError(t, err, "CloudWatchRegion required")

	// the region falls back to the environment
	t.Setenv("AWS_DEFAULT_REGION", "us-west-2")
	sink, err := cloudwatch.NewSink(&cfg)
	require.NoError(t, err)
	require.NotNil(t, sink)
}

func Test_Sink(t *testing.T) {
	// the publish interval never elapses, so nothing publishes concurrently
	// with the explicit Flush calls below
	sink, pub := newTestSink(t, &cloudwatch.Config{
		PublishInterval: time.Hour,
		MetricsExpiry:   time.Hour,
		WithSampleCount: true,
		WithCleanup:     true,
	})

	tags := []metrics.Tag{{Name: "tag1", Value: "val1"}}
	for i := range 10 {
		sink.IncrCounter(fmt.Sprintf("test_counter_%d", i%3), 1, tags)
		sink.SetGauge(fmt.Sprintf("test_gauge_%d", i%3), 1, tags)
		sink.AddSample(fmt.Sprintf("test_sample_%d", i%3), 1, tags)
	}

	ctx := context.Background()
	require.NoError(t, sink.Flush(ctx))

	// 3 counters, 3 gauges, and 3 samples with their _count, _sum and _avg
	published := pub.published()
	require.Len(t, published, 18)

	byName := map[string]float64{}
	for _, d := range published {
		if d.Value != nil {
			byName[*d.MetricName] = *d.Value
		}
	}
	// 10 emissions spread over 3 keys: 4, 3 and 3
	assert.Equal(t, float64(4), byName["test_counter_0"])
	assert.Equal(t, float64(3), byName["test_counter_1"])
	assert.Equal(t, float64(1), byName["test_gauge_0"], "a gauge keeps the last value")
	assert.Equal(t, float64(4), byName["test_sample_0_count"])
	assert.Equal(t, float64(4), byName["test_sample_0_sum"])
	assert.Equal(t, float64(1), byName["test_sample_0_avg"])

	// WithCleanup: what was published is gone, so a second flush sends nothing
	require.NoError(t, sink.Flush(ctx))
	assert.Len(t, pub.published(), 18)

	// and new emissions are published on the next flush
	sink.IncrCounter("test_counter", 1, tags)
	require.NoError(t, sink.Flush(ctx))
	assert.Len(t, pub.published(), 19)
}

func Test_Sink_Expiry(t *testing.T) {
	const expiry = 20 * time.Millisecond
	sink, pub := newTestSink(t, &cloudwatch.Config{
		PublishInterval: time.Hour,
		MetricsExpiry:   expiry,
	})

	tags := []metrics.Tag{{Name: "tag1", Value: "val1"}}
	sink.IncrCounter("test_counter", 1, tags)
	sink.SetGauge("test_gauge", 1, tags)
	sink.AddSample("test_sample", 1, tags)

	// idle for longer than the expiry: the data is dropped, not published
	time.Sleep(3 * expiry)
	require.NoError(t, sink.Flush(context.Background()))
	assert.Empty(t, pub.published(), "expired data must not be published")

	// a fresh emission is published as usual
	sink.IncrCounter("test_counter", 1, tags)
	require.NoError(t, sink.Flush(context.Background()))
	assert.Len(t, pub.published(), 1)
}

func Test_Run_PublishesOnInterval(t *testing.T) {
	sink, pub := newTestSink(t, &cloudwatch.Config{
		PublishInterval: 10 * time.Millisecond,
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
	sink.IncrCounter("test_counter", 1, tags)

	// the ticker publishes without any explicit Flush
	require.Eventually(t, func() bool {
		return len(pub.published()) == 1
	}, time.Second, 5*time.Millisecond)

	cancel()
	<-stopped
	assert.Len(t, pub.published(), 1, "nothing was left to publish on shutdown")
}

func Test_Run_StopsOnCredentialErrors(t *testing.T) {
	sink, pub := newTestSink(t, &cloudwatch.Config{
		PublishInterval: 10 * time.Millisecond,
		MetricsExpiry:   time.Hour,
	})
	pub.err = fmt.Errorf("operation error CloudWatch: PutMetricData, NoCredentialProviders")

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		sink.Run(context.Background())
	}()

	sink.IncrCounter("test_counter", 1, nil)

	// the loop gives up on credentials that cannot be retried, without the
	// context being cancelled
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run must return when the credentials are missing")
	}
	assert.Empty(t, pub.published())
}

func TestMain(m *testing.M) {
	// the sink tests never reach AWS; make sure a developer environment does
	// not leak credentials or a region into them
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}
