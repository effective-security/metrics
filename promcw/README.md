# promcw

Prometheus publisher to Cloud Watch

## Configuration

```go
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

	// Prometheus scrape URL. If it's empty, then internal Prometheus handler will be used.
	PrometheusScrapeURL string

	// Additional dimensions to send to CloudWatch
	AdditionalDimensions map[string]string

	// Replace dimensions with the provided label. This allows for aggregating metrics across dimensions so we can set CloudWatch Alarms on the metrics
	ReplaceDimensions map[string]string
}
```

## Usage

```go
	bridge, err := promcw.NewBridge(&c)
	if err != nil {
		return nil, errors.WithStack(err)
	}

    ctx, cancel := context.WithCancel(context.Background())
	bridge.Run(ctx)

    // To stop publisher call cancell when your service stops
    // cancel()
```