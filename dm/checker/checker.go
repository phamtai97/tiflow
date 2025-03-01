// Copyright 2019 PingCAP, Inc.
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

package checker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/tiflow/dm/dm/config"
	"github.com/pingcap/tiflow/dm/dm/pb"
	"github.com/pingcap/tiflow/dm/dm/unit"
	"github.com/pingcap/tiflow/dm/pkg/binlog"
	"github.com/pingcap/tiflow/dm/pkg/checker"
	"github.com/pingcap/tiflow/dm/pkg/conn"
	tcontext "github.com/pingcap/tiflow/dm/pkg/context"
	"github.com/pingcap/tiflow/dm/pkg/dumpling"
	fr "github.com/pingcap/tiflow/dm/pkg/func-rollback"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/pingcap/tiflow/dm/pkg/terror"
	"github.com/pingcap/tiflow/dm/pkg/utils"
	onlineddl "github.com/pingcap/tiflow/dm/syncer/online-ddl-tools"

	_ "github.com/go-sql-driver/mysql" // for mysql
	column "github.com/pingcap/tidb-tools/pkg/column-mapping"
	"github.com/pingcap/tidb-tools/pkg/dbutil"
	"github.com/pingcap/tidb-tools/pkg/filter"
	router "github.com/pingcap/tidb-tools/pkg/table-router"
	"github.com/pingcap/tidb/dumpling/export"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

const (
	// the total time needed to complete the check depends on the number of instances, databases and tables,
	// now increase the total timeout to 30min, but set `readTimeout` to 30s for source/target DB.
	// if we can not complete the check in 30min, then we must need to refactor the implementation of the function.
	checkTimeout = 30 * time.Minute
	readTimeout  = "30s"
)

type mysqlInstance struct {
	cfg *config.SubTaskConfig

	sourceDB     *conn.BaseDB
	sourceDBinfo *dbutil.DBConfig

	targetDB     *conn.BaseDB
	targetDBInfo *dbutil.DBConfig
}

// Checker performs pre-check of data synchronization.
type Checker struct {
	closed atomic.Bool

	tctx *tcontext.Context

	instances []*mysqlInstance

	checkList     []checker.RealChecker
	checkingItems map[string]string
	result        struct {
		sync.RWMutex
		detail *checker.Results
	}
	errCnt  int64
	warnCnt int64

	onlineDDL onlineddl.OnlinePlugin
}

// NewChecker returns a checker.
func NewChecker(cfgs []*config.SubTaskConfig, checkingItems map[string]string, errCnt, warnCnt int64) *Checker {
	c := &Checker{
		instances:     make([]*mysqlInstance, 0, len(cfgs)),
		checkingItems: checkingItems,

		errCnt:  errCnt,
		warnCnt: warnCnt,
	}

	for _, cfg := range cfgs {
		// we have verify it in SubTaskConfig.Adjust
		replica, _ := cfg.DecryptPassword()
		c.instances = append(c.instances, &mysqlInstance{
			cfg: replica,
		})
	}

	return c
}

// Init implements Unit interface.
func (c *Checker) Init(ctx context.Context) (err error) {
	rollbackHolder := fr.NewRollbackHolder("checker")
	defer func() {
		if err != nil {
			rollbackHolder.RollbackReverseOrder()
		}
	}()

	rollbackHolder.Add(fr.FuncRollback{Name: "close-DBs", Fn: c.closeDBs})

	c.tctx = tcontext.NewContext(ctx, log.With(zap.String("unit", "task check")))
	// targetTableID => source => [tables]
	sharding := make(map[string]map[string][]*filter.Table)
	shardingCounter := make(map[string]int)
	// sourceID => []table
	checkTablesMap := make(map[string][]*filter.Table)
	dbs := make(map[string]*sql.DB)
	columnMapping := make(map[string]*column.Mapping)
	_, checkingShardID := c.checkingItems[config.ShardAutoIncrementIDChecking]
	_, checkingShard := c.checkingItems[config.ShardTableSchemaChecking]
	_, checkSchema := c.checkingItems[config.TableSchemaChecking]

	for _, instance := range c.instances {
		bw, err := filter.New(instance.cfg.CaseSensitive, instance.cfg.BAList)
		if err != nil {
			return terror.ErrTaskCheckGenBAList.Delegate(err)
		}
		r, err := router.NewTableRouter(instance.cfg.CaseSensitive, instance.cfg.RouteRules)
		if err != nil {
			return terror.ErrTaskCheckGenTableRouter.Delegate(err)
		}

		if instance.cfg.OnlineDDL && c.onlineDDL == nil {
			c.onlineDDL, err = onlineddl.NewRealOnlinePlugin(c.tctx, instance.cfg)
			if err != nil {
				return err
			}
			rollbackHolder.Add(fr.FuncRollback{Name: "close-onlineDDL", Fn: c.closeOnlineDDL})
		}

		columnMapping[instance.cfg.SourceID], err = column.NewMapping(instance.cfg.CaseSensitive, instance.cfg.ColumnMappingRules)
		if err != nil {
			return terror.ErrTaskCheckGenColumnMapping.Delegate(err)
		}

		instance.sourceDBinfo = &dbutil.DBConfig{
			Host:     instance.cfg.From.Host,
			Port:     instance.cfg.From.Port,
			User:     instance.cfg.From.User,
			Password: instance.cfg.From.Password,
		}
		dbCfg := instance.cfg.From
		dbCfg.RawDBCfg = config.DefaultRawDBConfig().SetReadTimeout(readTimeout)
		instance.sourceDB, err = conn.DefaultDBProvider.Apply(&dbCfg)
		if err != nil {
			return terror.WithScope(terror.ErrTaskCheckFailedOpenDB.Delegate(err, instance.cfg.From.User, instance.cfg.From.Host, instance.cfg.From.Port), terror.ScopeUpstream)
		}

		instance.targetDBInfo = &dbutil.DBConfig{
			Host:     instance.cfg.To.Host,
			Port:     instance.cfg.To.Port,
			User:     instance.cfg.To.User,
			Password: instance.cfg.To.Password,
		}
		dbCfg = instance.cfg.To
		dbCfg.RawDBCfg = config.DefaultRawDBConfig().SetReadTimeout(readTimeout)
		instance.targetDB, err = conn.DefaultDBProvider.Apply(&dbCfg)
		if err != nil {
			return terror.WithScope(terror.ErrTaskCheckFailedOpenDB.Delegate(err, instance.cfg.To.User, instance.cfg.To.Host, instance.cfg.To.Port), terror.ScopeDownstream)
		}

		if _, ok := c.checkingItems[config.VersionChecking]; ok {
			c.checkList = append(c.checkList, checker.NewMySQLVersionChecker(instance.sourceDB.DB, instance.sourceDBinfo))
		}
		if _, ok := c.checkingItems[config.ServerIDChecking]; ok {
			c.checkList = append(c.checkList, checker.NewMySQLServerIDChecker(instance.sourceDB.DB, instance.sourceDBinfo))
		}
		if _, ok := c.checkingItems[config.BinlogEnableChecking]; ok {
			c.checkList = append(c.checkList, checker.NewMySQLBinlogEnableChecker(instance.sourceDB.DB, instance.sourceDBinfo))
		}
		if _, ok := c.checkingItems[config.BinlogFormatChecking]; ok {
			c.checkList = append(c.checkList, checker.NewMySQLBinlogFormatChecker(instance.sourceDB.DB, instance.sourceDBinfo))
		}
		if _, ok := c.checkingItems[config.BinlogRowImageChecking]; ok {
			c.checkList = append(c.checkList, checker.NewMySQLBinlogRowImageChecker(instance.sourceDB.DB, instance.sourceDBinfo))
		}
		if _, ok := c.checkingItems[config.ReplicationPrivilegeChecking]; ok {
			c.checkList = append(c.checkList, checker.NewSourceReplicationPrivilegeChecker(instance.sourceDB.DB, instance.sourceDBinfo))
		}

		mapping, err := utils.FetchTargetDoTables(ctx, instance.sourceDB.DB, bw, r)
		if err != nil {
			return err
		}

		err = sameTableNameDetection(mapping)
		if err != nil {
			return err
		}

		var checkTables []*filter.Table
		checkSchemas := make(map[string]struct{}, len(mapping))
		for targetTableID, tables := range mapping {
			checkTables = append(checkTables, tables...)
			if _, ok := sharding[targetTableID]; !ok {
				sharding[targetTableID] = make(map[string][]*filter.Table)
			}
			sharding[targetTableID][instance.cfg.SourceID] = append(sharding[targetTableID][instance.cfg.SourceID], tables...)
			shardingCounter[targetTableID] += len(tables)
			for _, table := range tables {
				if _, ok := checkSchemas[table.Schema]; !ok {
					checkSchemas[table.Schema] = struct{}{}
				}
			}
		}
		checkTablesMap[instance.cfg.SourceID] = checkTables
		dbs[instance.cfg.SourceID] = instance.sourceDB.DB
		if _, ok := c.checkingItems[config.DumpPrivilegeChecking]; ok {
			exportCfg := export.DefaultConfig()
			err := dumpling.ParseExtraArgs(&c.tctx.Logger, exportCfg, strings.Fields(instance.cfg.ExtraArgs))
			if err != nil {
				return err
			}
			c.checkList = append(c.checkList, checker.NewSourceDumpPrivilegeChecker(instance.sourceDB.DB, instance.sourceDBinfo, checkTables, exportCfg.Consistency))
		}
		if c.onlineDDL != nil {
			c.checkList = append(c.checkList, checker.NewOnlineDDLChecker(instance.sourceDB.DB, checkSchemas, c.onlineDDL, bw))
		}
	}

	dumpThreads := c.instances[0].cfg.MydumperConfig.Threads
	if checkSchema {
		c.checkList = append(c.checkList, checker.NewTablesChecker(dbs, checkTablesMap, dumpThreads))
	}

	if checkingShard {
		for name, shardingSet := range sharding {
			if shardingCounter[name] <= 1 {
				continue
			}

			c.checkList = append(c.checkList, checker.NewShardingTablesChecker(name, dbs, shardingSet, columnMapping, checkingShardID, dumpThreads))
		}
	}

	c.tctx.Logger.Info(c.displayCheckingItems())
	return nil
}

func (c *Checker) displayCheckingItems() string {
	if len(c.checkList) == 0 {
		return "not found any checking items\n"
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "\n************ task %s checking items ************\n", c.instances[0].cfg.Name)
	for _, checkFunc := range c.checkList {
		fmt.Fprintf(&buf, "%s\n", checkFunc.Name())
	}
	fmt.Fprintf(&buf, "************ task %s checking items ************", c.instances[0].cfg.Name)
	return buf.String()
}

// Process implements Unit interface.
func (c *Checker) Process(ctx context.Context, pr chan pb.ProcessResult) {
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	isCanceled := false
	errs := make([]*pb.ProcessError, 0, 1)
	result, err := checker.Do(cctx, c.checkList)
	if err != nil {
		errs = append(errs, unit.NewProcessError(err))
	} else if !result.Summary.Passed {
		errs = append(errs, unit.NewProcessError(errors.New("check was failed, please see detail")))
	}
	warnLeft, errLeft := c.warnCnt, c.errCnt

	// remove success result if not pass
	results := result.Results[:0]
	for _, r := range result.Results {
		if r.State == checker.StateSuccess {
			continue
		}

		// handle results without r.Errors
		if len(r.Errors) == 0 {
			switch r.State {
			case checker.StateWarning:
				if warnLeft == 0 {
					continue
				}
				warnLeft--
				results = append(results, r)
			case checker.StateFailure:
				if errLeft == 0 {
					continue
				}
				errLeft--
				results = append(results, r)
			}
			continue
		}

		subErrors := make([]*checker.Error, 0, len(r.Errors))
		for _, e := range r.Errors {
			switch e.Severity {
			case checker.StateWarning:
				if warnLeft == 0 {
					continue
				}
				warnLeft--
				subErrors = append(subErrors, e)
			case checker.StateFailure:
				if errLeft == 0 {
					continue
				}
				errLeft--
				subErrors = append(subErrors, e)
			}
		}
		// skip display an empty Result
		if len(subErrors) > 0 {
			r.Errors = subErrors
			results = append(results, r)
		}
	}
	result.Results = results

	c.updateInstruction(result)

	select {
	case <-cctx.Done():
		isCanceled = true
	default:
	}

	var rawResult []byte
	if result.Summary.Successful != result.Summary.Total {
		rawResult, err = json.MarshalIndent(result, "\t", "\t")
		if err != nil {
			rawResult = []byte(fmt.Sprintf("marshal error %v", err))
		}
	}

	c.result.Lock()
	c.result.detail = result
	c.result.Unlock()

	pr <- pb.ProcessResult{
		IsCanceled: isCanceled,
		Errors:     errs,
		Detail:     rawResult,
	}
}

// updateInstruction updates the check result's Instruction.
func (c *Checker) updateInstruction(result *checker.Results) {
	for _, r := range result.Results {
		if r.State == checker.StateSuccess {
			continue
		}

		// can't judge by other field, maybe update it later
		if r.Extra == checker.AutoIncrementKeyChecking {
			if strings.HasPrefix(r.Instruction, "please handle it by yourself") {
				r.Instruction += ",  refer to https://docs.pingcap.com/tidb-data-migration/stable/shard-merge-best-practices#handle-conflicts-of-auto-increment-primary-key) for details."
			}
		}
	}
}

// Close implements Unit interface.
func (c *Checker) Close() {
	if c.closed.Load() {
		return
	}

	c.closeDBs()
	c.closeOnlineDDL()

	c.closed.Store(true)
}

func (c *Checker) closeDBs() {
	for _, instance := range c.instances {
		if instance.sourceDB != nil {
			if err := instance.sourceDB.Close(); err != nil {
				c.tctx.Logger.Error("close source db", zap.Stringer("db", instance.sourceDBinfo), log.ShortError(err))
			}
			instance.sourceDB = nil
		}

		if instance.targetDB != nil {
			if err := instance.targetDB.Close(); err != nil {
				c.tctx.Logger.Error("close target db", zap.Stringer("db", instance.targetDBInfo), log.ShortError(err))
			}
			instance.targetDB = nil
		}
	}
}

func (c *Checker) closeOnlineDDL() {
	if c.onlineDDL != nil {
		c.onlineDDL.Close()
		c.onlineDDL = nil
	}
}

// Pause implements Unit interface.
func (c *Checker) Pause() {
	if c.closed.Load() {
		c.tctx.Logger.Warn("try to pause, but already closed")
		return
	}
}

// Resume resumes the paused process.
func (c *Checker) Resume(ctx context.Context, pr chan pb.ProcessResult) {
	if c.closed.Load() {
		c.tctx.Logger.Warn("try to resume, but already closed")
		return
	}

	c.Process(ctx, pr)
}

// Update implements Unit.Update.
func (c *Checker) Update(ctx context.Context, cfg *config.SubTaskConfig) error {
	// not support update configuration now
	return nil
}

// Type implements Unit interface.
func (c *Checker) Type() pb.UnitType {
	return pb.UnitType_Check
}

// IsFreshTask implements Unit.IsFreshTask.
func (c *Checker) IsFreshTask() (bool, error) {
	return true, nil
}

// Status implements Unit interface.
func (c *Checker) Status(_ *binlog.SourceStatus) interface{} {
	c.result.RLock()
	res := c.result.detail
	c.result.RUnlock()

	rawResult, err := json.Marshal(res)
	if err != nil {
		rawResult = []byte(fmt.Sprintf("marshal %+v error %v", res, err))
	}

	return &pb.CheckStatus{
		Passed:     res.Summary.Passed,
		Total:      int32(res.Summary.Total),
		Failed:     int32(res.Summary.Failed),
		Successful: int32(res.Summary.Successful),
		Warning:    int32(res.Summary.Warning),
		Detail:     rawResult,
	}
}

// Error implements Unit interface.
func (c *Checker) Error() interface{} {
	return &pb.CheckError{}
}

func sameTableNameDetection(tables map[string][]*filter.Table) error {
	tableNameSets := make(map[string]string)
	var messages []string

	for name := range tables {
		nameL := strings.ToLower(name)
		if nameO, ok := tableNameSets[nameL]; !ok {
			tableNameSets[nameL] = name
		} else {
			messages = append(messages, fmt.Sprintf("same target table %v vs %s", nameO, name))
		}
	}

	if len(messages) > 0 {
		return terror.ErrTaskCheckSameTableName.Generate(messages)
	}

	return nil
}
