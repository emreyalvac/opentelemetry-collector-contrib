// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cassandraexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/cassandraexporter"

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gocql/gocql"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/plog"
	conventions "go.opentelemetry.io/collector/semconv/v1.6.1"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal/traceutil"
)

type logsExporter struct {
	client *gocql.Session
	logger *zap.Logger
	cfg    *Config
}

func newLogsExporter(logger *zap.Logger, cfg *Config) (*logsExporter, error) {
	cluster := gocql.NewCluster(cfg.DSN)
	session, err := cluster.CreateSession()
	cluster.Keyspace = cfg.Keyspace
	cluster.Consistency = gocql.Quorum

	if err != nil {
		return nil, err
	}

	return &logsExporter{logger: logger, client: session, cfg: cfg}, nil
}

func initializeLogKernel(cfg *Config) error {
	ctx := context.Background()
	cluster := gocql.NewCluster(cfg.DSN)
	cluster.Consistency = gocql.Quorum
	session, err := cluster.CreateSession()
	if err != nil {
		return err
	}

	createDatabaseError := session.Query(parseCreateDatabaseSQL(cfg)).WithContext(ctx).Exec()
	if createDatabaseError != nil {
		return createDatabaseError
	}
	createLogTableError := session.Query(parseCreateLogTableSQL(cfg)).WithContext(ctx).Exec()
	if createLogTableError != nil {
		return createLogTableError
	}

	defer session.Close()

	return nil
}

func (e *logsExporter) Start(ctx context.Context, host component.Host) error {
	initializeErr := initializeLogKernel(e.cfg)
	return initializeErr
}

func (e *logsExporter) Shutdown(_ context.Context) error {
	if e.client != nil {
		e.client.Close()
	}

	return nil
}

func parseCreateLogTableSQL(cfg *Config) string {
	return fmt.Sprintf(createLogTableSQL, cfg.Keyspace, cfg.LogsTable, cfg.Compression.Algorithm)
}

func (e *logsExporter) pushLogsData(ctx context.Context, ld plog.Logs) error {
	start := time.Now()

	var serviceName string
	for i := 0; i < ld.ResourceLogs().Len(); i++ {
		logs := ld.ResourceLogs().At(i)
		res := logs.Resource()
		resAttr := attributesToMap(res.Attributes().AsRaw())
		if v, ok := res.Attributes().Get(conventions.AttributeServiceName); ok {
			serviceName = v.Str()
		}
		for j := 0; j < logs.ScopeLogs().Len(); j++ {
			rs := logs.ScopeLogs().At(j).LogRecords()
			for k := 0; k < rs.Len(); k++ {
				r := rs.At(k)
				logAttr := attributesToMap(r.Attributes().AsRaw())
				bodyByte, _ := json.Marshal(r.Body().AsRaw())

				insertLogError := e.client.Query(fmt.Sprintf(insertLogTableSQL, e.cfg.Keyspace, e.cfg.LogsTable),
					r.Timestamp().AsTime(),
					traceutil.TraceIDToHexOrEmptyString(r.TraceID()),
					traceutil.SpanIDToHexOrEmptyString(r.SpanID()),
					uint32(r.Flags()),
					r.SeverityText(),
					int32(r.SeverityNumber()),
					serviceName,
					string(bodyByte),
					resAttr,
					logAttr,
				).WithContext(ctx).Exec()

				if insertLogError != nil {
					e.logger.Error("insert log error", zap.Error(insertLogError))
				}
			}
		}
	}

	duration := time.Since(start)
	e.logger.Debug("insert logs", zap.Int("records", ld.LogRecordCount()),
		zap.String("cost", duration.String()))
	return nil
}
