// Copyright 2013 The ql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSES/QL-LICENSE file.

// Copyright 2015 PingCAP, Inc.
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

package session

import (
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hanchuanchuan/inception-core/ast"
	"github.com/hanchuanchuan/inception-core/config"
	"github.com/hanchuanchuan/inception-core/kv"
	"github.com/hanchuanchuan/inception-core/mysql"
	"github.com/hanchuanchuan/inception-core/parser"
	"github.com/hanchuanchuan/inception-core/sessionctx"
	"github.com/hanchuanchuan/inception-core/terror"
	"github.com/hanchuanchuan/inception-core/util"
	"github.com/hanchuanchuan/inception-core/util/chunk"
	"github.com/pingcap/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// type domainMap struct {
// 	// domains map[string]*domain.Domain
// 	mu sync.Mutex
// }

var (
	// domap = &domainMap{
	// 	// domains: map[string]*domain.Domain{},
	// }
	stores = make(map[string]kv.Driver)
	// store.UUID()-> IfBootstrapped
	storeBootstrapped     = make(map[string]bool)
	storeBootstrappedLock sync.Mutex

	// schemaLease is the time for re-updating remote schema.
	// In online DDL, we must wait 2 * SchemaLease time to guarantee
	// all servers get the neweset schema.
	// Default schema lease time is 1 second, you can change it with a proper time,
	// but you must know that too little may cause badly performance degradation.
	// For production, you should set a big schema lease, like 300s+.
	schemaLease = 1 * time.Second

	// statsLease is the time for reload stats table.
	statsLease = 3 * time.Second
)

// Parse parses a query string to raw ast.StmtNode.
func Parse(ctx sessionctx.Context, src string) ([]ast.StmtNode, error) {
	log.Debug("compiling", src)
	charset, collation := ctx.GetSessionVars().GetCharsetInfo()
	p := parser.New()
	p.SetSQLMode(ctx.GetSessionVars().SQLMode)
	stmts, _, err := p.Parse(src, charset, collation)
	if err != nil {
		log.Warnf("compiling %s, error: %v", src, err)
		return nil, errors.Trace(err)
	}
	return stmts, nil
}

// GetHistory get all stmtHistory in current txn. Exported only for test.
func GetHistory(ctx sessionctx.Context) *StmtHistory {
	hist, ok := ctx.GetSessionVars().TxnCtx.Histroy.(*StmtHistory)
	if ok {
		return hist
	}
	hist = new(StmtHistory)
	ctx.GetSessionVars().TxnCtx.Histroy = hist
	return hist
}

// GetRows4Test gets all the rows from a RecordSet, only used for test.
func GetRows4Test(ctx context.Context, sctx sessionctx.Context, rs ast.RecordSet) ([]chunk.Row, error) {
	if rs == nil {
		return nil, nil
	}
	var rows []chunk.Row
	chk := rs.NewChunk()
	for {
		// Since we collect all the rows, we can not reuse the chunk.
		iter := chunk.NewIterator4Chunk(chk)

		err := rs.Next(ctx, chk)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if chk.NumRows() == 0 {
			break
		}

		for row := iter.Begin(); row != iter.End(); row = iter.Next() {
			rows = append(rows, row)
		}
		chk = chunk.Renew(chk, sctx.GetSessionVars().MaxChunkSize)
	}
	return rows, nil
}

// RegisterStore registers a kv storage with unique name and its associated Driver.
func RegisterStore(name string, driver kv.Driver) error {
	name = strings.ToLower(name)

	if _, ok := stores[name]; ok {
		return errors.Errorf("%s is already registered", name)
	}

	stores[name] = driver
	return nil
}

// NewStore creates a kv Storage with path.
//
// The path must be a URL format 'engine://path?params' like the one for
// session.Open() but with the dbname cut off.
// Examples:
//    goleveldb://relative/path
//    boltdb:///absolute/path
//
// The engine should be registered before creating storage.
func NewStore(path string) (kv.Storage, error) {
	return newStoreWithRetry(path, util.DefaultMaxRetries)
}

func newStoreWithRetry(path string, maxRetries int) (kv.Storage, error) {
	storeURL, err := url.Parse(path)
	if err != nil {
		return nil, errors.Trace(err)
	}

	name := strings.ToLower(storeURL.Scheme)
	d, ok := stores[name]
	if !ok {
		return nil, errors.Errorf("invalid uri format, storage %s is not registered", name)
	}

	var s kv.Storage
	err = util.RunWithRetry(maxRetries, util.RetryInterval, func() (bool, error) {
		log.Infof("new store")
		s, err = d.Open(path)
		return kv.IsRetryableError(err), err
	})
	return s, errors.Trace(err)
}

// DialPumpClientWithRetry tries to dial to binlogSocket,
// if any error happens, it will try to re-dial,
// or return this error when timeout.
func DialPumpClientWithRetry(binlogSocket string, maxRetries int, dialerOpt grpc.DialOption) (*grpc.ClientConn, error) {
	var clientCon *grpc.ClientConn
	err := util.RunWithRetry(maxRetries, util.RetryInterval, func() (bool, error) {
		log.Infof("setup binlog client")
		var err error
		tlsConfig, err := config.GetGlobalConfig().Security.ToTLSConfig()
		if err != nil {
			log.Infof("error happen when setting binlog client: %s", errors.ErrorStack(err))
		}

		if tlsConfig != nil {
			clientCon, err = grpc.Dial(binlogSocket, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), dialerOpt)
		} else {
			clientCon, err = grpc.Dial(binlogSocket, grpc.WithInsecure(), dialerOpt)
		}

		if err != nil {
			log.Infof("error happen when setting binlog client: %s", errors.ErrorStack(err))
		}
		return true, errors.Trace(err)
	})
	return clientCon, errors.Trace(err)
}

var queryStmtTable = []string{"explain", "select", "show", "execute", "describe", "desc", "admin"}

func trimSQL(sql string) string {
	// Trim space.
	sql = strings.TrimSpace(sql)
	// Trim leading /*comment*/
	// There may be multiple comments
	for strings.HasPrefix(sql, "/*") {
		i := strings.Index(sql, "*/")
		if i != -1 && i < len(sql)+1 {
			sql = sql[i+2:]
			sql = strings.TrimSpace(sql)
			continue
		}
		break
	}
	// Trim leading '('. For `(select 1);` is also a query.
	return strings.TrimLeft(sql, "( ")
}

// IsQuery checks if a sql statement is a query statement.
func IsQuery(sql string) bool {
	sqlText := strings.ToLower(trimSQL(sql))
	for _, key := range queryStmtTable {
		if strings.HasPrefix(sqlText, key) {
			return true
		}
	}

	return false
}

var (
	errForUpdateCantRetry = terror.ClassSession.New(codeForUpdateCantRetry,
		mysql.MySQLErrName[mysql.ErrForUpdateCantRetry])
)

const (
	codeForUpdateCantRetry terror.ErrCode = mysql.ErrForUpdateCantRetry
)

func init() {
	sessionMySQLErrCodes := map[terror.ErrCode]uint16{
		codeForUpdateCantRetry: mysql.ErrForUpdateCantRetry,
	}
	terror.ErrClassToMySQLCodes[terror.ClassSession] = sessionMySQLErrCodes
}
