// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package redo

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/log"
	"github.com/pingcap/tiflow/cdc/contextutil"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/cdc/processor/tablepb"
	"github.com/pingcap/tiflow/cdc/redo/writer"
	"github.com/pingcap/tiflow/pkg/chann"
	"github.com/pingcap/tiflow/pkg/config"
	"github.com/pingcap/tiflow/pkg/redo"
	"github.com/pingcap/tiflow/pkg/spanz"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestConsistentConfig(t *testing.T) {
	t.Parallel()
	levelCases := []struct {
		level string
		valid bool
	}{
		{"none", true},
		{"eventual", true},
		{"NONE", false},
		{"", false},
	}
	for _, lc := range levelCases {
		require.Equal(t, lc.valid, redo.IsValidConsistentLevel(lc.level))
	}

	levelEnableCases := []struct {
		level      string
		consistent bool
	}{
		{"invalid-level", false},
		{"none", false},
		{"eventual", true},
	}
	for _, lc := range levelEnableCases {
		require.Equal(t, lc.consistent, redo.IsConsistentEnabled(lc.level))
	}

	storageCases := []struct {
		storage string
		valid   bool
	}{
		{"local", true},
		{"nfs", true},
		{"s3", true},
		{"blackhole", true},
		{"Local", false},
		{"", false},
	}
	for _, sc := range storageCases {
		require.Equal(t, sc.valid, redo.IsValidConsistentStorage(sc.storage))
	}

	s3StorageCases := []struct {
		storage   string
		s3Enabled bool
	}{
		{"local", false},
		{"nfs", false},
		{"s3", true},
		{"blackhole", false},
	}
	for _, sc := range s3StorageCases {
		require.Equal(t, sc.s3Enabled, redo.IsExternalStorage(sc.storage))
	}
}

// TestLogManagerInProcessor tests how redo log manager is used in processor,
// where the redo log manager needs to handle DMLs and redo log meta data
func TestLogManagerInProcessor(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logMgr, err := NewMockManager(ctx)
	require.Nil(t, err)
	defer logMgr.Cleanup(ctx)

	checkResolvedTs := func(mgr LogManager, expectedRts uint64) {
		time.Sleep(time.Duration(config.DefaultFlushIntervalInMs+200) * time.Millisecond)
		resolvedTs := mgr.GetMinResolvedTs()
		require.Equal(t, expectedRts, resolvedTs)
	}

	// check emit row changed events can move forward resolved ts
	spans := []tablepb.Span{
		spanz.TableIDToComparableSpan(53),
		spanz.TableIDToComparableSpan(55),
		spanz.TableIDToComparableSpan(57),
		spanz.TableIDToComparableSpan(59),
	}

	startTs := uint64(100)
	for _, span := range spans {
		logMgr.AddTable(span, startTs)
	}
	testCases := []struct {
		span tablepb.Span
		rows []*model.RowChangedEvent
	}{
		{
			span: spanz.TableIDToComparableSpan(53),
			rows: []*model.RowChangedEvent{
				{CommitTs: 120, Table: &model.TableName{TableID: 53}},
				{CommitTs: 125, Table: &model.TableName{TableID: 53}},
				{CommitTs: 130, Table: &model.TableName{TableID: 53}},
			},
		},
		{
			span: spanz.TableIDToComparableSpan(55),
			rows: []*model.RowChangedEvent{
				{CommitTs: 130, Table: &model.TableName{TableID: 55}},
				{CommitTs: 135, Table: &model.TableName{TableID: 55}},
			},
		},
		{
			span: spanz.TableIDToComparableSpan(57),
			rows: []*model.RowChangedEvent{
				{CommitTs: 130, Table: &model.TableName{TableID: 57}},
			},
		},
		{
			span: spanz.TableIDToComparableSpan(59),
			rows: []*model.RowChangedEvent{
				{CommitTs: 128, Table: &model.TableName{TableID: 59}},
				{CommitTs: 130, Table: &model.TableName{TableID: 59}},
				{CommitTs: 133, Table: &model.TableName{TableID: 59}},
			},
		},
	}
	for _, tc := range testCases {
		err := logMgr.EmitRowChangedEvents(ctx, tc.span, nil, tc.rows...)
		require.Nil(t, err)
	}

	// check UpdateResolvedTs can move forward the resolved ts when there is not row event.
	flushResolvedTs := uint64(150)
	for _, span := range spans {
		err := logMgr.UpdateResolvedTs(ctx, span, flushResolvedTs)
		require.Nil(t, err)
	}
	checkResolvedTs(logMgr, flushResolvedTs)

	// check remove table can work normally
	removeTable := spans[len(spans)-1]
	spans = spans[:len(spans)-1]
	logMgr.RemoveTable(removeTable)
	flushResolvedTs = uint64(200)
	for _, span := range spans {
		err := logMgr.UpdateResolvedTs(ctx, span, flushResolvedTs)
		require.Nil(t, err)
	}
	checkResolvedTs(logMgr, flushResolvedTs)
}

// TestLogManagerInOwner tests how redo log manager is used in owner,
// where the redo log manager needs to handle DDL event only.
func TestLogManagerInOwner(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logMgr, err := NewMockManager(ctx)
	require.Nil(t, err)
	defer logMgr.Cleanup(ctx)

	ddl := &model.DDLEvent{StartTs: 100, CommitTs: 120, Query: "CREATE TABLE `TEST.T1`"}
	err = logMgr.EmitDDLEvent(ctx, ddl)
	require.Nil(t, err)

	err = logMgr.writer.DeleteAllLogs(ctx)
	require.Nil(t, err)
}

func BenchmarkRedoManager(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runBenchTest(ctx, b)
}

func BenchmarkRedoManagerWaitFlush(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logMgr, maxTsMap := runBenchTest(ctx, b)

	var minResolvedTs model.Ts = math.MaxUint64
	maxTsMap.Range(func(span tablepb.Span, tp *uint64) bool {
		if *tp < minResolvedTs {
			minResolvedTs = *tp
		}
		return true
	})

	for t := logMgr.GetMinResolvedTs(); t != minResolvedTs; {
		time.Sleep(time.Millisecond * 200)
		log.Debug("", zap.Uint64("targetTs", minResolvedTs), zap.Uint64("minResolvedTs", t))
		t = logMgr.GetMinResolvedTs()
	}
}

func runBenchTest(
	ctx context.Context, b *testing.B,
) (LogManager, *spanz.HashMap[*model.Ts]) {
	logMgr, err := NewMockManager(ctx)
	require.Nil(b, err)

	// Init tables
	numOfTables := 200
	tables := make([]model.TableID, 0, numOfTables)
	maxTsMap := spanz.NewHashMap[*model.Ts]()
	startTs := uint64(100)
	for i := 0; i < numOfTables; i++ {
		tableID := model.TableID(i)
		tables = append(tables, tableID)
		span := spanz.TableIDToComparableSpan(tableID)
		ts := startTs
		maxTsMap.ReplaceOrInsert(span, &ts)
		logMgr.AddTable(span, startTs)
	}

	maxRowCount := 100000
	wg := sync.WaitGroup{}
	b.ResetTimer()
	for _, tableID := range tables {
		wg.Add(1)
		go func(span tablepb.Span) {
			defer wg.Done()
			maxCommitTs := maxTsMap.GetV(span)
			rows := []*model.RowChangedEvent{}
			for i := 0; i < maxRowCount; i++ {
				if i%100 == 0 {
					logMgr.UpdateResolvedTs(ctx, span, *maxCommitTs)
					// prepare new row change events
					b.StopTimer()
					*maxCommitTs += rand.Uint64() % 10
					rows = []*model.RowChangedEvent{
						{CommitTs: *maxCommitTs, Table: &model.TableName{TableID: span.TableID}},
						{CommitTs: *maxCommitTs, Table: &model.TableName{TableID: span.TableID}},
						{CommitTs: *maxCommitTs, Table: &model.TableName{TableID: span.TableID}},
					}

					b.StartTimer()
				}
				logMgr.EmitRowChangedEvents(ctx, span, nil, rows...)
			}
		}(spanz.TableIDToComparableSpan(tableID))
	}

	wg.Wait()
	return logMgr, maxTsMap
}

// TestManagerRtsMap tests whether Manager's internal rtsMap is managed correctly.
func TestManagerRtsMap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logMgr, err := NewMockManager(ctx)
	require.Nil(t, err)
	defer logMgr.Cleanup(ctx)

	var tables *spanz.HashMap[model.Ts]
	var minTs model.Ts

	tables, minTs = logMgr.prepareForFlush()
	require.Equal(t, 0, tables.Len())
	require.Equal(t, uint64(0), minTs)
	logMgr.postFlush(tables, minTs)
	require.Equal(t, uint64(math.MaxInt64), logMgr.GetMinResolvedTs())

	span1 := spanz.TableIDToComparableSpan(1)
	span2 := spanz.TableIDToComparableSpan(2)
	// Add a table.
	logMgr.AddTable(span1, model.Ts(10))
	logMgr.AddTable(span2, model.Ts(20))
	tables, minTs = logMgr.prepareForFlush()
	require.Equal(t, 2, tables.Len())
	require.Equal(t, uint64(10), minTs)
	logMgr.postFlush(tables, minTs)
	require.Equal(t, uint64(10), logMgr.GetMinResolvedTs())

	// Remove a table.
	logMgr.RemoveTable(span1)
	require.Equal(t, uint64(20), logMgr.GetMinResolvedTs())

	// Add the table back, GetMinResolvedTs can regress.
	logMgr.AddTable(span1, model.Ts(10))
	require.Equal(t, uint64(10), logMgr.GetMinResolvedTs())

	// Received some timestamps, some tables may not be updated.
	logMgr.onResolvedTsMsg(span1, model.Ts(30))
	tables, minTs = logMgr.prepareForFlush()
	require.Equal(t, 2, tables.Len())
	require.Equal(t, uint64(20), minTs)
	logMgr.postFlush(tables, minTs)
	require.Equal(t, uint64(20), logMgr.GetMinResolvedTs())

	// Remove all tables.
	logMgr.RemoveTable(span1)
	logMgr.RemoveTable(span2)
	require.Equal(t, uint64(math.MaxInt64), logMgr.GetMinResolvedTs())
}

// TestManagerError tests whether internal error in bgUpdateLog could be managed correctly.
func TestManagerError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	cfg := &config.ConsistentConfig{
		Level:             string(redo.ConsistentLevelEventual),
		Storage:           "blackhole://",
		FlushIntervalInMs: config.DefaultFlushIntervalInMs,
	}

	errCh := make(chan error, 1)
	opts := newMockManagerOptions(errCh)
	opts.EnableBgRunner = false
	opts.EnableGCRunner = false
	logMgr, err := NewManager(ctx, cfg, opts)
	require.Nil(t, err)
	logMgr.writer = writer.NewInvalidBlackHoleWriter(logMgr.writer)
	logMgr.logBuffer = chann.NewAutoDrainChann[cacheEvents]()
	go logMgr.bgUpdateLog(ctx, cfg.FlushIntervalInMs, errCh)

	testCases := []struct {
		span tablepb.Span
		rows []*model.RowChangedEvent
	}{
		{
			span: spanz.TableIDToComparableSpan(53),
			rows: []*model.RowChangedEvent{
				{CommitTs: 120, Table: &model.TableName{TableID: 53}},
				{CommitTs: 125, Table: &model.TableName{TableID: 53}},
				{CommitTs: 130, Table: &model.TableName{TableID: 53}},
			},
		},
	}
	for _, tc := range testCases {
		err := logMgr.EmitRowChangedEvents(ctx, tc.span, nil, tc.rows...)
		require.Nil(t, err)
	}

	// bgUpdateLog exists because of writer.WriteLog failure.
	select {
	case <-ctx.Done():
		t.Fatal("bgUpdateLog should return error before context is done")
	case err := <-errCh:
		require.Regexp(t, ".*invalid black hole writer.*", err)
		require.Regexp(t, ".*WriteLog.*", err)
	}

	logMgr, err = NewManager(ctx, cfg, opts)
	require.Nil(t, err)
	logMgr.writer = writer.NewInvalidBlackHoleWriter(logMgr.writer)
	logMgr.logBuffer = chann.NewAutoDrainChann[cacheEvents]()
	go logMgr.bgUpdateLog(ctx, cfg.FlushIntervalInMs, errCh)

	// bgUpdateLog exists because of writer.FlushLog failure.
	select {
	case <-ctx.Done():
		t.Fatal("bgUpdateLog should return error before context is done")
	case err := <-errCh:
		require.Regexp(t, ".*invalid black hole writer.*", err)
		require.Regexp(t, ".*FlushLog.*", err)
	}
}

func TestReuseWritter(t *testing.T) {
	ctxs := make([]context.Context, 0, 2)
	cancels := make([]func(), 0, 2)
	mgrs := make([]*ManagerImpl, 0, 2)

	dir := t.TempDir()
	cfg := &config.ConsistentConfig{
		Level:             string(redo.ConsistentLevelEventual),
		Storage:           "local://" + dir,
		FlushIntervalInMs: config.DefaultFlushIntervalInMs,
	}

	errCh := make(chan error, 1)
	opts := newMockManagerOptions(errCh)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ctx = contextutil.PutChangefeedIDInCtx(ctx, model.ChangeFeedID{
			Namespace: "default", ID: "test-reuse-writter",
		})
		mgr, err := NewManager(ctx, cfg, opts)
		require.Nil(t, err)

		ctxs = append(ctxs, ctx)
		cancels = append(cancels, cancel)
		mgrs = append(mgrs, mgr)
	}

	// Cancel one redo manager and wait for a while.
	cancels[0]()
	time.Sleep(time.Duration(100) * time.Millisecond)

	// The another redo manager shouldn't be influenced.
	var workTimeSlice time.Duration
	mgrs[1].flushLog(ctxs[1], func(err error) { opts.ErrCh <- err }, &workTimeSlice)
	select {
	case x := <-errCh:
		log.Panic("shouldn't get an error", zap.Error(x))
	case <-time.NewTicker(time.Duration(100) * time.Millisecond).C:
	}

	// After the manager is closed, APIs can return errors instead of panic.
	cancels[1]()
	time.Sleep(time.Duration(100) * time.Millisecond)
	span := spanz.TableIDToComparableSpan(1)
	err := mgrs[1].UpdateResolvedTs(context.Background(), span, 1)
	require.Error(t, err)
}
