package frostdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/arrow/go/v12/arrow"
	"github.com/apache/arrow/go/v12/arrow/array"
	"github.com/apache/arrow/go/v12/arrow/memory"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/ulid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/segmentio/parquet-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"github.com/polarsignals/frostdb/dynparquet"
	schemapb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/schema/v1alpha1"
	schemav2pb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/schema/v1alpha2"
	tablepb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/table/v1alpha1"
	walpb "github.com/polarsignals/frostdb/gen/proto/go/frostdb/wal/v1alpha1"
	"github.com/polarsignals/frostdb/pqarrow"
	"github.com/polarsignals/frostdb/query/logicalplan"
	"github.com/polarsignals/frostdb/wal"
	walpkg "github.com/polarsignals/frostdb/wal"
)

var (
	ErrNoSchema     = fmt.Errorf("no schema")
	ErrTableClosing = fmt.Errorf("table closing")
)

type ErrWriteRow struct{ err error }

func (e ErrWriteRow) Error() string { return "failed to write row: " + e.err.Error() }

type ErrReadRow struct{ err error }

func (e ErrReadRow) Error() string { return "failed to read row: " + e.err.Error() }

type ErrCreateSchemaWriter struct{ err error }

func (e ErrCreateSchemaWriter) Error() string {
	return "failed to create schema write: " + e.err.Error()
}

type TableOption func(*tablepb.TableConfig) error

// WithRowGroupSize sets the size in number of rows for each row group for parquet files. A <= 0 value indicates no limit.
func WithRowGroupSize(numRows int) TableOption {
	return func(config *tablepb.TableConfig) error {
		config.RowGroupSize = uint64(numRows)
		return nil
	}
}

// WithBlockReaderLimit sets the limit of go routines that will be used to read persisted block files. A negative number indicates no limit.
func WithBlockReaderLimit(n int) TableOption {
	return func(config *tablepb.TableConfig) error {
		config.BlockReaderLimit = uint64(n)
		return nil
	}
}

// WithoutWAL disables the WAL for this table.
func WithoutWAL() TableOption {
	return func(config *tablepb.TableConfig) error {
		config.DisableWal = true
		return nil
	}
}

// FromConfig sets the table configuration from the given config.
// NOTE: that this does not override the schema even though that is included in the passed in config.
func FromConfig(config *tablepb.TableConfig) TableOption {
	return func(cfg *tablepb.TableConfig) error {
		if config.BlockReaderLimit != 0 { // the zero value is not a valid block reader limit
			cfg.BlockReaderLimit = config.BlockReaderLimit
		}
		cfg.DisableWal = config.DisableWal
		cfg.RowGroupSize = config.RowGroupSize
		return nil
	}
}

func defaultTableConfig() *tablepb.TableConfig {
	return &tablepb.TableConfig{
		BlockReaderLimit: uint64(runtime.GOMAXPROCS(0)),
	}
}

func NewTableConfig(
	schema proto.Message,
	options ...TableOption,
) *tablepb.TableConfig {
	t := defaultTableConfig()

	switch v := schema.(type) {
	case *schemapb.Schema:
		t.Schema = &tablepb.TableConfig_DeprecatedSchema{
			DeprecatedSchema: v,
		}
	case *schemav2pb.Schema:
		t.Schema = &tablepb.TableConfig_SchemaV2{
			SchemaV2: v,
		}
	default:
		panic(fmt.Sprintf("unsupported schema type: %T", v))
	}

	for _, opt := range options {
		_ = opt(t)
	}

	return t
}

type completedBlock struct {
	prevTx uint64
	tx     uint64
}

type Table struct {
	db      *DB
	name    string
	metrics *tableMetrics
	logger  log.Logger
	tracer  trace.Tracer

	config *tablepb.TableConfig
	schema *dynparquet.Schema

	pendingBlocks   map[*TableBlock]struct{}
	completedBlocks []completedBlock
	lastCompleted   uint64

	mtx    *sync.RWMutex
	active *TableBlock

	wal     WAL
	closing bool
}

type WAL interface {
	Close() error
	Log(tx uint64, record *walpb.Record) error
	LogRecord(tx uint64, table string, record arrow.Record) error
	// Replay replays WAL records from the given first index. If firstIndex is
	// 0, the first index read from the WAL is used (i.e. given a truncation,
	// using 0 is still valid). If the given firstIndex is less than the WAL's
	// first index on disk, the replay happens from the first index on disk.
	// If the handler panics, the WAL implementation will truncate the WAL up to
	// the last valid index.
	Replay(tx uint64, handler wal.ReplayHandlerFunc) error
	Truncate(tx uint64) error
	FirstIndex() (uint64, error)
	LastIndex() (uint64, error)
}

type TableBlock struct {
	table  *Table
	logger log.Logger
	tracer trace.Tracer

	ulid   ulid.ULID
	minTx  uint64
	prevTx uint64

	size *atomic.Int64
	// lastSnapshotSize keeps track of the size of the block when it last
	// triggered a snapshot.
	lastSnapshotSize atomic.Int64
	index            *LSM

	pendingWritersWg sync.WaitGroup
	pendingReadersWg sync.WaitGroup

	mtx *sync.RWMutex
}

type tableMetrics struct {
	blockRotated              prometheus.Counter
	granulesCreated           prometheus.Counter
	compactions               prometheus.Counter
	granulesSplits            prometheus.Counter
	rowsInserted              prometheus.Counter
	zeroRowsInserted          prometheus.Counter
	granulesCompactionAborted prometheus.Counter
	rowInsertSize             prometheus.Histogram
	lastCompletedBlockTx      prometheus.Gauge
	numParts                  prometheus.Gauge
	unsortedInserts           prometheus.Counter
	compactionMetrics         *compactionMetrics
}

func schemaFromTableConfig(tableConfig *tablepb.TableConfig) (*dynparquet.Schema, error) {
	switch schema := tableConfig.Schema.(type) {
	case *tablepb.TableConfig_DeprecatedSchema:
		return dynparquet.SchemaFromDefinition(schema.DeprecatedSchema)
	case *tablepb.TableConfig_SchemaV2:
		return dynparquet.SchemaFromDefinition(schema.SchemaV2)
	default:
		// No schema defined for table; read/only table
		return nil, nil
	}
}

func newTable(
	db *DB,
	name string,
	tableConfig *tablepb.TableConfig,
	reg prometheus.Registerer,
	logger log.Logger,
	tracer trace.Tracer,
	wal WAL,
) (*Table, error) {
	if db.columnStore.indexDegree <= 0 {
		msg := fmt.Sprintf("Table's columnStore index degree must be a positive integer (received %d)", db.columnStore.indexDegree)
		return nil, errors.New(msg)
	}

	if db.columnStore.splitSize < 2 {
		msg := fmt.Sprintf("Table's columnStore splitSize must be a positive integer > 1 (received %d)", db.columnStore.splitSize)
		return nil, errors.New(msg)
	}

	reg = prometheus.WrapRegistererWith(prometheus.Labels{"table": name}, reg)

	if tableConfig == nil {
		tableConfig = defaultTableConfig()
	}

	s, err := schemaFromTableConfig(tableConfig)
	if err != nil {
		return nil, err
	}

	t := &Table{
		db:     db,
		config: tableConfig,
		name:   name,
		logger: logger,
		tracer: tracer,
		mtx:    &sync.RWMutex{},
		wal:    wal,
		schema: s,
		metrics: &tableMetrics{
			numParts: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
				Name: "frostdb_table_num_parts",
				Help: "Number of parts currently active.",
			}),
			blockRotated: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "frostdb_table_blocks_rotated_total",
				Help: "Number of table blocks that have been rotated.",
			}),
			granulesCreated: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "frostdb_table_granules_created_total",
				Help: "Number of granules created.",
			}),
			compactions: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "frostdb_table_granules_compactions_total",
				Help: "Number of granule compactions.",
			}),
			granulesSplits: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "frostdb_table_granules_splits_total",
				Help: "Number of granules splits executed.",
			}),
			granulesCompactionAborted: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "frostdb_table_granules_compaction_aborted_total",
				Help: "Number of aborted granules compaction.",
			}),
			rowsInserted: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "frostdb_table_rows_inserted_total",
				Help: "Number of rows inserted into table.",
			}),
			zeroRowsInserted: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "frostdb_table_zero_rows_inserted_total",
				Help: "Number of times it was attempted to insert zero rows into the table.",
			}),
			rowInsertSize: promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
				Name:    "frostdb_table_row_insert_size",
				Help:    "Size of batch inserts into table.",
				Buckets: prometheus.ExponentialBuckets(1, 2, 10),
			}),
			lastCompletedBlockTx: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
				Name: "frostdb_table_last_completed_block_tx",
				Help: "Last completed block transaction.",
			}),
			unsortedInserts: promauto.With(reg).NewCounter(prometheus.CounterOpts{
				Name: "frostdb_table_unsorted_inserts_total",
				Help: "The number of times a buffer to insert was not in sorted order.",
			}),
			compactionMetrics: newCompactionMetrics(reg, float64(db.columnStore.granuleSizeBytes)),
		},
	}

	// Disable the WAL for this table by replacing any given WAL with a nop wal
	if tableConfig.DisableWal {
		t.wal = &walpkg.NopWAL{}
	}

	t.pendingBlocks = make(map[*TableBlock]struct{})

	promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "frostdb_table_index_size",
		Help: "Number of parts in the table index currently.",
	}, func() float64 {
		if active := t.ActiveBlock(); active != nil {
			return float64(0) // TODO THOR - implement
		}
		return 0
	})

	promauto.With(reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "frostdb_table_active_block_size",
		Help: "Size of the active table block in bytes.",
	}, func() float64 {
		if active := t.ActiveBlock(); active != nil {
			return float64(active.Size())
		}
		return 0
	})

	return t, nil
}

func (t *Table) newTableBlock(prevTx, tx uint64, id ulid.ULID) error {
	b, err := id.MarshalBinary()
	if err != nil {
		return err
	}

	if err := t.wal.Log(tx, &walpb.Record{
		Entry: &walpb.Entry{
			EntryType: &walpb.Entry_NewTableBlock_{
				NewTableBlock: &walpb.Entry_NewTableBlock{
					TableName: t.name,
					BlockId:   b,
					Config:    t.config,
				},
			},
		},
	}); err != nil {
		return err
	}

	t.active, err = newTableBlock(t, prevTx, tx, id)
	if err != nil {
		return err
	}

	return nil
}

func (t *Table) dropPendingBlock(block *TableBlock) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	delete(t.pendingBlocks, block)

	// Wait for outstanding readers to finish with the block before releasing underlying resources.
	block.pendingReadersWg.Wait()

	// TODO THOR need to release all the records in the index
}

func (t *Table) writeBlock(block *TableBlock, snapshotDB bool) {
	level.Debug(t.logger).Log("msg", "syncing block")
	block.pendingWritersWg.Wait()

	// from now on, the block will no longer be modified, we can persist it to disk

	level.Debug(t.logger).Log("msg", "done syncing block")

	// Persist the block
	err := block.Persist()
	t.dropPendingBlock(block)
	if err != nil {
		level.Error(t.logger).Log("msg", "failed to persist block")
		level.Error(t.logger).Log("msg", err.Error())
		return
	}

	tx, _, commit := t.db.begin()
	defer commit()

	buf, err := block.ulid.MarshalBinary()
	if err != nil {
		level.Error(t.logger).Log("msg", "failed to record block persistence in WAL: marshal ulid", "err", err)
		return
	}

	if err := t.wal.Log(tx, &walpb.Record{
		Entry: &walpb.Entry{
			EntryType: &walpb.Entry_TableBlockPersisted_{
				TableBlockPersisted: &walpb.Entry_TableBlockPersisted{
					TableName: t.name,
					BlockId:   buf,
				},
			},
		},
	}); err != nil {
		level.Error(t.logger).Log("msg", "failed to record block persistence in WAL", "err", err)
		return
	}

	t.mtx.Lock()
	t.completedBlocks = append(t.completedBlocks, completedBlock{prevTx: block.prevTx, tx: block.minTx})
	sort.Slice(t.completedBlocks, func(i, j int) bool {
		return t.completedBlocks[i].prevTx < t.completedBlocks[j].prevTx
	})
	for {
		if len(t.completedBlocks) == 0 {
			break
		}
		if t.completedBlocks[0].prevTx != t.lastCompleted {
			break
		}

		t.lastCompleted = t.completedBlocks[0].tx
		t.metrics.lastCompletedBlockTx.Set(float64(t.lastCompleted))
		t.completedBlocks = t.completedBlocks[1:]
	}
	t.mtx.Unlock()
	t.db.maintainWAL()
	/*
		if snapshotDB && t.db.columnStore.snapshotTriggerSize != 0 && t.db.columnStore.enableWAL {
			func() {
				if !t.db.snapshotInProgress.CompareAndSwap(false, true) {
					// Snapshot already in progress. This could lead to duplicate
					// data when replaying (refer to the snapshot design document),
					// but discarding this data on recovery is better than a
					// potential additional CPU spike caused by another snapshot.
					return
				}
				defer t.db.snapshotInProgress.Store(false)
				// This snapshot snapshots the new, active, table block. Refer to
				// the snapshot design document for more details as to why this
				// snapshot is necessary.
				// context.Background is used here for the snapshot since callers
				// might cancel the context when the write is finished but the
				// snapshot is not. Note that block.Persist does the same.
				// TODO(asubiotto): Eventually we should register a cancel function
				// that is called with a grace period on db.Close.
				ctx := context.Background()
				if err := t.db.snapshotAtTX(ctx, tx); err != nil {
					level.Error(t.logger).Log(
						"msg", "failed to write snapshot on block rotation",
						"err", err,
					)
				}
				if err := t.db.reclaimDiskSpace(ctx); err != nil {
					level.Error(t.logger).Log(
						"msg", "failed to reclaim disk space after snapshot on block rotation",
						"err", err,
					)
					return
				}
			}()
		}
	*/
}

func (t *Table) RotateBlock(ctx context.Context, block *TableBlock) error {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	// Need to check that we haven't already rotated this block.
	if t.active != block {
		return nil
	}

	level.Debug(t.logger).Log("msg", "rotating block", "blockSize", block.Size())
	defer func() {
		level.Debug(t.logger).Log("msg", "done rotating block")
	}()

	tx, _, commit := t.db.begin()
	defer commit()

	id := generateULID()
	if err := t.newTableBlock(t.active.minTx, tx, id); err != nil {
		return err
	}
	t.metrics.blockRotated.Inc()
	t.metrics.numParts.Set(float64(0))

	t.pendingBlocks[block] = struct{}{}
	go t.writeBlock(block, true /* snapshotDB */)

	return nil
}

func (t *Table) ActiveBlock() *TableBlock {
	t.mtx.RLock()
	defer t.mtx.RUnlock()

	return t.active
}

func (t *Table) ActiveWriteBlock() (*TableBlock, func(), error) {
	t.mtx.RLock()
	defer t.mtx.RUnlock()

	if t.closing {
		return nil, nil, ErrTableClosing
	}

	t.active.pendingWritersWg.Add(1)
	return t.active, t.active.pendingWritersWg.Done, nil
}

func (t *Table) Schema() *dynparquet.Schema {
	if t.config == nil {
		return nil
	}
	return t.schema
}

func (t *Table) EnsureCompaction() error {
	return t.ActiveBlock().EnsureCompaction()
}

func (t *Table) InsertRecord(ctx context.Context, record arrow.Record) (uint64, error) {
	block, close, err := t.appender(ctx)
	if err != nil {
		return 0, fmt.Errorf("get appender: %w", err)
	}
	defer close()

	tx, _, commit := t.db.begin()
	defer commit()

	if err := t.wal.LogRecord(tx, t.name, record); err != nil {
		return tx, fmt.Errorf("append to log: %w", err)
	}

	if err := block.InsertRecord(ctx, tx, record); err != nil {
		return tx, fmt.Errorf("insert buffer into block: %w", err)
	}

	return tx, nil
}

func (t *Table) appendToLog(ctx context.Context, tx uint64, buf []byte) error {
	if err := t.wal.Log(tx, &walpb.Record{
		Entry: &walpb.Entry{
			EntryType: &walpb.Entry_Write_{
				Write: &walpb.Entry_Write{
					Data:      buf,
					TableName: t.name,
				},
			},
		},
	}); err != nil {
		return err
	}
	return nil
}

func (t *Table) appender(ctx context.Context) (*TableBlock, func(), error) {
	for {
		// Using active write block is important because it ensures that we don't
		// miss pending writers when synchronizing the block.
		block, close, err := t.ActiveWriteBlock()
		if err != nil {
			return nil, nil, err
		}

		blockSize := block.Size()
		/*
			if t.db.columnStore.snapshotTriggerSize != 0 &&
				// If size-lastSnapshotSize > snapshotTriggerSize (a column store
				// option), a new snapshot is triggered. This is basically the size
				// of the new data in this block since the last snapshot.
				blockSize-block.lastSnapshotSize.Load() > t.db.columnStore.snapshotTriggerSize {
				// context.Background is used here for the snapshot since callers
				// might cancel the context when the write is finished but the
				// snapshot is not.
				// TODO(asubiotto): Eventually we should register a cancel function
				// that is called with a grace period on db.Close.
				t.db.asyncSnapshot(context.Background(), func() {
					level.Debug(t.logger).Log(
						"msg", "successful snapshot on block size trigger",
						"block_size", humanize.IBytes(uint64(blockSize)),
						"last_snapshot_size", humanize.IBytes(uint64(block.lastSnapshotSize.Load())),
					)
					block.lastSnapshotSize.Store(blockSize)
					if err := t.db.reclaimDiskSpace(ctx); err != nil {
						level.Error(t.logger).Log(
							"msg", "failed to reclaim disk space after snapshot",
							"err", err,
						)
						return
					}
				})
			}
		*/
		if blockSize < t.db.columnStore.activeMemorySize {
			return block, close, nil
		}

		// We need to rotate the block and the writer won't actually be used.
		close()

		err = t.RotateBlock(ctx, block)
		if err != nil {
			return nil, nil, fmt.Errorf("rotate block: %w", err)
		}
	}
}

func (t *Table) View(ctx context.Context, fn func(ctx context.Context, tx uint64) error) error {
	ctx, span := t.tracer.Start(ctx, "Table/View")
	tx := t.db.beginRead()
	span.SetAttributes(attribute.Int64("tx", int64(tx))) // Attributes don't support uint64...
	defer span.End()
	return fn(ctx, tx)
}

// Iterator iterates in order over all granules in the table. It stops iterating when the iterator function returns false.
func (t *Table) Iterator(
	ctx context.Context,
	tx uint64,
	pool memory.Allocator,
	callbacks []logicalplan.Callback,
	options ...logicalplan.Option,
) error {
	iterOpts := &logicalplan.IterOptions{}
	for _, opt := range options {
		opt(iterOpts)
	}
	ctx, span := t.tracer.Start(ctx, "Table/Iterator")
	span.SetAttributes(attribute.Int("physicalProjections", len(iterOpts.PhysicalProjection)))
	span.SetAttributes(attribute.Int("projections", len(iterOpts.Projection)))
	span.SetAttributes(attribute.Int("distinct", len(iterOpts.DistinctColumns)))
	defer span.End()

	if len(callbacks) == 0 {
		return errors.New("no callbacks provided")
	}
	rowGroups := make(chan any, len(callbacks)*4) // buffer up to 4 row groups per callback

	// Previously we sorted all row groups into a single row group here,
	// but it turns out that none of the downstream uses actually rely on
	// the sorting so it's not worth it in the general case. Physical plans
	// can decide to sort if they need to in order to exploit the
	// characteristics of sorted data.

	// bufferSize specifies a threshold of records past which the
	// buffered results are flushed to the next operator.
	const bufferSize = 1024

	errg, ctx := errgroup.WithContext(ctx)
	for _, callback := range callbacks {
		callback := callback
		errg.Go(func() error {
			converter := pqarrow.NewParquetConverter(pool, *iterOpts)
			defer converter.Close()

			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case rg, ok := <-rowGroups:
					if !ok {
						r := converter.NewRecord()
						if r == nil || r.NumRows() == 0 {
							return nil
						}
						if err := callback(ctx, r); err != nil {
							return err
						}
						r.Release()
						return nil
					}

					switch t := rg.(type) {
					case arrow.Record:
						err := callback(ctx, t)
						t.Release()
						if err != nil {
							return err
						}
					case dynparquet.DynamicRowGroup:
						if err := converter.Convert(ctx, t); err != nil {
							return fmt.Errorf("failed to convert row group to arrow record: %v", err)
						}
						// This RowGroup had no relevant data. Ignore it.
						if len(converter.Fields()) == 0 {
							continue
						}
						if converter.NumRows() >= bufferSize {
							r := converter.NewRecord()
							if err := callback(ctx, r); err != nil {
								return err
							}
							r.Release()
						}
					default:
						return fmt.Errorf("unknown row group type: %T", t)
					}
				}
			}
		})
	}

	errg.Go(func() error {
		if err := t.collectRowGroups(ctx, tx, iterOpts.Filter, rowGroups); err != nil {
			return err
		}
		close(rowGroups)
		return nil
	})

	return errg.Wait()
}

// SchemaIterator iterates in order over all granules in the table and returns
// all the schemas seen across the table.
func (t *Table) SchemaIterator(
	ctx context.Context,
	tx uint64,
	pool memory.Allocator,
	callbacks []logicalplan.Callback,
	options ...logicalplan.Option,
) error {
	iterOpts := &logicalplan.IterOptions{}
	for _, opt := range options {
		opt(iterOpts)
	}
	ctx, span := t.tracer.Start(ctx, "Table/SchemaIterator")
	span.SetAttributes(attribute.Int("physicalProjections", len(iterOpts.PhysicalProjection)))
	span.SetAttributes(attribute.Int("projections", len(iterOpts.Projection)))
	span.SetAttributes(attribute.Int("distinct", len(iterOpts.DistinctColumns)))
	defer span.End()

	if len(callbacks) == 0 {
		return errors.New("no callbacks provided")
	}

	rowGroups := make(chan any, len(callbacks)*4) // buffer up to 4 row groups per callback

	schema := arrow.NewSchema(
		[]arrow.Field{
			{Name: "name", Type: arrow.BinaryTypes.String},
		},
		nil,
	)

	errg, ctx := errgroup.WithContext(ctx)
	for _, callback := range callbacks {
		callback := callback
		errg.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case rg, ok := <-rowGroups:
					if !ok {
						return nil // we're done
					}

					b := array.NewRecordBuilder(pool, schema)

					switch t := rg.(type) {
					case arrow.Record:
						for _, f := range t.Schema().Fields() {
							b.Field(0).(*array.StringBuilder).Append(f.Name)
						}
						record := b.NewRecord()
						err := callback(ctx, record)
						record.Release()
						t.Release()
						if err != nil {
							return err
						}
					case dynparquet.DynamicRowGroup:
						if rg == nil {
							return errors.New("received nil rowGroup") // shouldn't happen, but anyway
						}
						parquetFields := t.Schema().Fields()
						fieldNames := make([]string, 0, len(parquetFields))
						for _, f := range parquetFields {
							fieldNames = append(fieldNames, f.Name())
						}

						b.Field(0).(*array.StringBuilder).AppendValues(fieldNames, nil)

						record := b.NewRecord()
						if err := callback(ctx, record); err != nil {
							return err
						}
						record.Release()
						b.Release()
					default:
						return fmt.Errorf("unknown row group type: %T", t)
					}
				}
			}
		})
	}

	errg.Go(func() error {
		if err := t.collectRowGroups(ctx, tx, iterOpts.Filter, rowGroups); err != nil {
			return err
		}
		close(rowGroups)
		return nil
	})

	return errg.Wait()
}

func generateULID() ulid.ULID {
	t := time.Now()
	entropy := ulid.Monotonic(rand.New(rand.NewSource(t.UnixNano())), 0)
	return ulid.MustNew(ulid.Timestamp(t), entropy)
}

func newTableBlock(table *Table, prevTx, tx uint64, id ulid.ULID) (*TableBlock, error) {
	return &TableBlock{
		table:  table,
		index:  NewLSM(table.name, L2),
		mtx:    &sync.RWMutex{},
		ulid:   id,
		size:   &atomic.Int64{},
		logger: table.logger,
		tracer: table.tracer,
		minTx:  tx,
		prevTx: prevTx,
	}, nil
}

// EnsureCompaction forces a TableBlock compaction.
func (t *TableBlock) EnsureCompaction() error {
	return t.compact(t.table.db.columnStore.compactionConfig)
}

func (t *TableBlock) InsertRecord(ctx context.Context, tx uint64, record arrow.Record) error {
	defer func() {
		t.table.metrics.rowsInserted.Add(float64(record.NumRows()))
		t.table.metrics.rowInsertSize.Observe(float64(record.NumRows()))
	}()

	if record.NumRows() == 0 {
		t.table.metrics.zeroRowsInserted.Add(1)
		return nil
	}

	t.index.Add(tx, record)
	return nil
}

// Size returns the cumulative size of all buffers in the table. This is roughly the size of the table in bytes.
func (t *TableBlock) Size() int64 {
	return t.size.Load()
}

// Serialize the table block into a single Parquet file.
func (t *TableBlock) Serialize(writer io.Writer) error {
	for i := L0; i < L2; i++ { // Lazy implementation
		if err := t.index.merge(i, t.table.Schema(), nil); err != nil {
			return err
		}
	}
	return t.index.merge(L2, t.table.Schema(), writer)
}

// parquetRowWriter is a stateful parquet row group writer.
type parquetRowWriter struct {
	schema *dynparquet.Schema
	w      *dynparquet.PooledWriter

	rowGroupSize int
	maxNumRows   int

	rowGroupRowsWritten int
	totalRowsWritten    int
	rowsBuf             []parquet.Row
}

type parquetRowWriterOption func(p *parquetRowWriter)

func withMaxRows(max int) parquetRowWriterOption {
	return func(p *parquetRowWriter) {
		p.maxNumRows = max
	}
}

// rowWriter returns a new Parquet row writer with the given dynamic columns.
func (t *TableBlock) rowWriter(writer io.Writer, dynCols map[string][]string, options ...parquetRowWriterOption) (*parquetRowWriter, error) {
	w, err := t.table.schema.GetWriter(writer, dynCols)
	if err != nil {
		return nil, err
	}

	buffSize := 256
	if t.table.config.RowGroupSize > 0 {
		buffSize = int(t.table.config.RowGroupSize)
	}

	p := &parquetRowWriter{
		w:            w,
		schema:       t.table.schema,
		rowsBuf:      make([]parquet.Row, buffSize),
		rowGroupSize: int(t.table.config.RowGroupSize),
	}

	for _, option := range options {
		option(p)
	}

	return p, nil
}

// WriteRows will write the given rows to the underlying Parquet writer. It returns the number of rows written.
func (p *parquetRowWriter) writeRows(rows parquet.Rows) (int, error) {
	written := 0
	for p.maxNumRows == 0 || p.totalRowsWritten < p.maxNumRows {
		if p.rowGroupSize > 0 && p.rowGroupRowsWritten+len(p.rowsBuf) > p.rowGroupSize {
			// Read only as many rows as we need to complete the row group size limit.
			p.rowsBuf = p.rowsBuf[:p.rowGroupSize-p.rowGroupRowsWritten]
		}
		if p.maxNumRows != 0 && p.totalRowsWritten+len(p.rowsBuf) > p.maxNumRows {
			// Read only as many rows as we need to write if they would bring
			// us over the limit.
			p.rowsBuf = p.rowsBuf[:p.maxNumRows-p.totalRowsWritten]
		}
		n, err := rows.ReadRows(p.rowsBuf)
		if err != nil && err != io.EOF {
			return 0, err
		}
		if n == 0 {
			break
		}

		if _, err = p.w.WriteRows(p.rowsBuf[:n]); err != nil {
			return 0, err
		}
		written += n
		p.rowGroupRowsWritten += n
		p.totalRowsWritten += n
		if p.rowGroupSize > 0 && p.rowGroupRowsWritten >= p.rowGroupSize {
			if err := p.w.Flush(); err != nil {
				return 0, err
			}
			p.rowGroupRowsWritten = 0
		}
	}

	return written, nil
}

func (p *parquetRowWriter) close() error {
	defer p.schema.PutWriter(p.w)
	return p.w.Close()
}

// memoryBlocks collects the active and pending blocks that are currently resident in memory.
// The pendingReadersWg.Done() function must be called on all blocks returned once processing is finished.
func (t *Table) memoryBlocks() ([]*TableBlock, uint64) {
	t.mtx.RLock()
	defer t.mtx.RUnlock()

	if t.active == nil { // this is currently a read only table
		return nil, 0
	}

	lastReadBlockTimestamp := t.active.ulid.Time()
	t.active.pendingReadersWg.Add(1)
	memoryBlocks := []*TableBlock{t.active}
	for block := range t.pendingBlocks {
		block.pendingReadersWg.Add(1)
		memoryBlocks = append(memoryBlocks, block)

		if block.ulid.Time() < lastReadBlockTimestamp {
			lastReadBlockTimestamp = block.ulid.Time()
		}
	}

	return memoryBlocks, lastReadBlockTimestamp
}

// collectRowGroups collects all the row groups from the table for the given filter.
func (t *Table) collectRowGroups(
	ctx context.Context,
	tx uint64,
	filterExpr logicalplan.Expr,
	rowGroups chan<- any,
) error {
	ctx, span := t.tracer.Start(ctx, "Table/collectRowGroups")
	defer span.End()

	// pending blocks could be uploaded to the bucket while we iterate on them.
	// to avoid to iterate on them again while reading the block file
	// we keep the last block timestamp to be read from the bucket and pass it to the IterateBucketBlocks() function
	// so that every block with a timestamp >= lastReadBlockTimestamp is discarded while being read.
	memoryBlocks, lastBlockTimestamp := t.memoryBlocks()
	defer func() {
		for _, block := range memoryBlocks {
			block.pendingReadersWg.Done()
		}
	}()
	sources := t.db.sources
	for _, block := range memoryBlocks {
		sources = append(sources, block.index)
	}

	// Collect from all data sources.
	for _, source := range sources {
		span.AddEvent(fmt.Sprintf("source/%s", source.String()))
		if err := source.Scan(ctx, filepath.Join(t.db.name, t.name), t.schema, filterExpr, lastBlockTimestamp, func(ctx context.Context, v any) error {
			rowGroups <- v
			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

// close notifies a table to stop accepting writes.
func (t *Table) close() {
	t.mtx.Lock()
	defer t.mtx.Unlock()

	t.active.pendingWritersWg.Wait()
	t.closing = true
}
