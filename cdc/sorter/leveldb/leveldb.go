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

package leveldb

import (
	"container/list"
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/pingcap/log"
	"github.com/pingcap/tiflow/cdc/sorter/leveldb/message"
	"github.com/pingcap/tiflow/pkg/actor"
	actormsg "github.com/pingcap/tiflow/pkg/actor/message"
	"github.com/pingcap/tiflow/pkg/config"
	"github.com/pingcap/tiflow/pkg/db"
	cerrors "github.com/pingcap/tiflow/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
)

// Queue of IterRequest
type iterQueue struct {
	*list.List
	// TableID set.
	tables map[tableKey]struct{}
}

type iterItem struct {
	key tableKey
	req *message.IterRequest
}

type tableKey struct {
	UID     uint32
	TableID uint64
}

func (q *iterQueue) push(uid uint32, tableID uint64, req *message.IterRequest) {
	key := tableKey{UID: uid, TableID: tableID}
	_, ok := q.tables[key]
	if ok {
		log.Panic("A table should not issue two concurrent iterator requests",
			zap.Uint64("tableID", tableID),
			zap.Uint32("UID", uid),
			zap.Uint64("resolvedTs", req.ResolvedTs))
	}
	q.tables[key] = struct{}{}
	q.List.PushBack(iterItem{req: req, key: key})
}

func (q *iterQueue) pop() (*message.IterRequest, bool) {
	item := q.List.Front()
	if item == nil {
		return nil, false
	}
	q.List.Remove(item)
	req := item.Value.(iterItem)
	delete(q.tables, req.key)
	return req.req, true
}

// DBActor is a db actor, it reads, writes and deletes key value pair in its db.
type DBActor struct {
	id      actor.ID
	db      db.DB
	wb      db.Batch
	wbSize  int
	wbCap   int
	iterSem *semaphore.Weighted
	iterQ   iterQueue

	deleteCount int
	compact     *CompactScheduler

	closedWg *sync.WaitGroup

	metricWriteDuration prometheus.Observer
	metricWriteBytes    prometheus.Observer
}

var _ actor.Actor = (*DBActor)(nil)

// NewDBActor returns a db actor.
func NewDBActor(
	id int, db db.DB, cfg *config.DBConfig, compact *CompactScheduler,
	wg *sync.WaitGroup, captureAddr string,
) (*DBActor, actor.Mailbox, error) {
	idTag := strconv.Itoa(id)
	// Write batch size should be larger than block size to save CPU.
	const writeBatchSizeFactor = 16
	wbSize := cfg.BlockSize * writeBatchSizeFactor
	// Double batch capacity to avoid memory reallocation.
	const writeBatchCapFactor = 2
	wbCap := wbSize * writeBatchCapFactor
	wb := db.Batch(wbCap)
	// IterCount limits the total number of opened iterators to release db
	// resources in time.
	iterSema := semaphore.NewWeighted(int64(cfg.Concurrency))
	mb := actor.NewMailbox(actor.ID(id), cfg.Concurrency)
	wg.Add(1)

	return &DBActor{
		id:      actor.ID(id),
		db:      db,
		wb:      wb,
		iterSem: iterSema,
		iterQ: iterQueue{
			List:   list.New(),
			tables: make(map[tableKey]struct{}),
		},
		wbSize:  wbSize,
		wbCap:   wbCap,
		compact: compact,

		closedWg: wg,

		metricWriteDuration: sorterWriteDurationHistogram.WithLabelValues(captureAddr, idTag),
		metricWriteBytes:    sorterWriteBytesHistogram.WithLabelValues(captureAddr, idTag),
	}, mb, nil
}

func (ldb *DBActor) close(err error) {
	log.Info("db actor quit", zap.Uint64("ID", uint64(ldb.id)), zap.Error(err))
	ldb.closedWg.Done()
}

func (ldb *DBActor) maybeWrite(force bool) error {
	bytes := len(ldb.wb.Repr())
	if bytes >= ldb.wbSize || (force && bytes != 0) {
		startTime := time.Now()
		err := ldb.wb.Commit()
		if err != nil {
			return cerrors.ErrLevelDBSorterError.GenWithStackByArgs(err)
		}
		ldb.metricWriteDuration.Observe(time.Since(startTime).Seconds())
		ldb.metricWriteBytes.Observe(float64(bytes))

		// Reset write batch or reclaim memory if it grows too large.
		if cap(ldb.wb.Repr()) <= ldb.wbCap {
			ldb.wb.Reset()
		} else {
			ldb.wb = ldb.db.Batch(ldb.wbCap)
		}

		// Schedule a compact task when there are too many deletion.
		if ldb.compact.maybeCompact(ldb.id, ldb.deleteCount) {
			// Reset delete key count if schedule compaction successfully.
			ldb.deleteCount = 0
		}
	}
	return nil
}

// Batch acquire iterators for requests in the queue.
func (ldb *DBActor) acquireIterators() {
	for {
		succeed := ldb.iterSem.TryAcquire(1)
		if !succeed {
			break
		}
		req, ok := ldb.iterQ.pop()
		if !ok {
			ldb.iterSem.Release(1)
			break
		}

		iterCh := req.IterCh
		iterRange := req.Range
		iter := ldb.db.Iterator(iterRange[0], iterRange[1])
		iterCh <- &message.LimitedIterator{
			Iterator:   iter,
			Sema:       ldb.iterSem,
			ResolvedTs: req.ResolvedTs,
		}
		close(iterCh)
	}
}

// Poll implements actor.Actor.
// It handles tasks by writing kv, deleting kv and taking iterators.
func (ldb *DBActor) Poll(ctx context.Context, tasks []actormsg.Message) bool {
	select {
	case <-ctx.Done():
		ldb.close(ctx.Err())
		return false
	default:
	}
	requireIter := false
	for i := range tasks {
		var task message.Task
		msg := tasks[i]
		switch msg.Tp {
		case actormsg.TypeTick:
			continue
		case actormsg.TypeSorterTask:
			task = msg.SorterTask
		case actormsg.TypeStop:
			ldb.close(nil)
			return false
		default:
			log.Panic("unexpected message", zap.Any("message", msg))
		}

		for k, v := range task.Events {
			if len(v) != 0 {
				ldb.wb.Put([]byte(k), v)
			} else {
				// Delete the key if value is empty
				ldb.wb.Delete([]byte(k))
				ldb.deleteCount++
			}

			// Do not force write, batching for efficiency.
			if err := ldb.maybeWrite(false); err != nil {
				log.Panic("db error", zap.Error(err))
			}
		}
		if task.IterReq != nil {
			// Append to slice for later batch acquiring iterators.
			ldb.iterQ.push(task.UID, task.TableID, task.IterReq)
			requireIter = true
		}
	}

	// Force write only if there is a task requires an iterator.
	if err := ldb.maybeWrite(requireIter); err != nil {
		log.Panic("db error", zap.Error(err))
	}
	ldb.acquireIterators()

	return true
}
