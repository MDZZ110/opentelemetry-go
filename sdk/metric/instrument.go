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

package metric // import "go.opentelemetry.io/otel/sdk/metric"

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/aggregation"
	"go.opentelemetry.io/otel/sdk/metric/internal/aggregate"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var (
	zeroInstrumentKind InstrumentKind
	zeroScope          instrumentation.Scope
)

// InstrumentKind is the identifier of a group of instruments that all
// performing the same function.
type InstrumentKind uint8

const (
	// instrumentKindUndefined is an undefined instrument kind, it should not
	// be used by any initialized type.
	instrumentKindUndefined InstrumentKind = iota // nolint:deadcode,varcheck,unused
	// InstrumentKindCounter identifies a group of instruments that record
	// increasing values synchronously with the code path they are measuring.
	InstrumentKindCounter
	// InstrumentKindUpDownCounter identifies a group of instruments that
	// record increasing and decreasing values synchronously with the code path
	// they are measuring.
	InstrumentKindUpDownCounter
	// InstrumentKindHistogram identifies a group of instruments that record a
	// distribution of values synchronously with the code path they are
	// measuring.
	InstrumentKindHistogram
	// InstrumentKindObservableCounter identifies a group of instruments that
	// record increasing values in an asynchronous callback.
	InstrumentKindObservableCounter
	// InstrumentKindObservableUpDownCounter identifies a group of instruments
	// that record increasing and decreasing values in an asynchronous
	// callback.
	InstrumentKindObservableUpDownCounter
	// InstrumentKindObservableGauge identifies a group of instruments that
	// record current values in an asynchronous callback.
	InstrumentKindObservableGauge
)

type nonComparable [0]func() // nolint: unused  // This is indeed used.

// Instrument describes properties an instrument is created with.
type Instrument struct {
	// Name is the human-readable identifier of the instrument.
	Name string
	// Description describes the purpose of the instrument.
	Description string
	// Kind defines the functional group of the instrument.
	Kind InstrumentKind
	// Unit is the unit of measurement recorded by the instrument.
	Unit string
	// Scope identifies the instrumentation that created the instrument.
	Scope instrumentation.Scope

	// Ensure forward compatibility if non-comparable fields need to be added.
	nonComparable // nolint: unused
}

// empty returns if all fields of i are their zero-value.
func (i Instrument) empty() bool {
	return i.Name == "" &&
		i.Description == "" &&
		i.Kind == zeroInstrumentKind &&
		i.Unit == "" &&
		i.Scope == zeroScope
}

// matches returns whether all the non-zero-value fields of i match the
// corresponding fields of other. If i is empty it will match all other, and
// true will always be returned.
func (i Instrument) matches(other Instrument) bool {
	return i.matchesName(other) &&
		i.matchesDescription(other) &&
		i.matchesKind(other) &&
		i.matchesUnit(other) &&
		i.matchesScope(other)
}

// matchesName returns true if the Name of i is "" or it equals the Name of
// other, otherwise false.
func (i Instrument) matchesName(other Instrument) bool {
	return i.Name == "" || i.Name == other.Name
}

// matchesDescription returns true if the Description of i is "" or it equals
// the Description of other, otherwise false.
func (i Instrument) matchesDescription(other Instrument) bool {
	return i.Description == "" || i.Description == other.Description
}

// matchesKind returns true if the Kind of i is its zero-value or it equals the
// Kind of other, otherwise false.
func (i Instrument) matchesKind(other Instrument) bool {
	return i.Kind == zeroInstrumentKind || i.Kind == other.Kind
}

// matchesUnit returns true if the Unit of i is its zero-value or it equals the
// Unit of other, otherwise false.
func (i Instrument) matchesUnit(other Instrument) bool {
	return i.Unit == "" || i.Unit == other.Unit
}

// matchesScope returns true if the Scope of i is its zero-value or it equals
// the Scope of other, otherwise false.
func (i Instrument) matchesScope(other Instrument) bool {
	return (i.Scope.Name == "" || i.Scope.Name == other.Scope.Name) &&
		(i.Scope.Version == "" || i.Scope.Version == other.Scope.Version) &&
		(i.Scope.SchemaURL == "" || i.Scope.SchemaURL == other.Scope.SchemaURL)
}

// Stream describes the stream of data an instrument produces.
type Stream struct {
	// Name is the human-readable identifier of the stream.
	Name string
	// Description describes the purpose of the data.
	Description string
	// Unit is the unit of measurement recorded.
	Unit string
	// Aggregation the stream uses for an instrument.
	Aggregation aggregation.Aggregation
	// AllowAttributeKeys are an allow-list of attribute keys that will be
	// preserved for the stream. Any attribute recorded for the stream with a
	// key not in this slice will be dropped.
	//
	// If this slice is empty, all attributes will be kept.
	AllowAttributeKeys []attribute.Key
}

// attributeFilter returns an attribute.Filter that only allows attributes
// with keys in s.AttributeKeys.
//
// If s.AttributeKeys is empty an accept-all filter is returned.
func (s Stream) attributeFilter() attribute.Filter {
	if len(s.AllowAttributeKeys) <= 0 {
		return func(kv attribute.KeyValue) bool { return true }
	}

	allowed := make(map[attribute.Key]struct{})
	for _, k := range s.AllowAttributeKeys {
		allowed[k] = struct{}{}
	}
	return func(kv attribute.KeyValue) bool {
		_, ok := allowed[kv.Key]
		return ok
	}
}

// streamID are the identifying properties of a stream.
type streamID struct {
	// Name is the name of the stream.
	Name string
	// Description is the description of the stream.
	Description string
	// Unit is the unit of the stream.
	Unit string
	// Aggregation is the aggregation data type of the stream.
	Aggregation string
	// Monotonic is the monotonicity of an instruments data type. This field is
	// not used for all data types, so a zero value needs to be understood in the
	// context of Aggregation.
	Monotonic bool
	// Temporality is the temporality of a stream's data type. This field is
	// not used by some data types.
	Temporality metricdata.Temporality
	// Number is the number type of the stream.
	Number string
}

type int64Inst struct {
	measures []aggregate.Measure[int64]

	embedded.Int64Counter
	embedded.Int64UpDownCounter
	embedded.Int64Histogram
}

var _ metric.Int64Counter = (*int64Inst)(nil)
var _ metric.Int64UpDownCounter = (*int64Inst)(nil)
var _ metric.Int64Histogram = (*int64Inst)(nil)

func (i *int64Inst) Add(ctx context.Context, val int64, opts ...metric.AddOption) {
	c := metric.NewAddConfig(opts)
	i.aggregate(ctx, val, c.Attributes())
}

func (i *int64Inst) Record(ctx context.Context, val int64, opts ...metric.RecordOption) {
	c := metric.NewRecordConfig(opts)
	i.aggregate(ctx, val, c.Attributes())
}

func (i *int64Inst) aggregate(ctx context.Context, val int64, s attribute.Set) { // nolint:revive  // okay to shadow pkg with method.
	if err := ctx.Err(); err != nil {
		return
	}
	for _, in := range i.measures {
		in(ctx, val, s)
	}
}

type float64Inst struct {
	measures []aggregate.Measure[float64]

	embedded.Float64Counter
	embedded.Float64UpDownCounter
	embedded.Float64Histogram
}

var _ metric.Float64Counter = (*float64Inst)(nil)
var _ metric.Float64UpDownCounter = (*float64Inst)(nil)
var _ metric.Float64Histogram = (*float64Inst)(nil)

func (i *float64Inst) Add(ctx context.Context, val float64, opts ...metric.AddOption) {
	c := metric.NewAddConfig(opts)
	i.aggregate(ctx, val, c.Attributes())
}

func (i *float64Inst) Record(ctx context.Context, val float64, opts ...metric.RecordOption) {
	c := metric.NewRecordConfig(opts)
	i.aggregate(ctx, val, c.Attributes())
}

func (i *float64Inst) aggregate(ctx context.Context, val float64, s attribute.Set) {
	if err := ctx.Err(); err != nil {
		return
	}
	for _, in := range i.measures {
		in(ctx, val, s)
	}
}

// observablID is a comparable unique identifier of an observable.
type observablID[N int64 | float64] struct {
	name        string
	description string
	kind        InstrumentKind
	unit        string
	scope       instrumentation.Scope
}

type float64Observable struct {
	metric.Float64Observable
	*observable[float64]

	embedded.Float64ObservableCounter
	embedded.Float64ObservableUpDownCounter
	embedded.Float64ObservableGauge
}

var _ metric.Float64ObservableCounter = float64Observable{}
var _ metric.Float64ObservableUpDownCounter = float64Observable{}
var _ metric.Float64ObservableGauge = float64Observable{}

func newFloat64Observable(scope instrumentation.Scope, kind InstrumentKind, name, desc, u string, meas []aggregate.Measure[float64]) float64Observable {
	return float64Observable{
		observable: newObservable(scope, kind, name, desc, u, meas),
	}
}

type int64Observable struct {
	metric.Int64Observable
	*observable[int64]

	embedded.Int64ObservableCounter
	embedded.Int64ObservableUpDownCounter
	embedded.Int64ObservableGauge
}

var _ metric.Int64ObservableCounter = int64Observable{}
var _ metric.Int64ObservableUpDownCounter = int64Observable{}
var _ metric.Int64ObservableGauge = int64Observable{}

func newInt64Observable(scope instrumentation.Scope, kind InstrumentKind, name, desc, u string, meas []aggregate.Measure[int64]) int64Observable {
	return int64Observable{
		observable: newObservable(scope, kind, name, desc, u, meas),
	}
}

type observable[N int64 | float64] struct {
	metric.Observable
	observablID[N]

	measures []aggregate.Measure[N]
}

func newObservable[N int64 | float64](scope instrumentation.Scope, kind InstrumentKind, name, desc, u string, meas []aggregate.Measure[N]) *observable[N] {
	return &observable[N]{
		observablID: observablID[N]{
			name:        name,
			description: desc,
			kind:        kind,
			unit:        u,
			scope:       scope,
		},
		measures: meas,
	}
}

// observe records the val for the set of attrs.
func (o *observable[N]) observe(val N, s attribute.Set) {
	for _, in := range o.measures {
		in(context.Background(), val, s)
	}
}

var errEmptyAgg = errors.New("no aggregators for observable instrument")

// registerable returns an error if the observable o should not be registered,
// and nil if it should. An errEmptyAgg error is returned if o is effectively a
// no-op because it does not have any aggregators. Also, an error is returned
// if scope defines a Meter other than the one o was created by.
func (o *observable[N]) registerable(scope instrumentation.Scope) error {
	if len(o.measures) == 0 {
		return errEmptyAgg
	}
	if scope != o.scope {
		return fmt.Errorf(
			"invalid registration: observable %q from Meter %q, registered with Meter %q",
			o.name,
			o.scope.Name,
			scope.Name,
		)
	}
	return nil
}
