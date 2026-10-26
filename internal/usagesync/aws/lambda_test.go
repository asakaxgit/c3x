package aws_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/c3xdev/c3x/internal/usagesync"
)

func TestLambda_RequestsAndWeightedDuration(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"Invocations": {{at: day(2026, 9, 1), v: 100}, {at: day(2026, 9, 2), v: 300}},
		"Duration":    {{at: day(2026, 9, 1), v: 20000}, {at: day(2026, 9, 2), v: 30000}},
	}}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), lambdaFn, window30)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["monthly_requests"] != 400 {
		t.Errorf("monthly_requests = %v, want 400", res.Values["monthly_requests"])
	}
	if res.Values["request_duration_ms"] != 125 {
		t.Errorf("request_duration_ms = %v, want 50000/400 = 125", res.Values["request_duration_ms"])
	}
	if res.Series["monthly_requests"]["2026-09"] != 400 {
		t.Errorf("request series = %v", res.Series["monthly_requests"])
	}
	if res.Series["request_duration_ms"]["2026-09"] != 125 {
		t.Errorf("duration series = %v", res.Series["request_duration_ms"])
	}
	if _, ok := res.Values["monthly_provisioned_gb_seconds"]; ok {
		t.Error("provisioned concurrency is not a CloudWatch metric this stage reads")
	}
}

func TestLambda_RequestShapeIsFunctionNameOnly(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"Invocations": {{at: day(2026, 9, 1), v: 1}},
		"Duration":    {{at: day(2026, 9, 1), v: 10}},
	}}
	var regions []string
	if _, err := source(cw, &regions).Collect(context.Background(), lambdaFn, window30); err != nil {
		t.Fatal(err)
	}
	if len(regions) != 1 || regions[0] != "eu-west-1" {
		t.Errorf("regions = %v", regions)
	}
	if len(cw.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(cw.calls))
	}
	in := cw.calls[0]
	if !in.StartTime.Equal(window30.Start) || !in.EndTime.Equal(window30.End) {
		t.Errorf("window = %v..%v", in.StartTime, in.EndTime)
	}
	names := map[string]bool{}
	for _, q := range in.MetricDataQueries {
		ms := q.MetricStat
		if *ms.Metric.Namespace != "AWS/Lambda" || *ms.Stat != "Sum" || *ms.Period != 86400 {
			t.Errorf("query %s: namespace %s stat %s period %d", metricName(q), *ms.Metric.Namespace, *ms.Stat, *ms.Period)
		}
		names[metricName(q)] = true
		if got := dimMap(ms); len(got) != 1 || got["FunctionName"] != "api-prod" {
			t.Errorf("dimensions = %v, want only FunctionName=api-prod (Resource=$LATEST would drop aliases)", got)
		}
	}
	if len(names) != 2 || !names["Invocations"] || !names["Duration"] {
		t.Errorf("metrics = %v, want Invocations and Duration and nothing else (InitDuration is not billed here)", names)
	}
}

func TestLambda_ShortWindowScalesRequestsButNotDuration(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"Invocations": {{at: day(2026, 9, 1), v: 100}},
		"Duration":    {{at: day(2026, 9, 1), v: 10000}},
	}}
	short := usagesync.Window{Start: day(2026, 9, 1), End: day(2026, 9, 16)}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), lambdaFn, short)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["monthly_requests"] != 200 {
		t.Errorf("monthly_requests = %v, want 100 scaled over 15 days to 200", res.Values["monthly_requests"])
	}
	if res.Values["request_duration_ms"] != 100 {
		t.Errorf("request_duration_ms = %v, want 100, not scaled", res.Values["request_duration_ms"])
	}
}

func TestLambda_SeriesPerMonthAndDurationStaysWeighted(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"Invocations": {{at: day(2026, 8, 17), v: 10}, {at: day(2026, 9, 1), v: 30}},
		"Duration":    {{at: day(2026, 8, 17), v: 1000}, {at: day(2026, 9, 1), v: 6000}},
	}}
	cross := usagesync.Window{Start: day(2026, 8, 17), End: day(2026, 9, 16)}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), lambdaFn, cross)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["monthly_requests"] != 40 {
		t.Errorf("monthly_requests = %v, want 40", res.Values["monthly_requests"])
	}
	if res.Values["request_duration_ms"] != 175 {
		t.Errorf("request_duration_ms = %v, want 7000/40 = 175", res.Values["request_duration_ms"])
	}
	if res.Series["monthly_requests"]["2026-08"] != 20 || res.Series["monthly_requests"]["2026-09"] != 60 {
		t.Errorf("request series = %v, want August 20 and September 60", res.Series["monthly_requests"])
	}
	if res.Series["request_duration_ms"]["2026-08"] != 100 || res.Series["request_duration_ms"]["2026-09"] != 200 {
		t.Errorf("duration series = %v, want 100 and 200", res.Series["request_duration_ms"])
	}
}

func TestLambda_MonthWithoutDurationIsOmittedFromTheSeries(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"Invocations": {{at: day(2026, 8, 17), v: 10}, {at: day(2026, 9, 1), v: 30}},
		"Duration":    {{at: day(2026, 9, 1), v: 6000}},
	}}
	cross := usagesync.Window{Start: day(2026, 8, 17), End: day(2026, 9, 16)}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), lambdaFn, cross)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["monthly_requests"] != 40 || res.Values["request_duration_ms"] != 150 {
		t.Errorf("values = %v, want requests 40 and duration 150", res.Values)
	}
	if _, ok := res.Series["request_duration_ms"]["2026-08"]; ok {
		t.Errorf("August has invocations but no Duration; series = %v", res.Series["request_duration_ms"])
	}
	if res.Series["request_duration_ms"]["2026-09"] != 200 {
		t.Errorf("September duration = %v, want 200", res.Series["request_duration_ms"]["2026-09"])
	}
	if res.Series["monthly_requests"]["2026-08"] != 20 || res.Series["monthly_requests"]["2026-09"] != 60 {
		t.Errorf("request series = %v", res.Series["monthly_requests"])
	}
}

func TestLambda_NoInvocationsWritesNothing(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"Duration": {{at: day(2026, 9, 1), v: 1000}},
	}}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), lambdaFn, window30)
	if err == nil || !strings.Contains(err.Error(), "no Lambda Invocations datapoints in the window") {
		t.Fatalf("err = %v", err)
	}
	if len(res.Values) != 0 {
		t.Errorf("wrote %v; a missing invocation count must not leave a duration behind", res.Values)
	}
}

func TestLambda_InvocationsWithoutDurationWritesNothing(t *testing.T) {
	t.Parallel()
	cw := &fakeCW{valuesByMetric: map[string][]cwPoint{
		"Invocations": {{at: day(2026, 9, 1), v: 100}},
	}}
	var regions []string
	res, err := source(cw, &regions).Collect(context.Background(), lambdaFn, window30)
	if err == nil || !strings.Contains(err.Error(), "no Lambda Duration datapoints in the window") {
		t.Fatalf("err = %v", err)
	}
	if len(res.Values) != 0 || len(res.Series) != 0 {
		t.Errorf("wrote values %v series %v; the catalog would otherwise assume 100 ms", res.Values, res.Series)
	}
}

func TestLambda_APIErrorNamesTheCall(t *testing.T) {
	t.Parallel()
	denied := errors.New("AccessDenied")
	var regions []string
	_, err := source(&fakeCW{failMetric: map[string]error{"Invocations": denied}}, &regions).Collect(context.Background(), lambdaFn, window30)
	if !errors.Is(err, denied) || !strings.Contains(err.Error(), "CloudWatch GetMetricData AWS/Lambda/Invocations+Duration") {
		t.Errorf("err = %v", err)
	}
}

func dimMap(ms *types.MetricStat) map[string]string {
	out := map[string]string{}
	if ms == nil || ms.Metric == nil {
		return out
	}
	for _, d := range ms.Metric.Dimensions {
		if d.Name != nil && d.Value != nil {
			out[*d.Name] = *d.Value
		}
	}
	return out
}

var lambdaFn = usagesync.Target{
	Address: "aws_lambda_function.api",
	Kind:    "aws_lambda_function",
	ID:      "api-prod",
	Region:  "eu-west-1",
}
