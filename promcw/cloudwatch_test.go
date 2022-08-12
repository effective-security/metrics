package promcw

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBridge(t *testing.T) {
	_, err := NewBridge(&Config{})
	assert.EqualError(t, err, "CloudWatchNamespace required")

	if os.Getenv("AWS_DEFAULT_REGION") == "" {
		_, err = NewBridge(&Config{CloudWatchNamespace: "test"})
		assert.EqualError(t, err, "CloudWatchRegion required")
	}

	// Do not run if there is no connectivity
	if os.Getenv("AWS_ACCESS_KEY_ID") != "" {
		bridge, err := NewBridge(&Config{
			CloudWatchNamespace:       "test",
			AwsRegion:                 "us-west-2",
			CloudWatchPublishInterval: 1 * time.Second,
			CloudWatchPublishTimeout:  100 * time.Millisecond,
		})
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			bridge.Run(ctx)
		}()

		time.Sleep(2 * time.Second)
		cancel()
		wg.Wait()
	}
}

func TestNewBridgeLocal(t *testing.T) {
	os.Setenv("AWS_ACCESS_KEY_ID", "local")
	bridge, err := NewBridge(&Config{
		CloudWatchNamespace:       "test",
		AwsRegion:                 "us-west-2",
		CloudWatchPublishInterval: 1 * time.Second,
		CloudWatchPublishTimeout:  100 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		bridge.Run(ctx)
	}()

	time.Sleep(2 * time.Second)
	cancel()
	wg.Wait()
}
