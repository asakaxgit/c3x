package aws_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/c3xdev/c3x/internal/usagesync"
)

func TestNAT_SumsBytesInAndConvertsToGB(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"BytesInFromSource":      {{at: day(2026, 9, 1), v: 2 * float64(gb)}},
		"BytesInFromDestination": {{at: day(2026, 9, 2), v: float64(gb)}},
		"BytesOutToDestination":  {{at: day(2026, 9, 1), v: 9 * float64(gb)}},
		"BytesOutToSource":       {{at: day(2026, 9, 2), v: 9 * float64(gb)}},
	}}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), natGW, window30)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["monthly_data_processed_gb"] != 3 {
		t.Errorf("monthly_data_processed_gb = %v, want 3", res.Values["monthly_data_processed_gb"])
	}
	if _, ok := res.Values["monthly_inter_az_gb"]; ok {
		t.Error("monthly_inter_az_gb is a hypothetical for consolidation, not a measured quantity")
	}
	if len(cw.listCalls) != 0 {
		t.Errorf("ListMetrics calls = %d; a zonal gateway with datapoints must not also sum zones", len(cw.listCalls))
	}
	if len(cw.calls) != 1 {
		t.Fatalf("GetMetricData calls = %d, want 1", len(cw.calls))
	}
	names := map[string]bool{}
	for _, q := range cw.calls[0].MetricDataQueries {
		names[metricName(q)] = true
		ms := q.MetricStat
		if *ms.Metric.Namespace != "AWS/NATGateway" || *ms.Stat != "Sum" || *ms.Period != 86400 {
			t.Errorf("query %s: %s stat %s period %d", metricName(q), *ms.Metric.Namespace, *ms.Stat, *ms.Period)
		}
		if got := dimMap(ms); len(got) != 1 || got["NatGatewayId"] != "nat-0abc" {
			t.Errorf("dimensions = %v, want only NatGatewayId", got)
		}
	}
	if len(names) != 2 || !names["BytesInFromSource"] || !names["BytesInFromDestination"] {
		t.Errorf("metrics = %v, want the two byte-in metrics and not BytesOut*", names)
	}
}

func TestNAT_SmallVolumeIsNotRoundedToZero(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"BytesInFromSource": {{at: day(2026, 9, 1), v: float64(5 << 20)}},
	}}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), natGW, window30)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Values["monthly_data_processed_gb"]; got != 0.0049 {
		t.Errorf("value = %v, want 0.0049", got)
	}
}

func TestNAT_SeriesScalesEachMonth(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"BytesInFromSource": {
			{at: day(2026, 8, 17), v: float64(gb)},
			{at: day(2026, 9, 1), v: float64(gb)},
		},
	}}
	cross := usagesync.Window{Start: day(2026, 8, 17), End: day(2026, 9, 16)}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), natGW, cross)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["monthly_data_processed_gb"] != 2 {
		t.Errorf("value = %v, want 2 GB over the 30-day window", res.Values["monthly_data_processed_gb"])
	}
	got := res.Series["monthly_data_processed_gb"]
	if got["2026-08"] != 2 || got["2026-09"] != 2 || len(got) != 2 {
		t.Errorf("series = %v, want 2 GB in each month (1 GiB over 15 days)", got)
	}
}

func TestNAT_RegionalGatewaySumsZonesOnlyWhenTheGatewayMetricIsEmpty(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{
		listPages: []*cloudwatch.ListMetricsOutput{{Metrics: []types.Metric{
			metricDims("NatGatewayId", "nat-0abc", "AvailabilityZone", "eu-west-1b"),
			metricDims("NatGatewayId", "nat-0abc", "AvailabilityZone", "eu-west-1a"),
			// The same zone published again under another metric name is one zone.
			metricDims("NatGatewayId", "nat-0abc", "AvailabilityZone", "eu-west-1a"),
			// A dimension set this code does not understand is not summed.
			metricDims("NatGatewayId", "nat-0abc", "AvailabilityZone", "eu-west-1c", "Extra", "x"),
			metricDims("NatGatewayId", "nat-0abc"),
		}}},
		byName: map[string][]*cloudwatch.GetMetricDataOutput{
			"BytesInFromSource": {
				emptyMetricPage(), // gateway-level query is empty, so this is a regional gateway
				natPage(float64(gb), float64(gb)),
				natPage(float64(gb), 0),
			},
		},
	}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), natGW, window30)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["monthly_data_processed_gb"] != 3 {
		t.Errorf("value = %v, want 3 GB summed across zones", res.Values["monthly_data_processed_gb"])
	}
	if len(cw.listCalls) != 1 {
		t.Fatalf("ListMetrics calls = %d, want 1", len(cw.listCalls))
	}
	in := cw.listCalls[0]
	if in.Namespace == nil || *in.Namespace != "AWS/NATGateway" || in.MetricName != nil {
		t.Errorf("ListMetrics namespace=%v metric=%v, want AWS/NATGateway and no metric name", in.Namespace, in.MetricName)
	}
	if in.RecentlyActive != "" {
		t.Errorf("RecentlyActive = %q, want unset", in.RecentlyActive)
	}
	filters := map[string]string{}
	for _, d := range in.Dimensions {
		filters[*d.Name] = *d.Value
	}
	if len(filters) != 1 || filters["NatGatewayId"] != "nat-0abc" {
		t.Errorf("ListMetrics dimensions = %v", filters)
	}
	if len(cw.calls) != 3 {
		t.Fatalf("GetMetricData calls = %d, want the gateway query plus two zones", len(cw.calls))
	}
	if dims := dimMap(cw.calls[0].MetricDataQueries[0].MetricStat); len(dims) != 1 || dims["NatGatewayId"] != "nat-0abc" {
		t.Errorf("first query dimensions = %v, want the gateway-level metric", dims)
	}
	for i, az := range []string{"eu-west-1a", "eu-west-1b"} {
		dims := dimMap(cw.calls[i+1].MetricDataQueries[0].MetricStat)
		if dims["NatGatewayId"] != "nat-0abc" || dims["AvailabilityZone"] != az || len(dims) != 2 {
			t.Errorf("query %d dimensions = %v, want zone %s", i+1, dims, az)
		}
		for _, q := range cw.calls[i+1].MetricDataQueries {
			if name := metricName(q); name != "BytesInFromSource" && name != "BytesInFromDestination" {
				t.Errorf("zone query metric %s", name)
			}
		}
	}
}

func TestNAT_NoBytesIsAnExplainedError(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{byName: map[string][]*cloudwatch.GetMetricDataOutput{
		"BytesInFromSource": {emptyMetricPage()},
	}}
	var regions []string
	_, err := source(cw, &regions).Collect(context.Background(), natGW, window30)
	if err == nil || !strings.Contains(err.Error(), "no NAT gateway byte datapoints in the window") {
		t.Errorf("err = %v", err)
	}
}

func TestNAT_ListErrorNamesTheNamespace(t *testing.T) {
	t.Parallel()
	denied := errors.New("AccessDenied")
	cw := &fakeCW{
		byName:  map[string][]*cloudwatch.GetMetricDataOutput{"BytesInFromSource": {emptyMetricPage()}},
		listErr: denied,
	}
	var regions []string
	_, err := source(cw, &regions).Collect(context.Background(), natGW, window30)
	if !errors.Is(err, denied) || !strings.Contains(err.Error(), "CloudWatch ListMetrics AWS/NATGateway") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), "AWS/NATGateway/") {
		t.Errorf("err = %v, an empty metric name must not be printed as a slash", err)
	}
}

func TestNAT_MetricErrorNamesTheCall(t *testing.T) {
	t.Parallel()
	denied := errors.New("AccessDenied")
	var regions []string
	_, err := source(&fakeCW{failMetric: map[string]error{"BytesInFromSource": denied}}, &regions).Collect(context.Background(), natGW, window30)
	if !errors.Is(err, denied) || !strings.Contains(err.Error(), "CloudWatch GetMetricData AWS/NATGateway/BytesInFromSource+BytesInFromDestination") {
		t.Errorf("err = %v", err)
	}
}

func metricDims(pairs ...string) types.Metric {
	var dims []types.Dimension
	for i := 0; i < len(pairs); i += 2 {
		dims = append(dims, types.Dimension{Name: ptr(pairs[i]), Value: ptr(pairs[i+1])})
	}
	return types.Metric{Namespace: ptr("AWS/NATGateway"), Dimensions: dims}
}

func emptyMetricPage() *cloudwatch.GetMetricDataOutput {
	return &cloudwatch.GetMetricDataOutput{}
}

func natPage(fromSource, fromDestination float64) *cloudwatch.GetMetricDataOutput {
	return &cloudwatch.GetMetricDataOutput{MetricDataResults: []types.MetricDataResult{
		{Id: ptr("in_src"), Timestamps: []time.Time{day(2026, 9, 1)}, Values: []float64{fromSource}},
		{Id: ptr("in_dst"), Timestamps: []time.Time{day(2026, 9, 1)}, Values: []float64{fromDestination}},
	}}
}

var natGW = usagesync.Target{
	Address: "aws_nat_gateway.main",
	Kind:    "aws_nat_gateway",
	ID:      "nat-0abc",
	Region:  "eu-west-1",
}
