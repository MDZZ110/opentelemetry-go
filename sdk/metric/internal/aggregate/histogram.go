// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package aggregate // import "go.opentelemetry.io/otel/sdk/metric/internal/aggregate"

import (
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/aggregation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type buckets[N int64 | float64] struct {
	counts   []uint64
	count    uint64
	sum      N
	min, max N
}

// newBuckets returns buckets with n bins.
func newBuckets[N int64 | float64](n int) *buckets[N] {
	return &buckets[N]{counts: make([]uint64, n)}
}

func (b *buckets[N]) bin(idx int, value N) {
	b.counts[idx]++
	b.count++
	b.sum += value
	if value < b.min {
		b.min = value
	} else if value > b.max {
		b.max = value
	}
}

// histValues summarizes a set of measurements as an histValues with
// explicitly defined buckets.
type histValues[N int64 | float64] struct {
	bounds []float64

	values   map[attribute.Set]*buckets[N]
	valuesMu sync.Mutex
}

func newHistValues[N int64 | float64](bounds []float64) *histValues[N] {
	// The responsibility of keeping all buckets correctly associated with the
	// passed boundaries is ultimately this type's responsibility. Make a copy
	// here so we can always guarantee this. Or, in the case of failure, have
	// complete control over the fix.
	b := make([]float64, len(bounds))
	copy(b, bounds)
	sort.Float64s(b)
	return &histValues[N]{
		bounds: b,
		values: make(map[attribute.Set]*buckets[N]),
	}
}

// Aggregate records the measurement value, scoped by attr, and aggregates it
// into a histogram.
func (s *histValues[N]) Aggregate(value N, attr attribute.Set) {
	// This search will return an index in the range [0, len(s.bounds)], where
	// it will return len(s.bounds) if value is greater than the last element
	// of s.bounds. This aligns with the buckets in that the length of buckets
	// is len(s.bounds)+1, with the last bucket representing:
	// (s.bounds[len(s.bounds)-1], +∞).
	idx := sort.SearchFloat64s(s.bounds, float64(value))

	s.valuesMu.Lock()
	defer s.valuesMu.Unlock()

	b, ok := s.values[attr]
	if !ok {
		// N+1 buckets. For example:
		//
		//   bounds = [0, 5, 10]
		//
		// Then,
		//
		//   buckets = (-∞, 0], (0, 5.0], (5.0, 10.0], (10.0, +∞)
		b = newBuckets[N](len(s.bounds) + 1)
		// Ensure min and max are recorded values (not zero), for new buckets.
		b.min, b.max = value, value
		s.values[attr] = b
	}
	b.bin(idx, value)
}

// newDeltaHistogram returns an Aggregator that summarizes a set of
// measurements as an histogram. Each histogram is scoped by attributes and
// the aggregation cycle the measurements were made in.
//
// Each aggregation cycle is treated independently. When the returned
// Aggregator's Aggregations method is called it will reset all histogram
// counts to zero.
func newDeltaHistogram[N int64 | float64](cfg aggregation.ExplicitBucketHistogram) aggregator[N] {
	return &deltaHistogram[N]{
		histValues: newHistValues[N](cfg.Boundaries),
		noMinMax:   cfg.NoMinMax,
		start:      now(),
	}
}

// deltaHistogram summarizes a set of measurements made in a single
// aggregation cycle as an histogram with explicitly defined buckets.
type deltaHistogram[N int64 | float64] struct {
	*histValues[N]

	noMinMax bool
	start    time.Time
}

func (s *deltaHistogram[N]) Aggregation() metricdata.Aggregation {
	s.valuesMu.Lock()
	defer s.valuesMu.Unlock()

	if len(s.values) == 0 {
		return nil
	}

	t := now()
	// Do not allow modification of our copy of bounds.
	bounds := make([]float64, len(s.bounds))
	copy(bounds, s.bounds)
	h := metricdata.Histogram[N]{
		Temporality: metricdata.DeltaTemporality,
		DataPoints:  make([]metricdata.HistogramDataPoint[N], 0, len(s.values)),
	}
	for a, b := range s.values {
		hdp := metricdata.HistogramDataPoint[N]{
			Attributes:   a,
			StartTime:    s.start,
			Time:         t,
			Count:        b.count,
			Bounds:       bounds,
			BucketCounts: b.counts,
			Sum:          b.sum,
		}
		if !s.noMinMax {
			hdp.Min = metricdata.NewExtrema(b.min)
			hdp.Max = metricdata.NewExtrema(b.max)
		}
		h.DataPoints = append(h.DataPoints, hdp)

		// Unused attribute sets do not report.
		delete(s.values, a)
	}
	// The delta collection cycle resets.
	s.start = t
	return h
}

// newCumulativeHistogram returns an Aggregator that summarizes a set of
// measurements as an histogram. Each histogram is scoped by attributes.
//
// Each aggregation cycle builds from the previous, the histogram counts are
// the bucketed counts of all values aggregated since the returned Aggregator
// was created.
func newCumulativeHistogram[N int64 | float64](cfg aggregation.ExplicitBucketHistogram) aggregator[N] {
	return &cumulativeHistogram[N]{
		histValues: newHistValues[N](cfg.Boundaries),
		noMinMax:   cfg.NoMinMax,
		start:      now(),
	}
}

// cumulativeHistogram summarizes a set of measurements made over all
// aggregation cycles as an histogram with explicitly defined buckets.
type cumulativeHistogram[N int64 | float64] struct {
	*histValues[N]

	noMinMax bool
	start    time.Time
}

func (s *cumulativeHistogram[N]) Aggregation() metricdata.Aggregation {
	s.valuesMu.Lock()
	defer s.valuesMu.Unlock()

	if len(s.values) == 0 {
		return nil
	}

	t := now()
	// Do not allow modification of our copy of bounds.
	bounds := make([]float64, len(s.bounds))
	copy(bounds, s.bounds)
	h := metricdata.Histogram[N]{
		Temporality: metricdata.CumulativeTemporality,
		DataPoints:  make([]metricdata.HistogramDataPoint[N], 0, len(s.values)),
	}
	for a, b := range s.values {
		// The HistogramDataPoint field values returned need to be copies of
		// the buckets value as we will keep updating them.
		//
		// TODO (#3047): Making copies for bounds and counts incurs a large
		// memory allocation footprint. Alternatives should be explored.
		counts := make([]uint64, len(b.counts))
		copy(counts, b.counts)

		hdp := metricdata.HistogramDataPoint[N]{
			Attributes:   a,
			StartTime:    s.start,
			Time:         t,
			Count:        b.count,
			Bounds:       bounds,
			BucketCounts: counts,
			Sum:          b.sum,
		}
		if !s.noMinMax {
			hdp.Min = metricdata.NewExtrema(b.min)
			hdp.Max = metricdata.NewExtrema(b.max)
		}
		h.DataPoints = append(h.DataPoints, hdp)
		// TODO (#3006): This will use an unbounded amount of memory if there
		// are unbounded number of attribute sets being aggregated. Attribute
		// sets that become "stale" need to be forgotten so this will not
		// overload the system.
	}
	return h
}
