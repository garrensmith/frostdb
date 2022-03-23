package physicalplan

import (
	"errors"
	"fmt"
	"hash/maphash"

	"github.com/apache/arrow/go/v7/arrow"
	"github.com/apache/arrow/go/v7/arrow/array"
	"github.com/apache/arrow/go/v7/arrow/math"
	"github.com/apache/arrow/go/v7/arrow/memory"
	"github.com/apache/arrow/go/v7/arrow/scalar"
	"github.com/dgryski/go-metro"
	"github.com/polarsignals/arcticdb/dynparquet"
	"github.com/polarsignals/arcticdb/query/logicalplan"
)

func Aggregate(
	pool memory.Allocator,
	s *dynparquet.Schema,
	agg *logicalplan.Aggregation,
) (*HashAggregate, error) {
	groupByMatchers := make([]logicalplan.ColumnMatcher, 0, len(agg.GroupExprs))
	for _, groupExpr := range agg.GroupExprs {
		groupByMatchers = append(groupByMatchers, groupExpr.Matcher())
	}

	var (
		aggFunc      logicalplan.AggFunc
		aggFuncFound bool

		aggColumnMatcher logicalplan.ColumnMatcher
		aggColumnFound   bool
	)

	agg.AggExpr.Accept(PreExprVisitorFunc(func(expr logicalplan.Expr) bool {
		switch e := expr.(type) {
		case logicalplan.AggregationFunction:
			aggFunc = e.Func
			aggFuncFound = true
		case logicalplan.Column:
			aggColumnMatcher = e.Matcher()
			aggColumnFound = true
		}

		return true
	}))

	if !aggFuncFound {
		return nil, errors.New("aggregation function not found")
	}

	if !aggColumnFound {
		return nil, errors.New("aggregation column not found")
	}

	f, err := chooseAggregationFunction(aggFunc, agg.AggExpr.DataType(s))
	if err != nil {
		return nil, err
	}

	return NewHashAggregate(
		pool,
		agg.AggExpr.Name(),
		f,
		aggColumnMatcher,
		groupByMatchers,
	), nil
}

func chooseAggregationFunction(
	aggFunc logicalplan.AggFunc,
	dataType arrow.DataType,
) (AggregationFunction, error) {
	switch aggFunc {
	case logicalplan.SumAggFunc:
		switch dataType.ID() {
		case arrow.INT64:
			return &Int64SumAggregation{}, nil
		default:
			return nil, fmt.Errorf("unsupported sum of type: %s", dataType.Name())
		}
	default:
		return nil, fmt.Errorf("unsupported aggregation function: %s", aggFunc.String())
	}
}

type AggregationFunction interface {
	Aggregate(pool memory.Allocator, arrs []arrow.Array) (arrow.Array, error)
}

type HashAggregate struct {
	pool                  memory.Allocator
	resultColumnName      string
	groupByCols           map[string]array.Builder
	arraysToAggregate     []array.Builder
	hashToAggregate       map[uint64]int
	groupByColumnMatchers []logicalplan.ColumnMatcher
	columnToAggregate     logicalplan.ColumnMatcher
	aggregationFunction   AggregationFunction
	hashSeed              maphash.Seed
	nextCallback          func(r arrow.Record) error
}

func NewHashAggregate(
	pool memory.Allocator,
	resultColumnName string,
	aggregationFunction AggregationFunction,
	columnToAggregate logicalplan.ColumnMatcher,
	groupByColumnMatchers []logicalplan.ColumnMatcher,
) *HashAggregate {
	return &HashAggregate{
		pool:              pool,
		resultColumnName:  resultColumnName,
		groupByCols:       map[string]array.Builder{},
		arraysToAggregate: make([]array.Builder, 0),
		hashToAggregate:   map[uint64]int{},
		columnToAggregate: columnToAggregate,
		// TODO: Matchers can be optimized to be something like a radix tree or just a fast-lookup datastructure for exact matches or prefix matches.
		groupByColumnMatchers: groupByColumnMatchers,
		hashSeed:              maphash.MakeSeed(),
		aggregationFunction:   aggregationFunction,
	}
}

func (a *HashAggregate) SetNextCallback(nextCallback func(r arrow.Record) error) {
	a.nextCallback = nextCallback
}

func (a *HashAggregate) Callback(r arrow.Record) error {
	groupByFields := make([]arrow.Field, 0, 10)
	groupByArrays := make([]arrow.Array, 0, 10)
	var columnToAggregate arrow.Array
	aggregateFieldFound := false

	for i, field := range r.Schema().Fields() {
		for _, matcher := range a.groupByColumnMatchers {
			if matcher.Match(field.Name) {
				groupByFields = append(groupByFields, field)
				groupByArrays = append(groupByArrays, r.Column(i))
			}
		}

		if a.columnToAggregate.Match(field.Name) {
			columnToAggregate = r.Column(i)
			aggregateFieldFound = true
		}
	}

	if !aggregateFieldFound {
		return errors.New("aggregate field not found, aggregations are not possible without it")
	}

	numRows := int(r.NumRows())

	colScalars := make([]scalar.Scalar, len(groupByFields))
	for i := 0; i < numRows; i++ {
		colScalars = colScalars[:0]

		for _, arr := range groupByArrays {
			colScalar, err := scalar.GetScalar(arr, i)
			if err != nil {
				return err
			}

			colScalars = append(colScalars, colScalar)
		}

		hash := uint64(0)
		for j, colScalar := range colScalars {
			if colScalar == nil || !colScalar.IsValid() {
				continue
			}

			// TODO: This is extremely naive and will probably cause a ton of collisions.
			hash ^= metro.Hash64Str(groupByFields[j].Name, 0)
			hash ^= scalar.Hash(a.hashSeed, colScalar)
		}

		s, err := scalar.GetScalar(columnToAggregate, i)
		if err != nil {
			return err
		}

		k, ok := a.hashToAggregate[hash]
		if !ok {
			agg := array.NewBuilder(a.pool, s.DataType())
			a.arraysToAggregate = append(a.arraysToAggregate, agg)
			k = len(a.arraysToAggregate) - 1
			a.hashToAggregate[hash] = k

			// insert new row into columns grouped by and create new aggregate array to append to.
			for j, colScalar := range colScalars {
				fieldName := groupByFields[j].Name

				groupByCol, found := a.groupByCols[fieldName]
				if !found {
					groupByCol = array.NewBuilder(a.pool, groupByFields[j].Type)
					a.groupByCols[fieldName] = groupByCol
				}

				// We already appended to the arrays to aggregate, so we have
				// to account for that. We only want to back-fill null values
				// up until the index that we are about to insert into.
				for groupByCol.Len() < len(a.arraysToAggregate)-1 {
					groupByCol.AppendNull()
				}

				err := appendValue(groupByCol, colScalar)
				if err != nil {
					return err
				}
			}
		}

		if err := appendValue(a.arraysToAggregate[k], s); err != nil {
			return err
		}
	}

	return nil
}

func appendValue(arr array.Builder, s scalar.Scalar) error {
	if s == nil || !s.IsValid() {
		arr.AppendNull()
		return nil
	}

	switch s := s.(type) {
	case *scalar.Int64:
		arr.(*array.Int64Builder).Append(s.Value)
		return nil
	case *scalar.String:
		arr.(*array.StringBuilder).Append(string(s.Data()))
		return nil
	case *scalar.FixedSizeBinary:
		arr.(*array.FixedSizeBinaryBuilder).Append(s.Data())
		return nil
	case *scalar.List:
		// TODO: This seems horribly inefficient, we already have the whole
		// array and are just doing an expensive copy, but arrow doesn't seem
		// to be able to append whole list scalars at once.
		length := s.Value.Len()
		larr := arr.(*array.ListBuilder)
		vb := larr.ValueBuilder()
		larr.Append(true)
		for i := 0; i < length; i++ {
			v, err := scalar.GetScalar(s.Value, i)
			if err != nil {
				return err
			}

			err = appendValue(vb, v)
			if err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.New("unsupported type")
	}
}

func (a *HashAggregate) Finish() error {
	numCols := len(a.groupByCols) + 1

	groupByFields := make([]arrow.Field, 0, numCols)
	groupByArrays := make([]arrow.Array, 0, numCols)
	for fieldName, groupByCol := range a.groupByCols {
		arr := groupByCol.NewArray()
		groupByFields = append(groupByFields, arrow.Field{Name: fieldName, Type: arr.DataType()})
		groupByArrays = append(groupByArrays, arr)
	}

	arrs := make([]arrow.Array, 0, len(a.arraysToAggregate))
	for _, arr := range a.arraysToAggregate {
		arrs = append(arrs, arr.NewArray())
	}

	aggregateArray, err := a.aggregationFunction.Aggregate(a.pool, arrs)
	if err != nil {
		return fmt.Errorf("aggregate batched arrays: %w", err)
	}

	aggregateField := arrow.Field{Name: a.resultColumnName, Type: aggregateArray.DataType()}

	return a.nextCallback(array.NewRecord(
		arrow.NewSchema(append(groupByFields, aggregateField), nil),
		append(groupByArrays, aggregateArray),
		int64(aggregateArray.Len()),
	))
}

type Int64SumAggregation struct{}

var (
	ErrUnsupportedSumType = errors.New("unsupported type for sum aggregation, expected int64")
)

func (a *Int64SumAggregation) Aggregate(pool memory.Allocator, arrs []arrow.Array) (arrow.Array, error) {
	if len(arrs) == 0 {
		return array.NewInt64Builder(pool).NewArray(), nil
	}

	typ := arrs[0].DataType().ID()
	switch typ {
	case arrow.INT64:
		return sumInt64arrays(pool, arrs), nil
	default:
		return nil, fmt.Errorf("sum array of %s: %w", typ, ErrUnsupportedSumType)
	}
}

func sumInt64arrays(pool memory.Allocator, arrs []arrow.Array) arrow.Array {
	res := array.NewInt64Builder(pool)
	for _, arr := range arrs {
		res.Append(sumInt64array(arr.(*array.Int64)))
	}

	return res.NewArray()
}

func sumInt64array(arr *array.Int64) int64 {
	return math.Int64.Sum(arr)
}
