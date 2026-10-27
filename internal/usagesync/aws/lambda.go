package aws

import (
	"context"
	"errors"

	"github.com/c3xdev/c3x/internal/usagesync"
)

const (
	kindLambdaFunction   = "aws_lambda_function"
	keyMonthlyRequests   = "monthly_requests"
	keyRequestDurationMS = "request_duration_ms"
)

// lambdaFunction reads monthly_requests and request_duration_ms.
//
// Both come from the function-wide CloudWatch metrics (dimension
// FunctionName only). Adding Resource=$LATEST would count only the
// unpublished version and drop aliases. Duration is the processing time
// in milliseconds; it does not include the cold-start InitDuration, and
// the catalog has no key for that, so it is not added.
//
// The two keys are written together or not at all. The catalog's
// duration line defaults a missing request_duration_ms to 100 ms, and
// once monthly_requests is non-zero that default becomes a real cost.
// A function with invocations but no Duration is therefore an error,
// not a guess of 100 ms.
func (s *Source) lambdaFunction(ctx context.Context, t usagesync.Target, w usagesync.Window) (usagesync.Result, error) {
	got, err := metricSeries(ctx, s.client(t.Region), seriesRequest{
		namespace:  "AWS/Lambda",
		stat:       "Sum",
		dimensions: map[string]string{"FunctionName": t.ID},
		periodSecs: secondsPerDay,
		start:      w.Start,
		end:        w.End,
		metrics: []namedMetric{
			{id: "invocations", name: "Invocations"},
			{id: "duration", name: "Duration"},
		},
	})
	if err != nil {
		return usagesync.Result{}, err
	}
	inv, dur := got["Invocations"], got["Duration"]
	if len(inv) == 0 {
		return usagesync.Result{}, errors.New("no Lambda Invocations datapoints in the window")
	}
	if len(dur) == 0 {
		return usagesync.Result{}, errors.New("no Lambda Duration datapoints in the window")
	}

	requests, reqSeries := monthlyCount(inv, w)
	millis, durSeries := invocationWeightedMillis(inv, dur)
	res := usagesync.Result{Values: map[string]float64{
		keyMonthlyRequests:   requests,
		keyRequestDurationMS: millis,
	}}
	series := map[string]map[string]float64{}
	if len(reqSeries) > 0 {
		series[keyMonthlyRequests] = reqSeries
	}
	if len(durSeries) > 0 {
		series[keyRequestDurationMS] = durSeries
	}
	if len(series) > 0 {
		res.Series = series
	}
	return res, nil
}

// invocationWeightedMillis is total Duration divided by total Invocations.
// Duration is already a per-invocation quantity, so it is not scaled to
// 30 days the way a count is. A month that has invocations but no
// Duration points is left out of the series: there is no average to
// report for it.
func invocationWeightedMillis(inv, dur []datapoint) (float64, map[string]float64) {
	var invTotal float64
	invMonth := map[string]float64{}
	for _, p := range inv {
		invTotal += p.value
		invMonth[p.at.UTC().Format("2006-01")] += p.value
	}
	var durTotal float64
	durMonth := map[string]float64{}
	durSeen := map[string]bool{}
	for _, p := range dur {
		durTotal += p.value
		m := p.at.UTC().Format("2006-01")
		durMonth[m] += p.value
		durSeen[m] = true
	}
	value := 0.0
	if invTotal > 0 {
		value = round4(durTotal / invTotal)
	}
	var series map[string]float64
	for month, n := range invMonth {
		if n <= 0 || !durSeen[month] {
			continue
		}
		if series == nil {
			series = map[string]float64{}
		}
		series[month] = round4(durMonth[month] / n)
	}
	return value, series
}
