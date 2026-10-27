//go:build floci

package aws_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/c3xdev/c3x/internal/usagesync"
	awssrc "github.com/c3xdev/c3x/internal/usagesync/aws"
)

const flociGB = 1 << 30

// flociWindow is 30 days, so a monthly quantity is the measured total.
// Both instants are midnight UTC, which is the period boundary a
// 86400-second CloudWatch query uses.
var flociWindow = usagesync.Window{
	Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	End:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
}

// TestFloci_SyncsS3LambdaAndNAT is a fork-only smoke test. It is
// compiled only with -tags floci, so `go test ./...` and upstream CI
// never build it, and it must not be sent upstream. Floci does not
// publish AWS/S3, AWS/Lambda or AWS/NATGateway on its own; the test
// seeds those metrics and then runs the real usage-sync source.
func TestFloci_SyncsS3LambdaAndNAT(t *testing.T) {
	endpoint := flociEndpoint()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := awssrc.NewWithClients(func(region string) awssrc.CloudWatch {
		return flociCloudWatch(region, endpoint)
	})
	client := flociCloudWatch("eu-west-1", endpoint)
	waitFloci(ctx, t, client)

	// One id per run. Floci appends every PutMetricData sample, so a
	// fixed name would add this run's values to the previous run's.
	id := flociID(t)
	bucketName := "smoke-" + id
	filterID := "logs-prefix"
	functionName := "fn-" + id
	natID := "nat-" + id

	// September 10 and 11 sit inside the window. Request metrics share
	// one timestamp: the tier-2 remainder is AllRequests minus the named
	// metrics at that same instant, not across the month.
	day10 := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	day11 := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	s3Dims := []cwtypes.Dimension{
		{Name: aws.String("BucketName"), Value: aws.String(bucketName)},
		{Name: aws.String("FilterId"), Value: aws.String(filterID)},
	}

	// Eight request metrics go out as one GetMetricData call. Floci
	// 2.3.0 returns ListMetrics and GetMetricData in a single response
	// (it accepts NextToken and does not page), so this is the batched
	// query the collector's pagination loop actually issues.
	putMetrics(ctx, t, client, "AWS/S3", []cwtypes.MetricDatum{
		metricDatum("BucketSizeBytes", day10, 100*flociGB, cwtypes.StandardUnitBytes, []cwtypes.Dimension{
			{Name: aws.String("BucketName"), Value: aws.String(bucketName)},
			{Name: aws.String("StorageType"), Value: aws.String("StandardStorage")},
		}),
		metricDatum("PutRequests", day10, 1000, cwtypes.StandardUnitCount, s3Dims),
		metricDatum("PostRequests", day10, 200, cwtypes.StandardUnitCount, s3Dims),
		metricDatum("ListRequests", day10, 50, cwtypes.StandardUnitCount, s3Dims),
		metricDatum("GetRequests", day10, 800, cwtypes.StandardUnitCount, s3Dims),
		metricDatum("SelectRequests", day10, 10, cwtypes.StandardUnitCount, s3Dims),
		metricDatum("HeadRequests", day10, 20, cwtypes.StandardUnitCount, s3Dims),
		metricDatum("DeleteRequests", day10, 5, cwtypes.StandardUnitCount, s3Dims),
		// 15 requests match none of the named metrics and stay in tier 2.
		metricDatum("AllRequests", day10, 2100, cwtypes.StandardUnitCount, s3Dims),
	})
	putMetrics(ctx, t, client, "AWS/Lambda", []cwtypes.MetricDatum{
		metricDatum("Invocations", day10, 100, cwtypes.StandardUnitCount, []cwtypes.Dimension{
			{Name: aws.String("FunctionName"), Value: aws.String(functionName)},
		}),
		metricDatum("Invocations", day11, 300, cwtypes.StandardUnitCount, []cwtypes.Dimension{
			{Name: aws.String("FunctionName"), Value: aws.String(functionName)},
		}),
		metricDatum("Duration", day10, 20000, cwtypes.StandardUnitMilliseconds, []cwtypes.Dimension{
			{Name: aws.String("FunctionName"), Value: aws.String(functionName)},
		}),
		metricDatum("Duration", day11, 30000, cwtypes.StandardUnitMilliseconds, []cwtypes.Dimension{
			{Name: aws.String("FunctionName"), Value: aws.String(functionName)},
		}),
	})
	putMetrics(ctx, t, client, "AWS/NATGateway", []cwtypes.MetricDatum{
		metricDatum("BytesInFromSource", day10, 2*flociGB, cwtypes.StandardUnitBytes, []cwtypes.Dimension{
			{Name: aws.String("NatGatewayId"), Value: aws.String(natID)},
		}),
		metricDatum("BytesInFromDestination", day11, flociGB, cwtypes.StandardUnitBytes, []cwtypes.Dimension{
			{Name: aws.String("NatGatewayId"), Value: aws.String(natID)},
		}),
	})

	t.Run("S3", func(t *testing.T) {
		res := collectFloci(ctx, t, src, usagesync.Target{
			Address: "aws_s3_bucket.data",
			Kind:    "aws_s3_bucket",
			ID:      bucketName,
			Region:  "eu-west-1",
		})
		if res.Values["standard_storage_gb"] != 100 {
			t.Errorf("standard_storage_gb = %v, want 100", res.Values["standard_storage_gb"])
		}
		// tier 1 = 1000 + 200 + 50. tier 2 = 800 + 10 + 20 + 5 + 15 remainder.
		if res.Values["monthly_tier_1_requests"] != 1250 {
			t.Errorf("monthly_tier_1_requests = %v, want 1250", res.Values["monthly_tier_1_requests"])
		}
		if res.Values["monthly_tier_2_requests"] != 850 {
			t.Errorf("monthly_tier_2_requests = %v, want 850", res.Values["monthly_tier_2_requests"])
		}
		assertMonth(t, res, "standard_storage_gb", 100)
		assertMonth(t, res, "monthly_tier_1_requests", 1250)
		assertMonth(t, res, "monthly_tier_2_requests", 850)
	})

	t.Run("Lambda", func(t *testing.T) {
		res := collectFloci(ctx, t, src, usagesync.Target{
			Address: "aws_lambda_function.api",
			Kind:    "aws_lambda_function",
			ID:      functionName,
			Region:  "eu-west-1",
		})
		if res.Values["monthly_requests"] != 400 {
			t.Errorf("monthly_requests = %v, want 400", res.Values["monthly_requests"])
		}
		if res.Values["request_duration_ms"] != 125 {
			t.Errorf("request_duration_ms = %v, want 50000/400 = 125", res.Values["request_duration_ms"])
		}
		assertMonth(t, res, "monthly_requests", 400)
		assertMonth(t, res, "request_duration_ms", 125)
	})

	t.Run("NAT", func(t *testing.T) {
		res := collectFloci(ctx, t, src, usagesync.Target{
			Address: "aws_nat_gateway.egress",
			Kind:    "aws_nat_gateway",
			ID:      natID,
			Region:  "eu-west-1",
		})
		if res.Values["monthly_data_processed_gb"] != 3 {
			t.Errorf("monthly_data_processed_gb = %v, want 3", res.Values["monthly_data_processed_gb"])
		}
		assertMonth(t, res, "monthly_data_processed_gb", 3)
	})
}

func flociEndpoint() string {
	if v := os.Getenv("FLOCI_ENDPOINT"); v != "" {
		return v
	}
	return "http://localhost:4566"
}

func flociCloudWatch(region, endpoint string) *cloudwatch.Client {
	return cloudwatch.New(cloudwatch.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint: aws.String(endpoint),
	})
}

func waitFloci(ctx context.Context, t *testing.T, client *cloudwatch.Client) {
	t.Helper()
	var last error
	for i := 0; i < 40; i++ {
		_, err := client.ListMetrics(ctx, &cloudwatch.ListMetricsInput{
			Namespace: aws.String("AWS/S3"),
		})
		if err == nil {
			return
		}
		last = err
		select {
		case <-ctx.Done():
			t.Fatalf("Floci at %s: %v", flociEndpoint(), ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("Floci at %s is not accepting CloudWatch calls: %v", flociEndpoint(), last)
}

func flociID(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func metricDatum(name string, at time.Time, value float64, unit cwtypes.StandardUnit, dims []cwtypes.Dimension) cwtypes.MetricDatum {
	return cwtypes.MetricDatum{
		MetricName: aws.String(name),
		Timestamp:  aws.Time(at),
		Value:      aws.Float64(value),
		Unit:       unit,
		Dimensions: dims,
	}
}

func putMetrics(ctx context.Context, t *testing.T, client *cloudwatch.Client, namespace string, data []cwtypes.MetricDatum) {
	t.Helper()
	_, err := client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace:  aws.String(namespace),
		MetricData: data,
	})
	if err != nil {
		t.Fatalf("PutMetricData %s: %v", namespace, err)
	}
}

func collectFloci(ctx context.Context, t *testing.T, src *awssrc.Source, target usagesync.Target) usagesync.Result {
	t.Helper()
	res, err := src.Collect(ctx, target, flociWindow)
	if err != nil {
		t.Fatalf("Collect %s: %v", target.Kind, err)
	}
	if res.Problem != "" {
		t.Fatalf("Collect %s problem = %q", target.Kind, res.Problem)
	}
	return res
}

func assertMonth(t *testing.T, res usagesync.Result, key string, want float64) {
	t.Helper()
	got := res.Series[key]
	if len(got) != 1 || got["2026-09"] != want {
		t.Errorf("series %s = %v, want 2026-09 = %v", key, got, want)
	}
}
