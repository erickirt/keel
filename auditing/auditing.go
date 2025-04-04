package auditing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/karlseguin/typed"
	"github.com/teamkeel/keel/casing"
	"github.com/teamkeel/keel/db"
	"github.com/teamkeel/keel/proto"
)

// Audit operations
const (
	Insert = "insert"
	Update = "update"
	Delete = "delete"
)

// Audit table name
const TableName = "keel_audit"

// Audit table column names
const (
	ColumnId               = "id"
	ColumnTableName        = "table_name"
	ColumnOp               = "op"
	ColumnData             = "data"
	ColumnIdentityId       = "identity_id"
	ColumnTraceId          = "trace_id"
	ColumnCreatedAt        = "created_at"
	ColumnEventProcessedAt = "event_processed_at"
)

type AuditLog struct {
	Id               string
	TableName        string
	Op               string
	Data             map[string]any
	CreatedAt        time.Time
	EventProcessedAt time.Time
}

// Previous returns the previous log entry for the given data row.
func Previous(ctx context.Context, log *AuditLog) (*AuditLog, error) {
	database, err := db.GetDatabase(ctx)
	if err != nil {
		return nil, err
	}

	sql := fmt.Sprintf("SELECT * FROM %s WHERE table_name = ? AND data->>'id' = ? and created_at < ? ORDER BY created_at desc LIMIT 1", TableName)

	result, err := database.ExecuteQuery(ctx, sql, log.TableName, log.Data["id"], log.CreatedAt)
	if err != nil {
		return nil, err
	}

	if len(result.Rows) != 1 {
		return nil, nil
	}

	return fromRow(result.Rows[0])
}

// ManyPrevious returns the previous log entries for the given data rows.
func ManyPrevious(ctx context.Context, logs []*AuditLog) ([]*AuditLog, error) {
	if len(logs) == 0 {
		return []*AuditLog{}, nil
	}

	database, err := db.GetDatabase(ctx)
	if err != nil {
		return nil, err
	}

	placeholders := make([]string, len(logs))
	args := make([]any, len(logs)*3)

	for i, log := range logs {
		placeholders[i] = "(table_name = ? AND data->>'id' = ? AND created_at < ?)"
		args[i*3] = log.TableName
		args[i*3+1] = log.Data["id"]
		args[i*3+2] = log.CreatedAt
	}

	sql := fmt.Sprintf(`
		SELECT a.* FROM %s a
		WHERE (table_name, data->>'id', created_at) IN (
			SELECT table_name, data->>'id', MAX(created_at)
			FROM %s
			WHERE %s	
			GROUP BY table_name, data->>'id'
		)
	`, TableName, TableName, strings.Join(placeholders, " OR "))

	result, err := database.ExecuteQuery(ctx, sql, args...)
	if err != nil {
		return nil, err
	}

	res := make([]*AuditLog, len(logs))
	for i, row := range result.Rows {
		log, err := fromRow(row)
		if err != nil {
			return nil, err
		}
		res[i] = log
	}

	return res, nil
}

// ProcessEventsFromAuditTrail inspects the audit table for logs which need to be
// turned into events, updates their event_processed_at column, and then returns them.
func ProcessEventsFromAuditTrail(ctx context.Context, schema *proto.Schema, traceId string) ([]*AuditLog, error) {
	database, err := db.GetDatabase(ctx)
	if err != nil {
		return nil, err
	}

	sql, args, err := processEventsSql(schema, traceId)
	if err != nil {
		return nil, err
	}

	result, err := database.ExecuteQuery(ctx, sql, args...)
	if err != nil {
		return nil, err
	}

	auditLogs := []*AuditLog{}
	for _, row := range result.Rows {
		log, err := fromRow(row)
		if err != nil {
			return nil, err
		}
		auditLogs = append(auditLogs, log)
	}

	return auditLogs, nil
}

// fromRow parses an audit log table row as map[string]any to a AuditLog struct
func fromRow(row map[string]any) (*AuditLog, error) {
	audit := typed.New(row)

	id := audit.String(ColumnId)
	if id == "" {
		return nil, fmt.Errorf("audit '%s' column cannot be parsed or is empty", ColumnId)
	}

	tableName := audit.String(ColumnTableName)
	if tableName == "" {
		return nil, fmt.Errorf("audit '%s' column cannot be parsed or is empty", ColumnTableName)
	}

	op := audit.String(ColumnOp)
	if op == "" {
		return nil, fmt.Errorf("audit '%s' column cannot be parsed or is empty", ColumnOp)
	}

	data, err := typed.JsonString(audit.String(ColumnData))
	if err != nil {
		return nil, err
	}

	createdAt, ok := audit.TimeIf(ColumnCreatedAt)
	if !ok {
		return nil, fmt.Errorf("audit '%s' column cannot be parsed or is empty", ColumnCreatedAt)
	}

	return &AuditLog{
		Id:        id,
		TableName: tableName,
		Op:        op,
		Data:      data,
		CreatedAt: createdAt,
	}, nil
}

// processEventsSql generates SQL which updates and returns the relevant audit log
// entries which are to be turned into events.
func processEventsSql(schema *proto.Schema, traceId string) (string, []any, error) {
	if traceId == "" {
		return "", nil, errors.New("traceId cannot be empty")
	}

	if len(schema.Events) == 0 {
		return "", nil, errors.New("there are no events defined in this schema")
	}

	args := []any{}

	conditions := []string{}
	for _, e := range schema.Events {
		table := casing.ToSnake(e.ModelName)
		op, err := opFromActionType(e.ActionType)
		if err != nil {
			return "", nil, err
		}

		conditions = append(conditions, fmt.Sprintf("(%s = ? AND %s = ?)", ColumnTableName, ColumnOp))
		args = append(args, table, op)
	}

	filter := strings.Join(conditions, " OR ")
	if len(conditions) > 1 {
		filter = fmt.Sprintf("(%s)", filter)
	}

	sql := fmt.Sprintf(
		"UPDATE %s SET %s = now() WHERE %s = ? AND %s IS NULL AND %s RETURNING *",
		TableName, ColumnEventProcessedAt, ColumnTraceId, ColumnEventProcessedAt, filter)

	args = append([]any{traceId}, args...)

	return sql, args, nil
}

// opFromActionType gets the audit operation for a specific action type.
func opFromActionType(action proto.ActionType) (string, error) {
	switch action {
	case proto.ActionType_ACTION_TYPE_CREATE:
		return Insert, nil
	case proto.ActionType_ACTION_TYPE_UPDATE:
		return Update, nil
	case proto.ActionType_ACTION_TYPE_DELETE:
		return Delete, nil
	default:
		return "", fmt.Errorf("unsupported action type '%s' when creating event", action)
	}
}
