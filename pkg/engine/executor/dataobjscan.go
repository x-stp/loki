package executor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/grafana/loki/v3/pkg/dataobj"
	"github.com/grafana/loki/v3/pkg/dataobj/sections/logs"
	"github.com/grafana/loki/v3/pkg/dataobj/sections/streams"
	"github.com/grafana/loki/v3/pkg/engine/internal/datatype"
	"github.com/grafana/loki/v3/pkg/engine/internal/types"
	"github.com/grafana/loki/v3/pkg/engine/planner/physical"
)

type dataobjScan struct {
	opts   dataobjScanOptions
	logger log.Logger

	initialized bool
	reader      *logs.RowReader
	streams     map[int64]labels.Labels
	records     []logs.Record

	state state
}

type dataobjScanOptions struct {
	// TODO(rfratto): Limiting each DataObjScan to a single section is going to
	// be critical for limiting memory overhead here; the section is intended to
	// be the smallest unit of parallelization.

	Object      *dataobj.Object             // Object to read from.
	StreamIDs   []int64                     // Stream IDs to match from logs sections.
	Section     int                         // Logs section to fetch.
	Predicates  []logs.RowPredicate         // Predicate to apply to the logs.
	Projections []physical.ColumnExpression // Columns to include. An empty slice means all columns.

	Direction physical.SortOrder // Order of timestamps to return (ASC=Forward, DESC=Backward)
	Limit     uint32             // A limit on the number of rows to return (0=unlimited).

	batchSize int64 // The buffer size for reading rows, derived from the engine batch size.
}

var _ Pipeline = (*dataobjScan)(nil)

// newDataobjScanPipeline creates a new Pipeline which emits a single
// [arrow.Record] composed of all log sections in a data object. Rows in the
// returned record are ordered by timestamp in the direction specified by
// opts.Direction.
func newDataobjScanPipeline(opts dataobjScanOptions, logger log.Logger) *dataobjScan {
	if opts.Direction == physical.ASC {
		// It's ok to panic here, because the validation of log query direction is performed in the logical planner.
		panic("sorting by timestamp ASC is not supported by DataObjScan")
	}
	return &dataobjScan{opts: opts, logger: logger}
}

// Read retrieves the next [arrow.Record] from the dataobj.
func (s *dataobjScan) Read(ctx context.Context) error {
	if err := s.init(ctx); err != nil {
		return err
	}

	rec, err := s.read(ctx)
	s.state = newState(rec, err)

	if err != nil {
		return fmt.Errorf("reading data object: %w", err)
	}
	return nil
}

func (s *dataobjScan) init(ctx context.Context) error {
	if s.initialized {
		return nil
	}

	s.records = make([]logs.Record, 0, s.opts.batchSize)

	if err := s.initStreams(ctx); err != nil {
		return fmt.Errorf("initializing streams: %w", err)
	}

	s.reader = nil

	for idx, section := range s.opts.Object.Sections().Filter(logs.CheckSection) {
		// Filter out sections that are not part of this shard
		if s.opts.Section != idx {
			continue
		}

		sec, err := logs.Open(ctx, section)
		if err != nil {
			return fmt.Errorf("opening logs section: %w", err)
		}

		// TODO:(ashwanth): [dataobjscan] only supports reading logs sections
		// that are sorted primarily by timestamp in DESC order.
		//
		// Other sort orders should be supported by wrapping the scan with TopK
		// either during planning or execution.
		{
			colType, sortOrder, err := sec.PrimarySortOrder()
			if err != nil {
				level.Warn(s.logger).Log("msg", "missing sort order information", "section", idx)
			} else if colType != logs.ColumnTypeTimestamp || sortOrder != logs.SortDirectionDescending {
				level.Warn(s.logger).Log("msg", "section is not sorted by timestamp in DESC order",
					"dataobj", idx, "primaryColumnType", colType, "sortOrder", sortOrder)
			}
		}

		// TODO(rfratto): There's a few problems with using LogsReader as it is:
		//
		// 1. LogsReader doesn't support providing a subset of columns to read
		//    from, so we're applying projections after reading.
		//
		// 2. LogsReader is intended to be pooled to reduce memory, but we're
		//    creating a new one every time here.
		//
		// For the sake of the initial implementation I'm ignoring these issues,
		// but we'll absolutely need to solve this prior to production use.
		lr := logs.NewRowReader(sec)

		// The calls below can't fail because we're always using a brand new logs
		// reader.
		_ = lr.MatchStreams(slices.Values(s.opts.StreamIDs))
		_ = lr.SetPredicates(s.opts.Predicates)

		s.reader = lr
		break
	}

	if s.reader == nil {
		return fmt.Errorf("no logs section %d found", s.opts.Section)
	}

	s.initialized = true
	return nil
}

// initStreams retrieves all requested stream records from streams sections so
// that emitted [arrow.Record]s can include stream labels in results.
func (s *dataobjScan) initStreams(ctx context.Context) error {
	var sr streams.RowReader
	defer sr.Close()

	streamsBuf := make([]streams.Stream, s.opts.batchSize)

	// Initialize entries in the map so we can do a presence test in the loop
	// below.
	s.streams = make(map[int64]labels.Labels, len(s.opts.StreamIDs))
	for _, id := range s.opts.StreamIDs {
		s.streams[id] = labels.EmptyLabels()
	}

	for _, section := range s.opts.Object.Sections().Filter(streams.CheckSection) {
		sec, err := streams.Open(ctx, section)
		if err != nil {
			return fmt.Errorf("opening streams section: %w", err)
		}

		// TODO(rfratto): dataobj.StreamsPredicate is missing support for filtering
		// on stream IDs when we already know them in advance; this can cause the
		// Read here to take longer than it needs to since we're reading the
		// entirety of every row.
		sr.Reset(sec)

		for {
			n, err := sr.Read(ctx, streamsBuf)
			if n == 0 && errors.Is(err, io.EOF) {
				return nil
			} else if err != nil && !errors.Is(err, io.EOF) {
				return err
			}

			for i, stream := range streamsBuf[:n] {
				if _, found := s.streams[stream.ID]; !found {
					continue
				}

				s.streams[stream.ID] = stream.Labels.Copy()

				// Zero out the stream entry from the slice so the next call to sr.Read
				// doesn't overwrite any memory we just moved to s.streams.
				streamsBuf[i] = streams.Stream{}
			}
		}
	}

	// Check that all streams were populated.
	var errs []error
	for id, labels := range s.streams {
		if labels.IsEmpty() {
			errs = append(errs, fmt.Errorf("requested stream ID %d not found in any stream section", id))
		}
	}
	return errors.Join(errs...)
}

// read reads the entire data object into memory and generates an arrow.Record
// from the data. It returns an error upon encountering an error while reading
// one of the sections.
func (s *dataobjScan) read(ctx context.Context) (arrow.Record, error) {
	var (
		n   int   // number of rows yielded by the datobj reader
		err error // error yielded by the dataobj reader
	)

	// Read from the dataobj until it yields at least one row, to avoid these function calls from the parent.
	for n == 0 {
		// Reset buffer
		s.records = s.records[:s.opts.batchSize]

		n, err = s.reader.Read(ctx, s.records)
		if n == 0 && errors.Is(err, io.EOF) {
			return nil, EOF
		} else if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	s.records = s.records[:n]

	projections, err := s.effectiveProjections(s.records)
	if err != nil {
		return nil, fmt.Errorf("getting effective projections: %w", err)
	}

	schema, err := schemaFromColumns(projections)
	if err != nil {
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	// TODO(rfratto): pass allocator to builder
	rb := array.NewRecordBuilder(memory.NewGoAllocator(), schema)
	defer rb.Release()

	for _, record := range s.records {
		for i := 0; i < schema.NumFields(); i++ {
			field, builder := rb.Schema().Field(i), rb.Field(i)
			s.appendToBuilder(builder, &field, &record)
		}
	}

	return rb.NewRecord(), nil
}

// getLessFunc returns a "less comparison" function for records for the sort heap.
// direction determines the search order:
// BACKWARD is a backward search starting at the end of the time range.
// FORWARD is a forward search starting at the beginning of the time range.
// If two records have the same timestamp, the compareStreams function is used to determine the sort order.
func (s *dataobjScan) getLessFunc(direction physical.SortOrder) func(a, b logs.Record) bool {
	compareStreams := func(a, b logs.Record) bool {
		aStream, ok := s.streams[a.StreamID]
		if !ok {
			return false
		}

		bStream, ok := s.streams[b.StreamID]
		if !ok {
			return true
		}

		return labels.Compare(aStream, bStream) < 0
	}

	switch direction {
	case physical.ASC:
		return func(a, b logs.Record) bool {
			if a.Timestamp.Equal(b.Timestamp) {
				compareStreams(a, b)
			}
			return a.Timestamp.After(b.Timestamp)
		}
	case physical.DESC:
		return func(a, b logs.Record) bool {
			if a.Timestamp.Equal(b.Timestamp) {
				compareStreams(a, b)
			}
			return a.Timestamp.Before(b.Timestamp)
		}
	default:
		panic("invalid direction")
	}
}

// effectiveProjections returns the effective projections to return for a
// record. If s.opts.Projections is non-empty, then its column expressions are
// used for the projections.
//
// Otherwise, the set of all columns found in the heap are used, in order of:
//
// * All stream labels (sorted by name)
// * All metadata columns (sorted by name)
// * Log timestamp
// * Log message
//
// effectiveProjections does not mutate h.
func (s *dataobjScan) effectiveProjections(records []logs.Record) ([]physical.ColumnExpression, error) {
	if len(s.opts.Projections) > 0 {
		return s.opts.Projections, nil
	}

	var (
		columns      []physical.ColumnExpression
		foundStreams = map[int64]struct{}{}
		found        = map[physical.ColumnExpr]struct{}{}
	)

	addColumn := func(name string, ty types.ColumnType) {
		expr := physical.ColumnExpr{
			Ref: types.ColumnRef{Column: name, Type: ty},
		}

		if _, ok := found[expr]; !ok {
			found[expr] = struct{}{}
			columns = append(columns, &expr)
		}
	}

	for _, rec := range records {
		stream, ok := s.streams[rec.StreamID]
		if !ok {
			// If we hit this, there's a problem with either initStreams (we missed a
			// requested stream) or the predicate application, where it returned a
			// stream we didn't want.
			return nil, fmt.Errorf("stream ID %d not found in stream cache", rec.StreamID)
		}

		if _, addedStream := foundStreams[rec.StreamID]; !addedStream {
			stream.Range(func(label labels.Label) {
				addColumn(label.Name, types.ColumnTypeLabel)
			})
			foundStreams[rec.StreamID] = struct{}{}
		}

		rec.Metadata.Range(func(label labels.Label) {
			addColumn(label.Name, types.ColumnTypeMetadata)
		})
	}

	// Sort existing columns by type (preferring labels) then name.
	slices.SortFunc(columns, func(a, b physical.ColumnExpression) int {
		aRef, bRef := a.(*physical.ColumnExpr).Ref, b.(*physical.ColumnExpr).Ref

		if aRef.Type != bRef.Type {
			if aRef.Type == types.ColumnTypeLabel {
				return -1 // Labels first.
			}
			return 1
		}

		return cmp.Compare(aRef.Column, bRef.Column)
	})

	// Add fixed columns at the end.
	addColumn(types.ColumnNameBuiltinTimestamp, types.ColumnTypeBuiltin)
	addColumn(types.ColumnNameBuiltinMessage, types.ColumnTypeBuiltin)

	return columns, nil
}

func schemaFromColumns(columns []physical.ColumnExpression) (*arrow.Schema, error) {
	var (
		fields       = make([]arrow.Field, 0, len(columns))
		fingerprints = make(map[string]struct{}, len(columns))
	)

	addField := func(field arrow.Field) {
		fp := field.Fingerprint()
		if field.HasMetadata() {
			// We differentiate column type using metadata, but the metadata isn't
			// included in the fingerprint, so we need to manually include it here.
			fp += field.Metadata.String()
		}

		if _, exist := fingerprints[fp]; exist {
			return
		}

		fields = append(fields, field)
		fingerprints[fp] = struct{}{}
	}

	for _, column := range columns {
		columnExpr, ok := column.(*physical.ColumnExpr)
		if !ok {
			return nil, fmt.Errorf("invalid column expression type %T", column)
		}

		switch columnExpr.Ref.Type {
		case types.ColumnTypeLabel:
			// TODO(rfratto): Switch to dictionary encoding for labels.
			//
			// Since labels are more likely to repeat than metadata, we could cut
			// down on the memory overhead of a record by dictionary encoding the
			// labels.
			//
			// However, the csv package we use for testing DataObjScan currently
			// (2025-05-02) doesn't support dictionary encoding, and we would need
			// to find a solution there.
			//
			// We skipped dictionary encoding for now to get the initial prototype
			// working.
			ty, md := arrowTypeFromColumnRef(columnExpr.Ref)
			addField(arrow.Field{
				Name:     columnExpr.Ref.Column,
				Type:     ty,
				Nullable: true,
				Metadata: md,
			})

		case types.ColumnTypeMetadata:
			// Metadata is *not* encoded using dictionary encoding since metadata is
			// has unconstrained cardinality. Using dictionary encoding would require
			// tracking every encoded value in the record, which is likely to be too
			// expensive.
			ty, md := arrowTypeFromColumnRef(columnExpr.Ref)
			addField(arrow.Field{
				Name:     columnExpr.Ref.Column,
				Type:     ty,
				Nullable: true,
				Metadata: md,
			})

		case types.ColumnTypeBuiltin:
			ty, md := arrowTypeFromColumnRef(columnExpr.Ref)
			addField(arrow.Field{
				Name:     columnExpr.Ref.Column,
				Type:     ty,
				Nullable: true,
				Metadata: md,
			})

		case types.ColumnTypeAmbiguous:
			// The best handling for ambiguous columns (in terms of the schema) is to
			// explode it out into multiple columns, one for each type. (Except for
			// parsed, which can't be emitted from DataObjScan right now).
			//
			// TODO(rfratto): should ambiguity be passed down like this? It's odd for
			// the returned schema to be different than the set of columns you asked
			// for.
			//
			// As an alternative, ambiguity could be handled by the planner, where it
			// performs the explosion and propagates the ambiguity down into the
			// predicates.
			//
			// If we're ok with the schema changing from what was requested, then we
			// could update this to resolve the ambiguity at [dataobjScan.effectiveProjections]
			// so we don't always explode out to the full set of columns.
			addField(arrow.Field{
				Name:     columnExpr.Ref.Column,
				Type:     datatype.Arrow.String,
				Nullable: true,
				Metadata: datatype.ColumnMetadata(types.ColumnTypeLabel, datatype.Loki.String),
			})
			addField(arrow.Field{
				Name:     columnExpr.Ref.Column,
				Type:     datatype.Arrow.String,
				Nullable: true,
				Metadata: datatype.ColumnMetadata(types.ColumnTypeMetadata, datatype.Loki.String),
			})

		case types.ColumnTypeParsed, types.ColumnTypeGenerated:
			return nil, fmt.Errorf("parsed column type not supported: %s", columnExpr.Ref.Type)
		}
	}

	return arrow.NewSchema(fields, nil), nil
}

func arrowTypeFromColumnRef(ref types.ColumnRef) (arrow.DataType, arrow.Metadata) {
	if ref.Type == types.ColumnTypeBuiltin {
		switch ref.Column {
		case types.ColumnNameBuiltinTimestamp:
			return arrow.FixedWidthTypes.Timestamp_ns, datatype.ColumnMetadataBuiltinTimestamp
		case types.ColumnNameBuiltinMessage:
			return arrow.BinaryTypes.String, datatype.ColumnMetadataBuiltinMessage
		default:
			panic(fmt.Sprintf("unsupported builtin column type %s", ref))
		}
	}

	return datatype.Arrow.String, datatype.ColumnMetadata(ref.Type, datatype.Loki.String)
}

// appendToBuilder appends a the provided field from record into the given
// builder. The metadata of field is used to determine the category of column.
// appendToBuilder panics if the type of field does not match the datatype of
// builder.
func (s *dataobjScan) appendToBuilder(builder array.Builder, field *arrow.Field, record *logs.Record) {
	columnType, ok := field.Metadata.GetValue(types.MetadataKeyColumnType)
	if !ok {
		// This shouldn't happen; we control the metadata here on the fields.
		panic(fmt.Sprintf("missing column type in field %s", field.Name))
	}

	switch columnType {
	case types.ColumnTypeLabel.String():
		stream, ok := s.streams[record.StreamID]
		if !ok {
			panic(fmt.Sprintf("stream ID %d not found in stream cache", record.StreamID))
		}

		val := stream.Get(field.Name)
		if val == "" {
			builder.(*array.StringBuilder).AppendNull()
		} else {
			builder.(*array.StringBuilder).Append(val)
		}

	case types.ColumnTypeMetadata.String():
		val := record.Metadata.Get(field.Name)
		if val == "" {
			builder.(*array.StringBuilder).AppendNull()
		} else {
			builder.(*array.StringBuilder).Append(val)
		}

	case types.ColumnTypeBuiltin.String():
		if field.Name == types.ColumnNameBuiltinTimestamp {
			ts, _ := arrow.TimestampFromTime(record.Timestamp, arrow.Nanosecond)
			builder.(*array.TimestampBuilder).Append(ts)
		} else if field.Name == types.ColumnNameBuiltinMessage {
			// Use the inner BinaryBuilder to avoid converting record.Line to a
			// string and back.
			builder.(*array.StringBuilder).BinaryBuilder.Append(record.Line)
		} else {
			panic(fmt.Sprintf("unsupported builtin column %s", field.Name))
		}

	default:
		// This shouldn't happen; we control the metadata here on the fields.
		panic(fmt.Sprintf("unsupported column type %s", columnType))
	}
}

// Value returns the current [arrow.Record] retrieved by the previous call to
// [dataobjScan.Read], or an error if the record cannot be read.
func (s *dataobjScan) Value() (arrow.Record, error) { return s.state.batch, s.state.err }

// Close closes s and releases all resources.
func (s *dataobjScan) Close() {
	if s.reader != nil {
		_ = s.reader.Close()
	}
}

// Inputs implements Pipeline and returns nil, since DataObjScan accepts no
// pipelines as input.
func (s *dataobjScan) Inputs() []Pipeline { return nil }

// Transport implements Pipeline and returns [Local].
func (s *dataobjScan) Transport() Transport { return Local }
