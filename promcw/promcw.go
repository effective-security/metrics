package promcw

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"io"
	"math"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/effective-security/xlog"
	"github.com/matttproud/golang_protobuf_extensions/pbutil"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

var logger = xlog.NewPackageLogger("github.com/effective-security/metrics", "promcw")

const (
	batchSize      = 10
	cwHighResLabel = "__cw_high_res"
	cwUnitLabel    = "__cw_unit"
	acceptHeader   = `application/vnd.google.protobuf;proto=io.prometheus.client.MetricFamily;encoding=delimited;q=0.7,text/plain;version=0.0.4;q=0.3`
)

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

// Bridge pushes metrics to AWS CloudWatch
type Bridge struct {
	cloudWatchPublishInterval time.Duration
	cloudWatchNamespace       string
	cw                        *cloudwatch.CloudWatch
	prometheusScrapeURL       string
	additionalDimensions      map[string]string
	replaceDimensions         map[string]string
}

// NewBridge initializes and returns a pointer to a Bridge using the
// supplied configuration, or an error if there is a problem with the configuration
func NewBridge(c *Config) (*Bridge, error) {
	b := new(Bridge)

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

	b.cloudWatchNamespace = c.CloudWatchNamespace
	b.prometheusScrapeURL = c.PrometheusScrapeURL
	b.additionalDimensions = c.AdditionalDimensions
	b.replaceDimensions = c.ReplaceDimensions

	if c.CloudWatchPublishInterval > 0 {
		b.cloudWatchPublishInterval = c.CloudWatchPublishInterval
	} else {
		b.cloudWatchPublishInterval = 30 * time.Second
	}

	var client = http.DefaultClient

	if c.CloudWatchPublishTimeout > 0 {
		client.Timeout = c.CloudWatchPublishTimeout
	} else {
		client.Timeout = 5 * time.Second
	}

	config := aws.NewConfig().WithHTTPClient(client).WithRegion(region)
	sess, err := session.NewSession(config)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	b.cw = cloudwatch.New(sess)
	return b, nil
}

// Run starts a loop that will push metrics to Cloudwatch at the configured interval. Accepts a context.Context to support cancellation
func (b *Bridge) Run(ctx context.Context) {
	ticker := time.NewTicker(b.cloudWatchPublishInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.KV(xlog.DEBUG, "reason", "stopping")
			return
		case <-ticker.C:
			mfChan := make(chan *dto.MetricFamily, 1024)

			go b.fetchMetricFamilies(mfChan)

			var metricFamilies []*dto.MetricFamily
			for mf := range mfChan {
				metricFamilies = append(metricFamilies, mf)
			}

			count, err := b.publishMetricsToCloudWatch(metricFamilies)
			if err != nil {
				logger.KV(xlog.ERROR, "reason", "publishMetricsToCloudWatch", "err", err)
				msg := err.Error()
				// do not retry on expired or missing creds
				if strings.Contains(msg, "expired") ||
					strings.Contains(msg, "NoCredentialProviders") {
					return
				}
			} else {
				logger.KV(xlog.DEBUG, "reason", "publishMetricsToCloudWatch", "count", count)
			}
		}
	}
}

// NOTE: The CloudWatch API has the following limitations:
//   - Max 40kb request size
//   - Single namespace per request
//   - Max 10 dimensions per metric
func (b *Bridge) publishMetricsToCloudWatch(mfs []*dto.MetricFamily) (count int, e error) {
	vec, err := expfmt.ExtractSamples(&expfmt.DecodeOptions{Timestamp: model.Now()}, mfs...)
	if err != nil {
		return 0, errors.WithStack(err)
	}

	data := make([]*cloudwatch.MetricDatum, 0, batchSize)

	for _, s := range vec {
		name := getName(s.Metric)
		/*
			if b.shouldIgnoreMetric(name) {
				continue
			}
		*/
		data = appendDatum(data, name, s, b)
		if len(data) == batchSize {
			count += batchSize
			if err := b.flush(data); err != nil {
				logger.KV(xlog.ERROR, "reason", "flush", "err", err.Error())
				return 0, errors.WithStack(err)
			}
			data = make([]*cloudwatch.MetricDatum, 0, batchSize)
		}
	}

	count += len(data)
	return count, b.flush(data)
}

func (b *Bridge) flush(data []*cloudwatch.MetricDatum) error {
	//logger.Debugf("data=%d", len(data))
	if len(data) > 0 {
		in := &cloudwatch.PutMetricDataInput{
			MetricData: data,
			Namespace:  &b.cloudWatchNamespace,
		}
		req, _ := b.cw.PutMetricDataRequest(in)
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

func appendDatum(data []*cloudwatch.MetricDatum, name string, s *model.Sample, b *Bridge) []*cloudwatch.MetricDatum {
	metric := s.Metric

	if len(metric) == 0 {
		return data
	}

	// Check value before adding the datum
	value := float64(s.Value)
	if !validValue(value) {
		return data
	}

	datum := new(cloudwatch.MetricDatum)

	kubeStateDimensions, replacedDimensions := getDimensions(metric, 10-len(b.additionalDimensions), b)
	datum.SetMetricName(name).
		SetValue(value).
		SetTimestamp(s.Timestamp.Time()).
		SetDimensions(append(kubeStateDimensions, getAdditionalDimensions(b)...)).
		SetStorageResolution(getResolution(metric)).
		SetUnit(getUnit(metric))
	data = append(data, datum)

	// Don't add replacement if not configured
	if replacedDimensions != nil && len(replacedDimensions) > 0 {
		replacedDimensionDatum := &cloudwatch.MetricDatum{}
		replacedDimensionDatum.SetMetricName(name).
			SetValue(value).
			SetTimestamp(s.Timestamp.Time()).
			SetDimensions(append(replacedDimensions, getAdditionalDimensions(b)...)).
			SetStorageResolution(getResolution(metric)).
			SetUnit(getUnit(metric))
		data = append(data, replacedDimensionDatum)
	}

	return data
}

var (
	valueTooSmall = math.Pow(2, -260)
	valueTooLarge = math.Pow(2, 260)
)

// According to the documentation:
// "CloudWatch rejects values that are either too small or too large.
// Values must be in the range of 8.515920e-109 to 1.174271e+108 (Base 10)
// or 2e-360 to 2e360 (Base 2).
// In addition, special values (for example, NaN, +Infinity, -Infinity) are not supported."
func validValue(v float64) bool {
	if math.IsInf(v, 0) {
		return false
	}
	if math.IsNaN(v) {
		return false
	}
	// Check for zero first to avoid tripping on "value too small"
	if v == 0.0 {
		return true
	}
	// Check that a non-zero value is within the range of accepted values
	a := math.Abs(v)
	if a <= valueTooSmall || a >= valueTooLarge {
		return false
	}
	return true
}

func getName(m model.Metric) string {
	if n, ok := m[model.MetricNameLabel]; ok {
		return string(n)
	}
	return ""
}

// getDimensions returns up to 10 dimensions for the provided metric - one for each label (except the __name__ label)
// If a metric has more than 10 labels, it attempts to behave deterministically and returning the first 10 labels as dimensions
func getDimensions(m model.Metric, num int, b *Bridge) ([]*cloudwatch.Dimension, []*cloudwatch.Dimension) {
	if len(m) == 0 {
		return make([]*cloudwatch.Dimension, 0), nil
	} else if _, ok := m[model.MetricNameLabel]; len(m) == 1 && ok {
		return make([]*cloudwatch.Dimension, 0), nil
	}

	names := make([]string, 0, len(m))
	for k := range m {
		if !(k == model.MetricNameLabel || k == cwHighResLabel || k == cwUnitLabel) {
			names = append(names, string(k))
		}
	}

	sort.Strings(names)
	dims := make([]*cloudwatch.Dimension, 0, len(names))
	replacedDims := make([]*cloudwatch.Dimension, 0, len(names))

	for _, name := range names {
		if name != "" {
			val := string(m[model.LabelName(name)])
			if val != "" {
				dims = append(dims, new(cloudwatch.Dimension).SetName(name).SetValue(val))
				// Don't add replacement if not configured
				if b.replaceDimensions != nil && len(b.replaceDimensions) > 0 {
					if replacement, ok := b.replaceDimensions[name]; ok {
						replacedDims = append(replacedDims, new(cloudwatch.Dimension).SetName(name).SetValue(replacement))
					} else {
						replacedDims = append(replacedDims, new(cloudwatch.Dimension).SetName(name).SetValue(val))
					}
				}
			}
		}
	}

	if len(dims) > num {
		dims = dims[:num]
	}

	if len(replacedDims) > num {
		replacedDims = replacedDims[:num]
	}

	return dims, replacedDims
}

func getAdditionalDimensions(b *Bridge) []*cloudwatch.Dimension {
	dims := make([]*cloudwatch.Dimension, 0, len(b.additionalDimensions))
	for k, v := range b.additionalDimensions {
		dims = append(dims, new(cloudwatch.Dimension).SetName(k).SetValue(v))
	}
	return dims
}

// Returns 1 if the metric contains a __cw_high_res label, otherwise returns 60
func getResolution(m model.Metric) int64 {
	if _, ok := m[cwHighResLabel]; ok {
		return 1
	}
	return 60
}

func getUnit(m model.Metric) string {
	if u, ok := m[cwUnitLabel]; ok {
		return string(u)
	}
	return "None"
}

// fetchMetricFamilies retrieves metrics from the provided URL, decodes them into MetricFamily proto messages, and sends them to the provided channel.
// It returns after all MetricFamilies have been sent
func (b *Bridge) fetchMetricFamilies(ch chan<- *dto.MetricFamily) {
	url := b.prometheusScrapeURL
	defer close(ch)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		logger.KV(xlog.ERROR, "reason", "NewRequest", "err", err.Error())
		return
	}
	req.Header.Add("Accept", acceptHeader)

	var resp *http.Response

	if url != "" {
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		// TODO: create one client in Bridge
		client := &http.Client{Transport: transport}
		resp, err = client.Do(req)
		if err != nil {
			logger.KV(xlog.ERROR, "url", url, "err", err.Error())
			return
		}
	} else {
		w := httptest.NewRecorder()
		handler := promhttp.Handler()
		handler.ServeHTTP(w, req)

		resp = w.Result()
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.KV(xlog.ERROR, "http_status", resp.StatusCode, "url", url)
	}
	parseResponse(resp, ch)
}

// parseResponse consumes an http.Response and pushes it to the channel.
// It returns when all all MetricFamilies are parsed and put on the channel.
func parseResponse(resp *http.Response, ch chan<- *dto.MetricFamily) {
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		logger.KV(xlog.ERROR, "reason", "ParseMediaType", "err", err.Error())
	}

	if err == nil && mediaType == "application/vnd.google.protobuf" && params["encoding"] == "delimited" && params["proto"] == "io.prometheus.client.MetricFamily" {
		for {
			mf := new(dto.MetricFamily)
			if _, err = pbutil.ReadDelimited(resp.Body, mf); err != nil {
				if err == io.EOF {
					break
				}
				logger.KV(xlog.ERROR, "reason", "ReadDelimited", "err", err.Error())
				return
			}
			ch <- mf
		}
	} else {
		var parser expfmt.TextParser
		metricFamilies, err := parser.TextToMetricFamilies(resp.Body)
		if err != nil {
			logger.KV(xlog.ERROR, "reason", "TextToMetricFamilies", "err", err.Error())
			return
		}
		for _, mf := range metricFamilies {
			ch <- mf
		}
	}
}
