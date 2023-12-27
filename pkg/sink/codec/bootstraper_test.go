// Copyright 2022 PingCAP, Inc.
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

package codec

import (
	"context"
	"testing"
	"time"

	timodel "github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/stretchr/testify/require"
)

func getMockTableStatus() (TopicPartitionKey, *model.RowChangedEvent, *tableStatus) {
	tableInfo := &model.TableInfo{
		TableInfo: &timodel.TableInfo{
			UpdateTS: 1,
		},
	}
	table := &model.TableName{
		Schema:  "test",
		Table:   "t1",
		TableID: 1,
	}
	key := TopicPartitionKey{
		Topic:          "test.t1",
		Partition:      1,
		TotalPartition: 3,
	}
	row := &model.RowChangedEvent{
		TableInfo: tableInfo,
		Table:     table,
	}
	// case 1: A new added table should send bootstrap message immediately
	tb1 := newTableStatus(key, row)
	return key, row, tb1
}

func TestShouldSendBootstrapMsg(t *testing.T) {
	t.Parallel()

	_, _, tb1 := getMockTableStatus()

	// case 1: A new added table should send bootstrap message immediately
	require.True(t, tb1.
		shouldSendBootstrapMsg(defaultSendBootstrapInterval, defaultSendBootstrapInMsgCount))

	// case 2: A table which has sent bootstrap message should not send bootstrap message
	tb1.lastSendTime.Store(time.Now())
	require.False(t, tb1.shouldSendBootstrapMsg(defaultSendBootstrapInterval, defaultSendBootstrapInMsgCount))

	// case 3: When the table receive message more than sendBootstrapInMsgCount,
	// it should send bootstrap message
	tb1.counter.Add(int32(defaultSendBootstrapInMsgCount))
	require.True(t, tb1.shouldSendBootstrapMsg(defaultSendBootstrapInterval, defaultSendBootstrapInMsgCount))

	// case 4: When the table does not send bootstrap message for a sendBootstrapInterval time,
	// it should send bootstrap message
	tb1.lastSendTime.Store(time.Now().Add(-defaultSendBootstrapInterval))
	require.True(t, tb1.shouldSendBootstrapMsg(defaultSendBootstrapInterval, defaultSendBootstrapInMsgCount))
}

func TestIsActive(t *testing.T) {
	t.Parallel()
	key, row, tb1 := getMockTableStatus()
	// case 1: A new added table should be active
	require.True(t, tb1.isActive(defaultMaxInactiveDuration))

	// case 2: A table which does not receive message for a long time should be inactive
	tb1.lastMsgReceivedTime.Store(time.Now().Add(-defaultMaxInactiveDuration))
	require.False(t, tb1.isActive(defaultMaxInactiveDuration))

	// case 3: A table which receive message recently should be active
	// Note: A table's update method will be call any time it receive message
	// So use update method to simulate the table receive message
	tb1.update(key, row)
	require.True(t, tb1.isActive(defaultMaxInactiveDuration))
}

func TestBootstrapWorker(t *testing.T) {
	t.Parallel()
	// new builder
	builder := &MockRowEventEncoderBuilder{}

	outCh := make(chan *future, defaultInputChanSize)
	worker := newBootstrapWorker(outCh,
		builder,
		defaultSendBootstrapInterval,
		defaultSendBootstrapInMsgCount,
		defaultMaxInactiveDuration)

	// Start the worker in a separate goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = worker.run(ctx)
	}()

	// case 1: A new added table should send bootstrap message immediately
	// The messages number should be equal to the total partition number
	// Event if we send the same table twice, it should only send bootstrap message once
	key, row, _ := getMockTableStatus()
	err := worker.addEvent(ctx, key, row)
	require.NoError(t, err)
	err = worker.addEvent(ctx, key, row)
	require.NoError(t, err)
	var msgCount int32
	sctx, sancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer sancel()
	for {
		select {
		case future := <-outCh:
			require.NotNil(t, future)
			require.Equal(t, key.Topic, future.Key.Topic)
			require.Equal(t, key.TotalPartition, future.Key.TotalPartition)
			msgCount++
		case <-sctx.Done():
			require.Equal(t, key.TotalPartition, msgCount)
			return
		}
	}
}
