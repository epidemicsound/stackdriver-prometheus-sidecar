/*
Copyright 2018 Google Inc.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package retrieval

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	timestamp_pb "github.com/golang/protobuf/ptypes/timestamp"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/pkg/textparse"
	"github.com/prometheus/tsdb"
	tsdbLabels "github.com/prometheus/tsdb/labels"
	distribution_pb "google.golang.org/genproto/googleapis/api/distribution"
	metric_pb "google.golang.org/genproto/googleapis/api/metric"
	monitoring_pb "google.golang.org/genproto/googleapis/monitoring/v3"
)

type sampleBuilder struct {
	series seriesGetter
}

// next extracts the next sample from the TSDB input sample list and returns
// the remainder of the input.
func (b *sampleBuilder) next(ctx context.Context, samples []tsdb.RefSample) (*monitoring_pb.TimeSeries, uint64, []tsdb.RefSample, error) {
	sample := samples[0]
	tailSamples := samples[1:]

	if math.IsNaN(sample.V) {
		return nil, 0, tailSamples, nil
	}

	entry, ok, err := b.series.get(ctx, sample.Ref)
	if err != nil {
		return nil, 0, samples, errors.Wrap(err, "get series information")
	}
	if !ok {
		return nil, 0, tailSamples, nil
	}

	if entry.tracker != nil {
		entry.tracker.newPoint(ctx, entry.lset, sample.T, sample.V)
	}

	if !entry.exported {
		return nil, 0, tailSamples, nil
	}
	// Get a shallow copy of the proto so we can overwrite the point field
	// and safely send it into the remote queues.
	ts := *entry.proto

	point := &monitoring_pb.Point{
		Interval: &monitoring_pb.TimeInterval{
			EndTime: getTimestamp(sample.T),
		},
	}
	ts.Points = append(ts.Points, point)

	var resetTimestamp int64

	switch entry.metadata.MetricType {
	case textparse.MetricTypeCounter:
		var v float64
		resetTimestamp, v, ok = b.series.getResetAdjusted(sample.Ref, sample.T, sample.V)
		if !ok {
			return nil, 0, tailSamples, nil
		}
		point.Interval.StartTime = getTimestamp(resetTimestamp)
		point.Value = buildTypedValue(entry.metadata.ValueType, v)

	case textparse.MetricTypeGauge, textparse.MetricTypeUnknown:
		point.Value = buildTypedValue(entry.metadata.ValueType, sample.V)

	case textparse.MetricTypeSummary:
		switch entry.suffix {
		case metricSuffixSum:
			var v float64
			resetTimestamp, v, ok = b.series.getResetAdjusted(sample.Ref, sample.T, sample.V)
			if !ok {
				return nil, 0, tailSamples, nil
			}
			point.Interval.StartTime = getTimestamp(resetTimestamp)
			point.Value = &monitoring_pb.TypedValue{Value: &monitoring_pb.TypedValue_DoubleValue{v}}
		case metricSuffixCount:
			var v float64
			resetTimestamp, v, ok = b.series.getResetAdjusted(sample.Ref, sample.T, sample.V)
			if !ok {
				return nil, 0, tailSamples, nil
			}
			point.Interval.StartTime = getTimestamp(resetTimestamp)
			point.Value = &monitoring_pb.TypedValue{Value: &monitoring_pb.TypedValue_Int64Value{int64(v)}}
		case "": // Actual quantiles.
			point.Value = &monitoring_pb.TypedValue{Value: &monitoring_pb.TypedValue_DoubleValue{sample.V}}
		default:
			return nil, 0, tailSamples, errors.Errorf("unexpected metric name suffix %q", entry.suffix)
		}

	case textparse.MetricTypeHistogram:
		// We pass in the original lset for matching since Prometheus's target label must
		// be the same as well.
		var v *distribution_pb.Distribution
		v, resetTimestamp, tailSamples, err = b.buildDistribution(ctx, entry.metadata.Metric, entry.lset, samples)
		if v == nil || err != nil {
			return nil, 0, tailSamples, err
		}
		point.Interval.StartTime = getTimestamp(resetTimestamp)
		point.Value = &monitoring_pb.TypedValue{
			Value: &monitoring_pb.TypedValue_DistributionValue{v},
		}

	default:
		return nil, 0, samples[1:], errors.Errorf("unexpected metric type %s", entry.metadata.MetricType)
	}

	if !b.series.updateSampleInterval(entry.hash, resetTimestamp, sample.T) {
		return nil, 0, tailSamples, nil
	}
	return &ts, entry.hash, tailSamples, nil
}

const (
	metricSuffixBucket = "_bucket"
	metricSuffixSum    = "_sum"
	metricSuffixCount  = "_count"
	metricSuffixTotal  = "_total"
)

func stripComplexMetricSuffix(name string) (prefix string, suffix string, ok bool) {
	if strings.HasSuffix(name, metricSuffixBucket) {
		return name[:len(name)-len(metricSuffixBucket)], metricSuffixBucket, true
	}
	if strings.HasSuffix(name, metricSuffixCount) {
		return name[:len(name)-len(metricSuffixCount)], metricSuffixCount, true
	}
	if strings.HasSuffix(name, metricSuffixSum) {
		return name[:len(name)-len(metricSuffixSum)], metricSuffixSum, true
	}
	if strings.HasSuffix(name, metricSuffixTotal) {
		return name[:len(name)-len(metricSuffixTotal)], metricSuffixTotal, true
	}
	return name, "", false
}

const (
	maxLabelCount = 10
	metricsPrefix = "external.googleapis.com/prometheus"
)

func getMetricType(prefix string, promName string) string {
	if prefix == "" {
		return metricsPrefix + "/" + promName
	}
	return prefix + "/" + promName
}

// getTimestamp converts a millisecond timestamp into a protobuf timestamp.
func getTimestamp(t int64) *timestamp_pb.Timestamp {
	return &timestamp_pb.Timestamp{
		Seconds: t / 1000,
		Nanos:   int32((t % 1000) * int64(time.Millisecond)),
	}
}

type distribution struct {
	bounds []float64
	values []int64
}

func (d *distribution) Len() int {
	return len(d.bounds)
}

func (d *distribution) Less(i, j int) bool {
	return d.bounds[i] < d.bounds[j]
}

func (d *distribution) Swap(i, j int) {
	d.bounds[i], d.bounds[j] = d.bounds[j], d.bounds[i]
	d.values[i], d.values[j] = d.values[j], d.values[i]
}

// buildDistribution consumes series from the beginning of the input slice that belong to a histogram
// with the given metric name and label set.
// It returns the reset timestamp along with the distrubution.
func (b *sampleBuilder) buildDistribution(
	ctx context.Context,
	baseName string,
	matchLset tsdbLabels.Labels,
	samples []tsdb.RefSample,
) (*distribution_pb.Distribution, int64, []tsdb.RefSample, error) {
	var (
		consumed       int
		count, sum     float64
		resetTimestamp int64
		lastTimestamp  int64
		dist           = distribution{bounds: make([]float64, 0, 20), values: make([]int64, 0, 20)}
		skip           = false
	)
	// We assume that all series belonging to the histogram are sequential. Consume series
	// until we hit a new metric.
Loop:
	for i, s := range samples {
		e, ok, err := b.series.get(ctx, s.Ref)
		if err != nil {
			return nil, 0, samples, err
		}
		if !ok {
			consumed++
			// TODO(fabxc): increment metric.
			continue
		}
		name := e.lset.Get("__name__")
		// The series matches if it has the same base name, the remainder is a valid histogram suffix,
		// and the labels aside from the le and __name__ label match up.
		if !strings.HasPrefix(name, baseName) || !histogramLabelsEqual(e.lset, matchLset) {
			break
		}
		// In general, a scrape cannot contain the same (set of) series repeatedlty but for different timestamps.
		// It could still happen with bad clients though and we are doing it in tests for simplicity.
		// If we detect the same series as before but for a different timestamp, return the histogram up to this
		// series and leave the duplicate time series untouched on the input.
		if i > 0 && s.T != lastTimestamp {
			break
		}
		lastTimestamp = s.T

		rt, v, ok := b.series.getResetAdjusted(s.Ref, s.T, s.V)

		switch name[len(baseName):] {
		case metricSuffixSum:
			sum = v
		case metricSuffixCount:
			count = v
			// We take the count series as the authoritative source for the overall reset timestamp.
			resetTimestamp = rt
		case metricSuffixBucket:
			upper, err := strconv.ParseFloat(e.lset.Get("le"), 64)
			if err != nil {
				consumed++
				// TODO(fabxc): increment metric.
				continue
			}
			dist.bounds = append(dist.bounds, upper)
			dist.values = append(dist.values, int64(v))
		default:
			break Loop
		}
		// If a series appeared for the first time, we won't get a valid reset timestamp yet.
		// This may happen if the histogram is entirely new or if new series appeared through bucket changes.
		// We skip the entire histogram sample in this case.
		if !ok {
			skip = true
		}
		consumed++
	}
	// Don't emit a sample if we explicitly skip it or no reset timestamp was set because the
	// count series was missing.
	if skip || resetTimestamp == 0 {
		return nil, 0, samples[consumed:], nil
	}
	// We do not assume that the buckets in the sample batch are in order, so we sort them again here.
	// The code below relies on this to convert between Prometheus's and Stackdriver's bucketing approaches.
	sort.Sort(&dist)
	// Reuse slices we already populated to build final bounds and values.
	var (
		bounds           = dist.bounds[:0]
		values           = dist.values[:0]
		mean, dev, lower float64
		prevVal          int64
	)
	if count > 0 {
		mean = sum / count
	}
	for i, upper := range dist.bounds {
		if math.IsInf(upper, 1) {
			upper = lower
		} else {
			bounds = append(bounds, upper)
		}

		val := dist.values[i] - prevVal
		x := (lower + upper) / 2
		dev += float64(val) * (x - mean) * (x - mean)

		lower = upper
		prevVal = dist.values[i]
		values = append(values, val)
	}
	d := &distribution_pb.Distribution{
		Count:                 int64(count),
		Mean:                  mean,
		SumOfSquaredDeviation: dev,
		BucketOptions: &distribution_pb.Distribution_BucketOptions{
			Options: &distribution_pb.Distribution_BucketOptions_ExplicitBuckets{
				ExplicitBuckets: &distribution_pb.Distribution_BucketOptions_Explicit{
					Bounds: bounds,
				},
			},
		},
		BucketCounts: values,
	}
	return d, resetTimestamp, samples[consumed:], nil
}

// histogramLabelsEqual checks whether two label sets for a histogram series are equal aside from their
// le and __name__ labels.
func histogramLabelsEqual(a, b tsdbLabels.Labels) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Name == "le" || a[i].Name == "__name__" {
			i++
			continue
		}
		if b[j].Name == "le" || b[j].Name == "__name__" {
			j++
			continue
		}
		if a[i] != b[j] {
			return false
		}
		i++
		j++
	}
	// Consume trailing le and __name__ labels so the check below passes correctly.
	for i < len(a) {
		if a[i].Name == "le" || a[i].Name == "__name__" {
			i++
			continue
		}
		break
	}
	for j < len(b) {
		if b[j].Name == "le" || b[j].Name == "__name__" {
			j++
			continue
		}
		break
	}
	// If one label set still has labels left, they are not equal.
	return i == len(a) && j == len(b)
}

func buildTypedValue(valueType metric_pb.MetricDescriptor_ValueType, v float64) *monitoring_pb.TypedValue {
	if valueType == metric_pb.MetricDescriptor_INT64 {
		return &monitoring_pb.TypedValue{Value: &monitoring_pb.TypedValue_Int64Value{int64(math.Round(v))}}
	}
	// Default to double, which is the only type supported by Prometheus.
	return &monitoring_pb.TypedValue{Value: &monitoring_pb.TypedValue_DoubleValue{v}}
}
