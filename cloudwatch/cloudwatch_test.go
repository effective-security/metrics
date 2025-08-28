package cloudwatch_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	awscloudwatch "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
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

func Test_Sink(t *testing.T) {
	xlog.SetGlobalLogLevel(xlog.DEBUG)
	_ = os.Setenv("AWS_DEFAULT_REGION", "")

	cfg := cloudwatch.Config{}
	_, err := cloudwatch.NewSink(&cfg)
	assert.EqualError(t, err, "CloudWatchNamespace required")

	cfg = cloudwatch.Config{
		Namespace: "es",
	}
	_, err = cloudwatch.NewSink(&cfg)
	assert.EqualError(t, err, "CloudWatchRegion required")

	mock := &mockPublisher{t: t}
	cfg = cloudwatch.Config{
		AwsRegion:       "us-west-2",
		Namespace:       "es",
		PublishInterval: 200 * time.Millisecond,
		MetricsExpiry:   100 * time.Millisecond,
		WithSampleCount: true,
		WithCleanup:     true,
	}
	s, err := cloudwatch.NewSink(&cfg)
	require.NoError(t, err)
	s.Publisher = mock

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go s.Run(ctx)

	tags := []metrics.Tag{{Name: "tag1", Value: "val1"}}
	for i := 0; i < 10; i++ {
		s.IncrCounter(fmt.Sprintf("test_counter_%d", i%3), 1, tags)
		s.SetGauge(fmt.Sprintf("test_gauge_%d", i%3), 1, tags)
		s.AddSample(fmt.Sprintf("test_sample_%d", i%3), 1, tags)
	}

	time.Sleep(200 * time.Millisecond)
	s.AddSample("test_sample", 1, tags)
	s.IncrCounter("test_counter", 1, tags)
	s.SetGauge("test_gauge2", 1, tags)

	//	time.Sleep(1 * time.Second)
	err = s.Flush(ctx)
	assert.NoError(t, err)

	cancel()
	assert.Len(t, mock.data, 6)
}

type mockPublisher struct {
	data []types.MetricDatum
	t    *testing.T
}

func (m *mockPublisher) PutMetricData(ctx context.Context, in *awscloudwatch.PutMetricDataInput, optFns ...func(*awscloudwatch.Options)) (*awscloudwatch.PutMetricDataOutput, error) {
	m.t.Logf("received %d", len(in.MetricData))
	m.data = append(m.data, in.MetricData...)
	return &awscloudwatch.PutMetricDataOutput{}, nil
}
