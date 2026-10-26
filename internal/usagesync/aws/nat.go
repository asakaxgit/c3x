package aws

import (
	"context"
	"errors"
	"sort"

	"github.com/c3xdev/c3x/internal/usagesync"
)

const (
	kindNATGateway            = "aws_nat_gateway"
	keyMonthlyDataProcessedGB = "monthly_data_processed_gb"
)

// natGateway reads monthly_data_processed_gb.
//
// AWS bills each gigabyte processed once, in either direction. Bytes in
// from the VPC (an upload) plus bytes in from the internet (a download)
// are that volume. Bytes out the other side are the same flows leaving,
// so they are not added.
//
// A zonal gateway publishes those metrics with only NatGatewayId. A
// regional gateway publishes them per availability zone and the
// gateway-level query is empty. Zones are summed only in that case:
// adding them on top of a zonal total would count the same bytes twice.
// monthly_inter_az_gb is not emitted; the catalog uses it for a
// hypothetical consolidation, and its default is zero.
func (s *Source) natGateway(ctx context.Context, t usagesync.Target, w usagesync.Window) (usagesync.Result, error) {
	points, err := s.natBytes(ctx, t, w, "")
	if err != nil {
		return usagesync.Result{}, err
	}
	if len(points) == 0 {
		zones, err := natZones(ctx, s.client(t.Region), t.ID)
		if err != nil {
			return usagesync.Result{}, err
		}
		for _, az := range zones {
			part, err := s.natBytes(ctx, t, w, az)
			if err != nil {
				return usagesync.Result{}, err
			}
			points = append(points, part...)
		}
	}
	if len(points) == 0 {
		return usagesync.Result{}, errors.New("no NAT gateway byte datapoints in the window")
	}

	total, byMonth := monthlyCount(points, w)
	series := make(map[string]float64, len(byMonth))
	for month, bytes := range byMonth {
		series[month] = toGB(bytes)
	}
	res := usagesync.Result{Values: map[string]float64{keyMonthlyDataProcessedGB: toGB(total)}}
	if len(series) > 0 {
		res.Series = map[string]map[string]float64{keyMonthlyDataProcessedGB: series}
	}
	return res, nil
}

// natBytes reads one dimension set. az is empty for the gateway-level
// metric and set for one availability zone of a regional gateway.
func (s *Source) natBytes(ctx context.Context, t usagesync.Target, w usagesync.Window, az string) ([]datapoint, error) {
	dims := map[string]string{"NatGatewayId": t.ID}
	if az != "" {
		dims["AvailabilityZone"] = az
	}
	got, err := metricSeries(ctx, s.client(t.Region), seriesRequest{
		namespace:  "AWS/NATGateway",
		stat:       "Sum",
		dimensions: dims,
		periodSecs: secondsPerDay,
		start:      w.Start,
		end:        w.End,
		metrics: []namedMetric{
			{id: "in_src", name: "BytesInFromSource"},
			{id: "in_dst", name: "BytesInFromDestination"},
		},
	})
	if err != nil {
		return nil, err
	}
	points := append([]datapoint{}, got["BytesInFromSource"]...)
	return append(points, got["BytesInFromDestination"]...), nil
}

// natZones is the availability zones this gateway publishes metrics for.
// Only the NatGatewayId + AvailabilityZone shape is used; anything else
// is a dimension set this code does not understand and is not summed.
func natZones(ctx context.Context, cw CloudWatch, id string) ([]string, error) {
	listed, err := listMetrics(ctx, cw, "AWS/NATGateway", "", map[string]string{
		"NatGatewayId": id,
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, dims := range listed {
		if dims["NatGatewayId"] != id || dims["AvailabilityZone"] == "" || len(dims) != 2 {
			continue
		}
		seen[dims["AvailabilityZone"]] = struct{}{}
	}
	zones := make([]string, 0, len(seen))
	for az := range seen {
		zones = append(zones, az)
	}
	sort.Strings(zones)
	return zones, nil
}
