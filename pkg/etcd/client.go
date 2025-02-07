// Copyright 2020 PingCAP, Inc.
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

package etcd

import (
	"context"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	cerrors "github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/errorutil"
	"github.com/pingcap/tiflow/pkg/retry"
	"github.com/prometheus/client_golang/prometheus"
	"go.etcd.io/etcd/clientv3"
	v3rpc "go.etcd.io/etcd/etcdserver/api/v3rpc/rpctypes"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

// etcd operation names
const (
	EtcdPut    = "Put"
	EtcdGet    = "Get"
	EtcdTxn    = "Txn"
	EtcdDel    = "Del"
	EtcdGrant  = "Grant"
	EtcdRevoke = "Revoke"
)

const (
	backoffBaseDelayInMs = 500
	// in previous/backoff retry pkg, the DefaultMaxInterval = 60 * time.Second
	backoffMaxDelayInMs = 60 * 1000
	// If no msg comes from an etcd watchCh for etcdWatchChTimeoutDuration long,
	// we should cancel the watchCh and request a new watchCh from etcd client
	etcdWatchChTimeoutDuration = 10 * time.Second
	// If no msg comes from an etcd watchCh for etcdRequestProgressDuration long,
	// we should call RequestProgress of etcd client
	etcdRequestProgressDuration = 1 * time.Second
	// etcdWatchChBufferSize is arbitrarily specified, it will be modified in the future
	etcdWatchChBufferSize = 16
	// etcdTxnTimeoutDuration represents the timeout duration for committing a
	// transaction to Etcd
	etcdTxnTimeoutDuration = 30 * time.Second
)

var (
	TxnEmptyCmps    = []clientv3.Cmp{}
	TxnEmptyOpsThen = []clientv3.Op{}
	TxnEmptyOpsElse = []clientv3.Op{}
)

// set to var instead of const for mocking the value to speedup test
var maxTries int64 = 8

// Client is a simple wrapper that adds retry to etcd RPC
type Client struct {
	cli     *clientv3.Client
	metrics map[string]prometheus.Counter
	// clock is for making it easier to mock time-related data structures in unit tests
	clock clock.Clock
}

// Wrap warps a clientv3.Client that provides etcd APIs required by TiCDC.
func Wrap(cli *clientv3.Client, metrics map[string]prometheus.Counter) *Client {
	return &Client{cli: cli, metrics: metrics, clock: clock.New()}
}

// Unwrap returns a clientv3.Client
func (c *Client) Unwrap() *clientv3.Client {
	return c.cli
}

func retryRPC(rpcName string, metric prometheus.Counter, etcdRPC func() error) error {
	// By default, PD etcd sets [3s, 6s) for election timeout.
	// Some rpc could fail due to etcd errors, like "proposal dropped".
	// Retry at least two election timeout to handle the case that two PDs restarted
	// (the first election maybe failed).
	// 16s = \sum_{n=0}^{6} 0.5*1.5^n
	return retry.Do(context.Background(), func() error {
		err := etcdRPC()
		if err != nil && errors.Cause(err) != context.Canceled {
			log.Warn("etcd RPC failed", zap.String("RPC", rpcName), zap.Error(err))
		}
		if metric != nil {
			metric.Inc()
		}
		return err
	}, retry.WithBackoffBaseDelay(backoffBaseDelayInMs), retry.WithBackoffMaxDelay(backoffMaxDelayInMs), retry.WithMaxTries(maxTries), retry.WithIsRetryableErr(isRetryableError(rpcName)))
}

// Put delegates request to clientv3.KV.Put
func (c *Client) Put(ctx context.Context, key, val string, opts ...clientv3.OpOption) (resp *clientv3.PutResponse, err error) {
	err = retryRPC(EtcdPut, c.metrics[EtcdPut], func() error {
		var inErr error
		resp, inErr = c.cli.Put(ctx, key, val, opts...)
		return inErr
	})
	return
}

// Get delegates request to clientv3.KV.Get
func (c *Client) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (resp *clientv3.GetResponse, err error) {
	err = retryRPC(EtcdGet, c.metrics[EtcdGet], func() error {
		var inErr error
		resp, inErr = c.cli.Get(ctx, key, opts...)
		return inErr
	})
	return
}

// Delete delegates request to clientv3.KV.Delete
func (c *Client) Delete(ctx context.Context, key string, opts ...clientv3.OpOption) (resp *clientv3.DeleteResponse, err error) {
	if metric, ok := c.metrics[EtcdDel]; ok {
		metric.Inc()
	}
	// We don't retry on delete operation. It's dangerous.
	return c.cli.Delete(ctx, key, opts...)
}

// Txn delegates request to clientv3.KV.Txn. The error returned can only be a non-retryable error,
// such as context.Canceled, context.DeadlineExceeded, errors.ErrReachMaxTry.
func (c *Client) Txn(ctx context.Context, cmps []clientv3.Cmp, opsThen, opsElse []clientv3.Op) (resp *clientv3.TxnResponse, err error) {
	txnCtx, cancel := context.WithTimeout(ctx, etcdTxnTimeoutDuration)
	defer cancel()
	err = retryRPC(EtcdTxn, c.metrics[EtcdTxn], func() error {
		var inErr error
		resp, inErr = c.cli.Txn(txnCtx).If(cmps...).Then(opsThen...).Else(opsElse...).Commit()
		return inErr
	})
	return
}

// Grant delegates request to clientv3.Lease.Grant
func (c *Client) Grant(ctx context.Context, ttl int64) (resp *clientv3.LeaseGrantResponse, err error) {
	err = retryRPC(EtcdGrant, c.metrics[EtcdGrant], func() error {
		var inErr error
		resp, inErr = c.cli.Grant(ctx, ttl)
		return inErr
	})
	return
}

func isRetryableError(rpcName string) retry.IsRetryable {
	return func(err error) bool {
		if !cerrors.IsRetryableError(err) {
			return false
		}

		switch rpcName {
		case EtcdRevoke:
			if etcdErr, ok := err.(v3rpc.EtcdError); ok && etcdErr.Code() == codes.NotFound {
				// It means the etcd lease is already expired or revoked
				return false
			}
		case EtcdTxn:
			return errorutil.IsRetryableEtcdError(err)
		default:
			// For other types of operation, we retry directly without handling errors
		}

		return true
	}
}

// Revoke delegates request to clientv3.Lease.Revoke
func (c *Client) Revoke(ctx context.Context, id clientv3.LeaseID) (resp *clientv3.LeaseRevokeResponse, err error) {
	err = retryRPC(EtcdRevoke, c.metrics[EtcdRevoke], func() error {
		var inErr error
		resp, inErr = c.cli.Revoke(ctx, id)
		return inErr
	})
	return
}

// TimeToLive delegates request to clientv3.Lease.TimeToLive
func (c *Client) TimeToLive(ctx context.Context, lease clientv3.LeaseID, opts ...clientv3.LeaseOption) (resp *clientv3.LeaseTimeToLiveResponse, err error) {
	err = retryRPC(EtcdRevoke, c.metrics[EtcdRevoke], func() error {
		var inErr error
		resp, inErr = c.cli.TimeToLive(ctx, lease, opts...)
		return inErr
	})
	return
}

// Watch delegates request to clientv3.Watcher.Watch
func (c *Client) Watch(ctx context.Context, key string, role string, opts ...clientv3.OpOption) clientv3.WatchChan {
	watchCh := make(chan clientv3.WatchResponse, etcdWatchChBufferSize)
	go c.WatchWithChan(ctx, watchCh, key, role, opts...)
	return watchCh
}

// WatchWithChan maintains a watchCh and sends all msg from the watchCh to outCh
func (c *Client) WatchWithChan(ctx context.Context, outCh chan<- clientv3.WatchResponse, key string, role string, opts ...clientv3.OpOption) {
	defer func() {
		close(outCh)
		log.Info("WatchWithChan exited", zap.String("role", role))
	}()

	// get initial revision from opts to avoid revision fall back
	lastRevision := getRevisionFromWatchOpts(opts...)

	watchCtx, cancel := context.WithCancel(ctx)
	defer func() {
		// Using closures to handle changes to the cancel function
		cancel()
	}()
	watchCh := c.cli.Watch(watchCtx, key, opts...)

	ticker := c.clock.Ticker(etcdRequestProgressDuration)
	defer ticker.Stop()
	lastReceivedResponseTime := c.clock.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case response := <-watchCh:
			lastReceivedResponseTime = c.clock.Now()
			if response.Err() == nil && !response.IsProgressNotify() {
				lastRevision = response.Header.Revision
			}

		Loop:
			// we must loop here until the response is sent to outCh
			// or otherwise the response will be lost
			for {
				select {
				case <-ctx.Done():
					return
				case outCh <- response: // it may block here
					break Loop
				case <-ticker.C:
					if c.clock.Since(lastReceivedResponseTime) >= etcdWatchChTimeoutDuration {
						log.Warn("etcd client outCh blocking too long, the etcdWorker may be stuck",
							zap.Duration("duration", c.clock.Since(lastReceivedResponseTime)),
							zap.String("role", role))
					}
				}
			}

			ticker.Reset(etcdRequestProgressDuration)
		case <-ticker.C:
			if err := c.RequestProgress(ctx); err != nil {
				log.Warn("failed to request progress for etcd watcher", zap.Error(err))
			}
			if c.clock.Since(lastReceivedResponseTime) >= etcdWatchChTimeoutDuration {
				// cancel the last cancel func to reset it
				log.Warn("etcd client watchCh blocking too long, reset the watchCh",
					zap.Duration("duration", c.clock.Since(lastReceivedResponseTime)),
					zap.Stack("stack"),
					zap.String("role", role))
				cancel()
				watchCtx, cancel = context.WithCancel(ctx)
				// to avoid possible context leak warning from govet
				_ = cancel
				watchCh = c.cli.Watch(watchCtx, key, clientv3.WithPrefix(), clientv3.WithRev(lastRevision))
				// we need to reset lastReceivedResponseTime after reset Watch
				lastReceivedResponseTime = c.clock.Now()
			}
		}
	}
}

// RequestProgress requests a progress notify response be sent in all watch channels.
func (c *Client) RequestProgress(ctx context.Context) error {
	return c.cli.RequestProgress(ctx)
}
