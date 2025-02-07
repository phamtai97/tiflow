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

package kv

import (
	"fmt"

	"github.com/pingcap/errors"
	tidbconfig "github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/kv"
	tidbkv "github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/store"
	"github.com/pingcap/tidb/store/driver"
	cerror "github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/flags"
	"github.com/pingcap/tiflow/pkg/security"
)

// GetSnapshotMeta returns tidb meta information
// TODO: Simplify the signature of this function
func GetSnapshotMeta(tiStore tidbkv.Storage, ts uint64) (*meta.Meta, error) {
	snapshot := tiStore.GetSnapshot(tidbkv.NewVersion(ts))
	return meta.NewSnapshotMeta(snapshot), nil
}

// CreateTiStore creates a new tikv storage client
func CreateTiStore(urls string, credential *security.Credential) (kv.Storage, error) {
	urlv, err := flags.NewURLsValue(urls)
	if err != nil {
		return nil, errors.Trace(err)
	}

	// Ignore error if it is already registered.
	_ = store.Register("tikv", driver.TiKVDriver{})

	if credential.CAPath != "" {
		conf := tidbconfig.GetGlobalConfig()
		conf.Security.ClusterSSLCA = credential.CAPath
		conf.Security.ClusterSSLCert = credential.CertPath
		conf.Security.ClusterSSLKey = credential.KeyPath
		tidbconfig.StoreGlobalConfig(conf)
	}

	tiPath := fmt.Sprintf("tikv://%s?disableGC=true", urlv.HostString())
	tiStore, err := store.New(tiPath)
	if err != nil {
		return nil, cerror.WrapError(cerror.ErrNewStore, err)
	}
	return tiStore, nil
}
