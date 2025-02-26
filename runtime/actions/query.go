package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iancoleman/strcase"
	"github.com/samber/lo"
	"github.com/teamkeel/keel/casing"
	"github.com/teamkeel/keel/db"
	"github.com/teamkeel/keel/proto"
	"github.com/teamkeel/keel/runtime/auth"
	"github.com/teamkeel/keel/runtime/common"
	"github.com/teamkeel/keel/runtime/types"
	"github.com/teamkeel/keel/schema/parser"
	"github.com/teamkeel/keel/storage"
	"github.com/teamkeel/keel/timeperiod"
	"go.opentelemetry.io/otel/trace"
)

// Some field on the query builder's model.
func Field(field string) *QueryOperand {
	return &QueryOperand{
		column: casing.ToSnake(field),
	}
}

// The identifier field on the query builder's model.
func IdField() *QueryOperand {
	return &QueryOperand{
		column: casing.ToSnake(parser.FieldNameId),
	}
}

// All fields on the query builder's model.
func AllFields() *QueryOperand {
	return &QueryOperand{
		column: "*",
	}
}

// Some field from the fragments of an expression or input.
func ExpressionField(fragments []string, field string, arrayField bool) *QueryOperand {
	return &QueryOperand{
		table:      casing.ToSnake(strings.Join(fragments, "$")),
		column:     casing.ToSnake(field),
		arrayField: arrayField,
	}
}

// Represents an inline query.
// column refers to the single column to select from the inline query statement.
func InlineQuery(query *QueryBuilder, column *QueryOperand) *QueryOperand {
	return &QueryOperand{
		query:  query,
		table:  column.table,
		column: column.column,
	}
}

// Represents a raw SQL operand.
func Raw(sql string) *QueryOperand {
	return &QueryOperand{raw: sql}
}

// Represents a value operand.
func Value(value any) *QueryOperand {
	return &QueryOperand{value: value}
}

// Represents a null value operand.
func Null() *QueryOperand {
	return &QueryOperand{}
}

func ValueOrNullIfEmpty(value any) *QueryOperand {
	if value == nil || reflect.ValueOf(value).IsZero() {
		return Null()
	}
	return Value(value)
}

type QueryOperand struct {
	query      *QueryBuilder
	raw        string
	table      string
	column     string
	arrayField bool
	value      any
}

// A query builder to be evaluated and injected as an operand.
func (o *QueryOperand) IsInlineQuery() bool {
	return o.query != nil
}

// Raw SQL to be used as a operand.
func (o *QueryOperand) IsRaw() bool {
	return o.raw != ""
}

func (o *QueryOperand) IsField() bool {
	return o.column != "" && o.query == nil
}

func (o *QueryOperand) IsValue() bool {
	return o.value != nil && o.query == nil
}

func (o *QueryOperand) IsTimePeriodValue() bool {
	if !o.IsValue() {
		return false
	}

	switch o.value.(type) {
	case timeperiod.TimePeriod:
		return true
	}

	return false
}

func (o *QueryOperand) IsArrayValue() bool {
	if !o.IsValue() {
		return false
	}

	// Check that it is a slice
	slice := reflect.ValueOf(o.value)
	if slice.Kind() != reflect.Slice || slice.IsNil() {
		return false
	}

	return true
}

func (o *QueryOperand) IsArrayField() bool {
	return o.arrayField
}

func (o *QueryOperand) IsNull() bool {
	return o.query == nil && o.table == "" && o.column == "" && o.value == nil && o.raw == ""
}

// Generates the string operand that will be used in the actual SQL statement
func (o *QueryOperand) toSqlOperandString(query *QueryBuilder) string {
	switch {
	case o.IsValue() && !o.IsArrayValue():
		if o.IsTimePeriodValue() {
			tp, _ := o.value.(timeperiod.TimePeriod)
			sql := "NOW()"
			if tp.IsTimezoneRelative() && query.timezone != "" {
				sql = fmt.Sprintf("(NOW() AT TIME ZONE '%s')", query.timezone)
			}
			if tp.Offset != 0 {
				sql = fmt.Sprintf("%s + INTERVAL '%d %s'", sql, tp.Offset, tp.Period)
			}
			if tp.Complete {
				sql = fmt.Sprintf("DATE_TRUNC('%s', %s)", tp.Period, sql)
			} else {
				sql = fmt.Sprintf("(%s)", sql)
			}
			return sql
		}

		return "?"
	case o.IsValue() && o.IsArrayValue():
		operands := []string{}
		for i := 0; i < reflect.ValueOf(o.value).Len(); i++ {
			operands = append(operands, "?")
		}

		values := o.toSqlArgs()

		var cast string
		if len(values) > 0 {
			switch values[0].(type) {
			case int, int64, int32:
				cast = "::INTEGER[]"
			case float32, float64:
				cast = "::NUMERIC[]"
			case types.Date:
				cast = "::DATE[]"
			case types.Timestamp:
				cast = "::TIMESTAMPTZ[]"
			case bool:
				cast = "::BOOL[]"
			case storage.FileInfo:
				cast = "::JSONB[]"
			case types.Duration:
				cast = "::INTERVAL[]"
			default:
				cast = "::TEXT[]"
			}
		} else {
			return "'{}'"
		}

		return fmt.Sprintf("ARRAY[%s]%s", strings.Join(operands, ", "), cast)
	case o.IsField():
		table := o.table
		// If no model table is specified, then use the base model in the query builder
		if table == "" {
			table = query.table
		}
		return sqlQuote(table, o.column)
	case o.IsNull():
		return "NULL"
	case o.IsRaw():
		return o.raw
	case o.IsInlineQuery():
		return fmt.Sprintf("(%s)", o.query.SelectStatement().SqlTemplate())
	default:
		return ""
	}
}

// Generates the value that will be used as an argument for a SQL template
func (o *QueryOperand) toSqlArgs() []any {
	switch {
	case o.IsTimePeriodValue():
		return nil
	case o.IsValue() && !o.IsArrayValue():
		return []any{o.value}
	case o.IsValue() && o.IsArrayValue():
		// Safely map rhs slice to []any
		slice := reflect.ValueOf(o.value)
		inValues := make([]any, slice.Len())
		for i := 0; i < slice.Len(); i++ {
			inValues[i] = slice.Index(i).Interface()
		}

		return inValues
	case o.IsField(), o.IsNull(), o.IsRaw():
		return []any{}
	case o.IsInlineQuery():
		return o.query.SelectStatement().SqlArgs()
	default:
		return nil
	}
}

// The templated SQL statement and associated values, ready to be executed.
type Statement struct {
	// The model that represents the table.
	model *proto.Model
	// The generated SQL template.
	template string
	// The arguments associated with the generated SQL template.
	args []any
}

func (statement *Statement) SqlTemplate() string {
	return statement.template
}

func (statement *Statement) SqlArgs() []any {
	return statement.args
}

type QueryBuilder struct {
	// The base model this query builder is acting on.
	Model *proto.Model
	// The table name in the database.
	table string
	// The columns and clauses in SELECT.
	selection []string
	// The columns and clause in DISTINCT ON.
	distinctOn []string
	// The join clauses required for the query.
	joins []joinClause
	// The filter fragments used to construct WHERE.
	filters []string
	// The columns and clauses in ORDER BY.
	orderBy []*orderClause
	// The columns and clauses in RETURNING.
	returning []string
	// The value for LIMIT.
	limit *int
	// The columns and clauses in GROUP BY.
	groupBy []string
	// The value for OFFSET.
	offset *int
	// The ordered slice of arguments for the SQL statement template.
	args []any
	// The graph of rows to be written during an INSERT or UPDATE.
	writeValues *Row
	// The type of SQL join to use.
	joinType JoinType
	// The timezone to be used if we're dealing with relative dates (e.g. DATE_TRUNC("day", NOW()))
	timezone string
}

type JoinType string

const (
	JoinTypeInner JoinType = "INNER"
	JoinTypeLeft  JoinType = "LEFT"
)

type JoinOption struct {
	Type JoinType
}

type joinClause struct {
	table     string
	alias     string
	condition string
	joinType  JoinType
}

type orderClause struct {
	field     *QueryOperand
	direction string
}

type Row struct {
	// The schema model which this row represents data for.
	model *proto.Model
	// The target fragments that this row represents in the input.
	target []string
	// The values of the fields to insert.
	values map[string]*QueryOperand
	// Other rows to insert which this row depends on.
	references []*Relationship
	// Other rows to insert which are dependent on this row.
	referencedBy []*Relationship
}

type Relationship struct {
	// The row which is either referenced to or by in a relationship.
	row *Row
	// The foreign key in the relationship.
	foreignKey *proto.Field
}

type QueryBuilderOption func(qb *QueryBuilder)

func WithJoinType(joinType JoinType) QueryBuilderOption {
	return func(qb *QueryBuilder) {
		qb.joinType = joinType
	}
}

// WithTimezone sets the time zone for the query
func WithTimezone(tz string) QueryBuilderOption {
	return func(qb *QueryBuilder) {
		qb.timezone = tz
	}
}

func NewQuery(model *proto.Model, opts ...QueryBuilderOption) *QueryBuilder {
	qb := &QueryBuilder{
		Model:      model,
		table:      casing.ToSnake(model.Name),
		selection:  []string{},
		distinctOn: []string{},
		joins:      []joinClause{},
		filters:    []string{},
		orderBy:    []*orderClause{},
		limit:      nil,
		groupBy:    []string{},
		returning:  []string{},
		args:       []any{},
		writeValues: &Row{
			model:        nil,
			target:       nil,
			values:       map[string]*QueryOperand{},
			referencedBy: []*Relationship{},
			references:   []*Relationship{},
		},
		joinType: JoinTypeLeft,
	}

	if len(opts) > 0 {
		for _, o := range opts {
			o(qb)
		}
	}

	return qb
}

// Creates a copy of the query builder.
func (query *QueryBuilder) Copy() *QueryBuilder {
	return &QueryBuilder{
		Model:      query.Model,
		table:      query.table,
		selection:  copySlice(query.selection),
		distinctOn: copySlice(query.distinctOn),
		joins:      copySlice(query.joins),
		filters:    copySlice(query.filters),
		orderBy:    copySlice(query.orderBy),
		limit:      query.limit,
		returning:  copySlice(query.returning),
		args:       query.args,
	}
}

// Includes a value to be written during an INSERT or UPDATE.
func (query *QueryBuilder) AddWriteValue(operand *QueryOperand, value *QueryOperand) {
	query.writeValues.model = query.Model
	query.writeValues.values[operand.column] = value
}

// Includes values to be written during an INSERT or UPDATE.
func (query *QueryBuilder) AddWriteValues(values map[string]*QueryOperand) {
	query.writeValues.model = query.Model
	for k, v := range values {
		query.AddWriteValue(Field(k), v)
	}
}

// Includes a column in SELECT.
func (query *QueryBuilder) Select(operand *QueryOperand) {
	c := operand.toSqlOperandString(query)

	if !lo.Contains(query.selection, c) {
		query.selection = append(query.selection, c)
	}
}

// Includes an array column in SELECT and unnests it.
func (query *QueryBuilder) SelectUnnested(operand *QueryOperand) {
	c := fmt.Sprintf("unnest(%s)", operand.toSqlOperandString(query))

	if !lo.Contains(query.selection, c) {
		query.selection = append(query.selection, c)
	}
}

// Include a clause in SELECT.
func (query *QueryBuilder) SelectClause(clause string) {
	if !lo.Contains(query.selection, clause) {
		query.selection = append(query.selection, clause)
	}
}

// Include a column in this table in DISTINCT ON.
func (query *QueryBuilder) DistinctOn(operand *QueryOperand) {
	c := operand.toSqlOperandString(query)

	if !lo.Contains(query.distinctOn, c) {
		query.distinctOn = append(query.distinctOn, c)
	}
}

// Include a WHERE condition, ANDed to the existing filters (unless an OR has been specified)
func (query *QueryBuilder) Where(left *QueryOperand, operator ActionOperator, right *QueryOperand) error {
	template, args, err := query.generateConditionTemplate(left, operator, right)
	if err != nil {
		return err
	}

	query.filters = append(query.filters, template)
	query.args = append(query.args, args...)

	return nil
}

// Appends the next condition with a logical AND.
func (query *QueryBuilder) And() {
	query.filters = trimRhsOperators(query.filters)
	if len(query.filters) > 0 {
		query.filters = append(query.filters, "AND")
	}
}

// Appends the next condition with a logical OR.
func (query *QueryBuilder) Or() {
	query.filters = trimRhsOperators(query.filters)
	if len(query.filters) > 0 {
		query.filters = append(query.filters, "OR")
	}
}

// Opens a new conditional scope in the where expression (i.e. open parethesis).
func (query *QueryBuilder) Not() {
	query.filters = append(query.filters, "NOT")
}

// Opens a new conditional scope in the where expression (i.e. open parethesis).
func (query *QueryBuilder) OpenParenthesis() {
	query.filters = append(query.filters, "(")
}

// Closes the current conditional scope in the where expression (i.e. close parethesis).
func (query *QueryBuilder) CloseParenthesis() {
	query.filters = trimRhsOperators(query.filters)
	query.filters = append(query.filters, ")")
}

// Trims an excess OR / AND operators from the rhs side of the filter conditions.
func trimRhsOperators(filters []string) []string {
	return lo.DropRightWhile(filters, func(s string) bool { return s == "OR" || s == "AND" })
}

// Include an JOIN clause.
func (query *QueryBuilder) Join(joinModel string, joinField *QueryOperand, modelField *QueryOperand) {
	join := joinClause{
		table:     sqlQuote(casing.ToSnake(joinModel)),
		alias:     sqlQuote(joinField.table),
		condition: fmt.Sprintf("%s = %s", joinField.toSqlOperandString(query), modelField.toSqlOperandString(query)),
		joinType:  query.joinType,
	}

	if !lo.Contains(query.joins, join) {
		query.joins = append(query.joins, join)
	}
}

// Include a column in ORDER BY.
// If the column already exists, then just update the sort direction.
func (query *QueryBuilder) AppendOrderBy(operand *QueryOperand, direction string) {
	order := &orderClause{field: operand, direction: strings.ToUpper(direction)}

	existing, found := lo.Find(query.orderBy, func(o *orderClause) bool {
		return o.field.column == order.field.column
	})

	if found {
		existing.direction = strings.ToUpper(direction)
	} else {
		query.orderBy = append(query.orderBy, order)
		query.DistinctOn(operand)
	}
}

// Set the LIMIT to a number.
func (query *QueryBuilder) Limit(limit int) {
	query.limit = &limit
}

func (query *QueryBuilder) GroupBy(operand *QueryOperand) {
	c := operand.toSqlOperandString(query)

	if !lo.Contains(query.groupBy, c) {
		query.groupBy = append(query.groupBy, c)
	}
}

// Set the OFFSET to a number.
func (query *QueryBuilder) Offset(offset int) {
	query.offset = &offset
}

// Include a column in RETURNING.
func (query *QueryBuilder) AppendReturning(operand *QueryOperand) {
	c := operand.toSqlOperandString(query)

	if !lo.Contains(query.returning, c) {
		query.returning = append(query.returning, c)
	}
}

func (query *QueryBuilder) SelectFacets(scope *Scope, input map[string]any) error {
	where, ok := input["where"].(map[string]any)
	if !ok {
		where = map[string]any{}
	}

	facetFields := proto.FacetFields(scope.Schema, scope.Action)
	if len(facetFields) == 0 {
		return nil
	}

	var sql string
	selects := []string{}
	ctes := []string{}
	for i, field := range facetFields {
		// Exclude this field from the where clause because
		// when calculating the facet data
		subWhere := map[string]any{}
		for k, v := range where {
			if k != field.Name {
				subWhere[k] = v
			}
		}

		column := strcase.ToSnake(field.Name)

		facetQuery := NewQuery(scope.Model)

		err := facetQuery.applyImplicitFiltersForList(scope, subWhere)
		if err != nil {
			return err
		}

		err = facetQuery.applyExpressionFilters(scope, where)
		if err != nil {
			return err
		}

		var statement *Statement
		var sel string
		switch field.Type.Type {
		case proto.Type_TYPE_DECIMAL, proto.Type_TYPE_INT, proto.Type_TYPE_DURATION:
			sel = fmt.Sprintf(`json_build_object(
				'min', MIN(%s),
				'max', MAX(%s),
				'avg', AVG(%s)
			) AS %s`, sqlQuote(column), sqlQuote(column), sqlQuote(column), sqlQuote(column))
			facetQuery.SelectClause(sel)
			statement = facetQuery.SelectStatement()
		case proto.Type_TYPE_TIMESTAMP, proto.Type_TYPE_DATE, proto.Type_TYPE_DATETIME:
			sel = fmt.Sprintf(`json_build_object(
				'min', MIN(%s),
				'max', MAX(%s)
			) AS %s`, sqlQuote(column), sqlQuote(column), sqlQuote(column))
			facetQuery.SelectClause(sel)
			statement = facetQuery.SelectStatement()
		case proto.Type_TYPE_ID, proto.Type_TYPE_STRING, proto.Type_TYPE_ENUM:
			facetQuery.Select(Field(field.Name))
			facetQuery.SelectClause("COUNT(*) as \"count\"")
			facetQuery.AppendOrderBy(Field(field.Name), "ASC")
			facetQuery.GroupBy(Field(field.Name))
			subStatement := facetQuery.SelectStatement()

			sel = fmt.Sprintf(`SELECT
				jsonb_agg(
					jsonb_build_object(
						'value', %s,
						'count', "count"
					)
				) AS %s
				FROM (
					%s
				)`, sqlQuote(column), sqlQuote(column), subStatement.template)

			statement = &Statement{
				template: sel,
				args:     subStatement.args,
			}
		default:
			return fmt.Errorf("unsupported facet field type: %s", field.Type.Type)
		}

		sql += fmt.Sprintf("%s AS (%s)", sqlQuote(fmt.Sprintf("%s_facets", column)), statement.template)
		if i < len(facetFields)-1 {
			sql += ", "
		} else {
			sql += " "
		}

		ctes = append(ctes, sqlQuote(fmt.Sprintf("%s_facets", column)))

		query.args = append(query.args, statement.args...)

		selects = append(selects, fmt.Sprintf("'%s', %s.%s", column, sqlQuote(fmt.Sprintf("%s_facets", column)), sqlQuote(column)))
	}

	query.SelectClause(fmt.Sprintf("(WITH %s SELECT json_build_object(%s) FROM %s) AS \"_facets\"", sql, strings.Join(selects, ", "), strings.Join(ctes, ", ")))

	return nil
}

// Apply pagination filters to the query.
func (query *QueryBuilder) ApplyPaging(page Page) error {
	// Paging condition is ANDed to any existing conditions
	query.And()

	// Add where condition to implement the page size
	if page.GetLimit() > 0 {
		query.Limit(page.GetLimit())
	}

	// Specify the ORDER BY - but also a "LEAD" extra column to harvest extra data
	// that helps to determine "hasNextPage"
	query.AppendOrderBy(IdField(), "ASC")

	// Select hasNext clause
	orderByClausesAsSql := []string{}
	for _, o := range query.orderBy {
		orderByClausesAsSql = append(orderByClausesAsSql, fmt.Sprintf("%s %s", o.field.toSqlOperandString(query), o.direction))
	}
	hasNext := fmt.Sprintf("CASE WHEN LEAD(%s) OVER (ORDER BY %s) IS NOT NULL THEN true ELSE false END AS hasNext", IdField().toSqlOperandString(query), strings.Join(orderByClausesAsSql, ", "))
	query.SelectClause(hasNext)

	// We add a subquery to the select list that fetches the total count of records
	// matching the constraints specified by the main query without the offset/limit applied
	// This is actually more performant than COUNT(*) OVER() [window function]
	countQuery, args := query.countQuery()
	totalResults := fmt.Sprintf("(%s) AS totalCount", countQuery)
	query.SelectClause(totalResults)

	// Because we are essentially performing the same query again within the subquery, we need to duplicate the query parameters again as they will be used twice in the course of the whole query
	query.args = append(query.args, args...)

	// if we have offset pagination..
	if page.OffsetPagination() {
		query.Offset(page.Offset)
	} else {
		// otherwise default to cursor pagination
		// Add where condition to implement the after/before paging request
		if page.Cursor() != "" {
			err := query.applyCursorFilter(page.Cursor(), page.IsBackwards())
			if err != nil {
				return err
			}
		}

		// if the page has backwards pagination, we will be reversing the order fields. The results will be reversed after retrieval in .ExecuteToMany()
		if page.IsBackwards() {
			for _, ob := range query.orderBy {
				if strings.EqualFold(ob.direction, "ASC") {
					ob.direction = "DESC"
				} else {
					ob.direction = "ASC"
				}
			}
		}
	}

	return nil
}

// Apply forward pagination 'after' cursor filter to the query, or backwards `before` cursor
func (query *QueryBuilder) applyCursorFilter(cursor string, isBackwards bool) error {
	query.And()

	var err error
	if len(query.orderBy) > 1 {
		query.OpenParenthesis()
	}

	// For each column being ordered, we need to filter those which proceed the cursor row.
	for i := 0; i < len(query.orderBy); i++ {
		if i > 0 {
			query.OpenParenthesis()
		}
		for j := 0; j < i; j++ {
			orderClause := query.orderBy[j]

			inline := NewQuery(query.Model)
			inline.Select(orderClause.field)
			err = inline.Where(IdField(), Equals, Value(cursor))
			if err != nil {
				return err
			}

			err = query.Where(orderClause.field, Equals, InlineQuery(inline, orderClause.field))
			if err != nil {
				return err
			}
			query.And()
		}

		orderClause := query.orderBy[i]

		inline := NewQuery(query.Model)
		inline.Select(orderClause.field)
		err = inline.Where(IdField(), Equals, Value(cursor))
		if err != nil {
			return err
		}

		var operator ActionOperator
		switch {
		case strings.EqualFold(orderClause.direction, "ASC") && !isBackwards:
			operator = GreaterThan
		case strings.EqualFold(orderClause.direction, "ASC") && isBackwards:
			operator = LessThan
		case strings.EqualFold(orderClause.direction, "DESC") && !isBackwards:
			operator = LessThan
		case strings.EqualFold(orderClause.direction, "DESC") && isBackwards:
			operator = GreaterThan
		default:
			return errors.New("unknown order by direction")
		}

		err = query.Where(orderClause.field, operator, InlineQuery(inline, orderClause.field))
		if err != nil {
			return err
		}
		if i > 0 {
			query.CloseParenthesis()
		}
		query.Or()
	}

	if len(query.orderBy) > 1 {
		query.CloseParenthesis()
	}
	return nil
}

func (query *QueryBuilder) countQuery() (string, []any) {
	selection := "COUNT("
	joins := ""
	filters := ""

	if len(query.distinctOn) > 0 {
		distinctFields := strings.Join(query.distinctOn, ", ")
		if len(query.distinctOn) > 1 {
			distinctFields = fmt.Sprintf("(%s)", distinctFields)
		}
		selection += fmt.Sprintf("DISTINCT %s", distinctFields)
	} else {
		selection += "*"
	}
	selection += ")"

	if len(query.joins) > 0 {
		for _, j := range query.joins {
			joins += fmt.Sprintf("%s JOIN %s AS %s ON %s ", query.joinType, j.table, j.alias, j.condition)
		}
	}

	conditions := trimRhsOperators(query.filters)
	if len(conditions) > 0 {
		filters = fmt.Sprintf("WHERE %s", strings.Join(conditions, " "))
	}

	// This is a bit gross as we are assuming the previous X args are the ones used in the where condition
	argCount := strings.Count(filters, "?")
	args := query.args[len(query.args)-argCount:]

	sql := fmt.Sprintf("SELECT %s FROM %s %s %s",
		selection,
		sqlQuote(query.table),
		joins,
		filters)

	return sql, args
}

// Generates an executable SELECT statement with the list of arguments.
func (query *QueryBuilder) SelectStatement() *Statement {
	distinctOn := ""
	joins := ""
	filters := ""
	orderBy := ""
	limit := ""
	groupBy := ""
	offset := ""

	if len(query.distinctOn) > 0 {
		distinctOn = fmt.Sprintf("DISTINCT ON(%s)", strings.Join(query.distinctOn, ", "))
	}

	if len(query.selection) == 0 {
		query.Select(AllFields())
	}

	selection := strings.Join(query.selection, ", ")

	if len(query.joins) > 0 {
		for _, j := range query.joins {
			joins += fmt.Sprintf("%s JOIN %s AS %s ON %s ", query.joinType, j.table, j.alias, j.condition)
		}
	}

	conditions := trimRhsOperators(query.filters)
	if len(conditions) > 0 {
		filters = fmt.Sprintf("WHERE %s", strings.Join(conditions, " "))
	}

	if len(query.orderBy) > 0 {
		orderByClausesAsSql := []string{}
		for _, o := range query.orderBy {
			orderByClausesAsSql = append(orderByClausesAsSql, fmt.Sprintf("%s %s", o.field.toSqlOperandString(query), o.direction))
		}

		orderBy = fmt.Sprintf("ORDER BY %s", strings.Join(orderByClausesAsSql, ", "))
	}

	if len(query.groupBy) > 0 {
		groupBy = fmt.Sprintf("GROUP BY %s", strings.Join(query.groupBy, ", "))
	}

	if query.limit != nil {
		limit = "LIMIT ?"
		query.args = append(query.args, *query.limit)
	}

	if query.offset != nil {
		offset = "OFFSET ?"
		query.args = append(query.args, *query.offset)
	}

	sql := fmt.Sprintf("SELECT %s %s FROM %s %s %s %s %s %s %s",
		distinctOn,
		selection,
		sqlQuote(query.table),
		joins,
		filters,
		groupBy,
		orderBy,
		limit,
		offset)

	return &Statement{
		template: cleanSql(sql),
		args:     query.args,
		model:    query.Model,
	}
}

// Generates an executable INSERT statement with the list of arguments.
func (query *QueryBuilder) InsertStatement(ctx context.Context) *Statement {
	ctes := []string{}
	args := []any{}
	ctes, args, alias := query.generateInsertCte(ctes, args, query.writeValues, nil, "")

	selection := []string{"*"}
	if auth.IsAuthenticated(ctx) {
		identity, _ := auth.GetIdentity(ctx)
		selection = append(selection, setIdentityIdClause())
		args = append(args, identity[parser.FieldNameId].(string))
	}

	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		selection = append(selection, setTraceIdClause())
		args = append(args, spanContext.TraceID().String())
	}

	statement := fmt.Sprintf("WITH %s SELECT %s FROM %s",
		strings.Join(ctes, ", "),
		strings.Join(selection, ", "),
		sqlQuote(alias))

	return &Statement{
		model:    query.Model,
		template: cleanSql(statement),
		args:     args,
	}
}

// Recursively generates in common table expression insert query for the write values graph.
func (query *QueryBuilder) generateInsertCte(ctes []string, args []any, row *Row, foreignKey *proto.Field, primaryKeyTableAlias string) ([]string, []any, string) {
	alias := fmt.Sprintf("new_%v_%s", makeAlias(query.writeValues, row), casing.ToSnake(row.model.Name))
	columnNames := []string{}

	// Rows which this row references need to created first, and the primary needs to be extracted (as a SELECT statement from them to insert into this row.
	// The foreign key field for this row is returned, along with the alias of the table needed to extract the primary key from.
	for _, r := range row.references {
		var primaryKeyTable string
		ctes, args, primaryKeyTable = query.generateInsertCte(ctes, args, r.row, nil, alias)

		// For every row that this references, we need to set the foreign key.
		// For example, on the Sale row; customerId = (SELECT id FROM new_customer_1)
		row.values[r.foreignKey.ForeignKeyFieldName.Value] = Raw(fmt.Sprintf("(SELECT \"id\" FROM %s)", sqlQuote(primaryKeyTable)))
	}

	// Does this foreign key of the relationship exist on this row?
	// This means this row exists as a referencedBy row for another.
	// For example, on the SaleItem row; saleId = (SELECT id FROM new_sale_1)
	if foreignKey != nil && row.model.Name == foreignKey.ModelName {
		row.values[foreignKey.ForeignKeyFieldName.Value] = Raw(fmt.Sprintf("(SELECT \"id\" FROM %s)", sqlQuote(primaryKeyTableAlias)))
	}

	// Make iterating through the map with deterministic ordering
	orderedKeys := make([]string, 0, len(row.values))
	for k := range row.values {
		orderedKeys = append(orderedKeys, k)
	}
	sort.Strings(orderedKeys)

	columnValues := []string{}

	// For any inline query operands (such as with backlinks),
	// we want to create the common table expressions first,
	// and ensure we only create the CTE once (as there may be more
	// than once reference by other fields).
	for i, col := range orderedKeys {
		operand := row.values[col]
		if !operand.IsInlineQuery() {
			continue
		}

		cteAlias := fmt.Sprintf("select_%s_%v", operand.query.table, i)
		cteExists := false
		for _, c := range ctes {
			if strings.HasPrefix(c, sqlQuote(cteAlias)) {
				cteExists = true
				break
			}
		}

		if !cteExists {
			cteAliases := []string{}
			for i := range operand.query.selection {
				cteAliases = append(cteAliases, sqlQuote(fmt.Sprintf("column_%v", i)))
			}

			cte := fmt.Sprintf("%s (%s) AS (%s)",
				sqlQuote(cteAlias),
				strings.Join(cteAliases, ", "),
				operand.query.SelectStatement().SqlTemplate())

			ctes = append(ctes, cte)
			args = append(args, operand.query.SelectStatement().SqlArgs()...)
		}
	}

	for i, col := range orderedKeys {
		colName := casing.ToSnake(col)
		columnNames = append(columnNames, sqlQuote(colName))
		operand := row.values[col]

		switch {
		case operand.IsField(), operand.IsValue(), operand.IsNull(), operand.IsRaw():
			sql := operand.toSqlOperandString(query)
			opArgs := operand.toSqlArgs()

			columnValues = append(columnValues, sql)
			args = append(args, opArgs...)
		case operand.IsInlineQuery():
			cteAlias := fmt.Sprintf("select_%s_%v", operand.query.table, i)
			columnAlias := ""

			for i, s := range operand.query.selection {
				if s == sqlQuote(operand.table, operand.column) {
					columnAlias = fmt.Sprintf("column_%v", i)
					break
				}
			}

			sql := fmt.Sprintf("(SELECT %s FROM %s)", sqlQuote(columnAlias), sqlQuote(cteAlias))
			columnValues = append(columnValues, sql)
		default:
			panic("no handling for rhs QueryOperand type")
		}
	}

	// If there are no values to insert then we use "DEFAULT VALUES" which means:
	// "All columns will be filled with their default values"
	values := "DEFAULT VALUES"

	if len(columnNames) > 0 {
		values = fmt.Sprintf("(%s) VALUES (%s)",
			strings.Join(columnNames, ", "),
			strings.Join(columnValues, ", "))
	}

	cte := fmt.Sprintf("%s AS (INSERT INTO %s %s RETURNING *)",
		sqlQuote(alias),
		sqlQuote(casing.ToSnake(row.model.Name)),
		values)

	ctes = append(ctes, cte)

	// If this row is referenced by other rows, then we need to create these rows afterwards. We need to pass in this row table alias in order to extract the primary key.
	for _, r := range row.referencedBy {
		ctes, args, _ = query.generateInsertCte(ctes, args, r.row, r.foreignKey, alias)
	}

	return ctes, args, alias
}

// Generates a unique alias for this row in the graph.
func makeAlias(graph *Row, row *Row) int {
	rows := orderGraphNodes(graph)

	modelCount := map[string]int{}

	for _, r := range rows {
		modelCount[r.model.Name] += 1

		if r == row {
			return modelCount[r.model.Name]
		}
	}

	panic("the row does not exist within this graph")
}

// Generates an ordered slice of rows by insertion order.
func orderGraphNodes(graph *Row) []*Row {
	rows := []*Row{}

	for _, r := range graph.references {
		g := orderGraphNodes(r.row)
		rows = append(rows, g...)
	}

	rows = append(rows, graph)

	for _, r := range graph.referencedBy {
		g := orderGraphNodes(r.row)
		rows = append(rows, g...)
	}

	return rows
}

// Generates an executable UPDATE statement with the list of arguments.
func (query *QueryBuilder) UpdateStatement(ctx context.Context) *Statement {
	queryFilters := query.filters

	joins := ""
	filters := ""
	returning := ""
	sets := []string{}
	args := []any{}
	ctes := []string{}

	// Make iteratng through the writeValues map deterministically ordered
	orderedKeys := make([]string, 0, len(query.writeValues.values))
	for k := range query.writeValues.values {
		orderedKeys = append(orderedKeys, k)
	}
	sort.Strings(orderedKeys)

	for i, v := range orderedKeys {
		operand := query.writeValues.values[v]
		if !operand.IsInlineQuery() {
			continue
		}

		cteAlias := fmt.Sprintf("select_%s_%v", operand.query.table, i)
		cteExists := false
		for _, c := range ctes {
			if strings.HasPrefix(c, sqlQuote(cteAlias)) {
				cteExists = true
				break
			}
		}

		if !cteExists {
			cteAliases := []string{}
			for i := range operand.query.selection {
				cteAliases = append(cteAliases, sqlQuote(fmt.Sprintf("column_%v", i)))
			}

			cte := fmt.Sprintf("%s (%s) AS (%s)",
				sqlQuote(cteAlias),
				strings.Join(cteAliases, ", "),
				operand.query.SelectStatement().SqlTemplate())

			ctes = append(ctes, cte)
			args = append(args, operand.query.SelectStatement().SqlArgs()...)
		}
	}

	for i, v := range orderedKeys {
		operand := query.writeValues.values[v]

		if operand.IsInlineQuery() {
			cteAlias := fmt.Sprintf("select_%s_%v", operand.query.table, i)
			columnAlias := ""

			for i, s := range operand.query.selection {
				if s == sqlQuote(operand.table, operand.column) {
					columnAlias = fmt.Sprintf("column_%v", i)
					break
				}
			}

			sql := fmt.Sprintf("(SELECT %s FROM %s)", sqlQuote(columnAlias), sqlQuote(cteAlias))
			sets = append(sets, fmt.Sprintf("%s = %s", sqlQuote(casing.ToSnake(v)), sql))
		} else {
			sqlOperand := operand.toSqlOperandString(query)
			sqlArgs := operand.toSqlArgs()

			args = append(args, sqlArgs...)
			sets = append(sets, fmt.Sprintf("%s = %s", sqlQuote(casing.ToSnake(v)), sqlOperand))
		}
	}

	args = append(args, query.args...)

	if len(query.joins) > 0 {
		for _, j := range query.joins {
			joins += fmt.Sprintf("%s JOIN %s AS %s ON %s ", query.joinType, j.table, j.alias, j.condition)
		}
	}

	conditions := trimRhsOperators(queryFilters)
	if len(conditions) > 0 {
		filters = fmt.Sprintf("WHERE %s", strings.Join(conditions, " "))
	}

	if auth.IsAuthenticated(ctx) {
		identity, _ := auth.GetIdentity(ctx)
		query.returning = append(query.returning, setIdentityIdClause())
		args = append(args, identity[parser.FieldNameId].(string))
	}

	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		query.returning = append(query.returning, setTraceIdClause())
		args = append(args, spanContext.TraceID().String())
	}

	if len(query.returning) > 0 {
		returning = fmt.Sprintf("RETURNING %s", strings.Join(query.returning, ", "))
	}

	commonTableExpressions := ""
	if len(ctes) > 0 {
		commonTableExpressions = fmt.Sprintf("WITH %s", strings.Join(ctes, ", "))
	}

	var template string
	if len(query.joins) == 0 {
		template = fmt.Sprintf("%s UPDATE %s SET %s %s %s",
			commonTableExpressions,
			sqlQuote(query.table),
			strings.Join(sets, ", "),
			filters,
			returning)
	} else {
		template = fmt.Sprintf("%s UPDATE %s SET %s WHERE \"id\" = (SELECT %s.\"id\" FROM %s %s %s) %s",
			commonTableExpressions,
			sqlQuote(query.table),
			strings.Join(sets, ", "),
			sqlQuote(query.table),
			sqlQuote(query.table),
			joins,
			filters,
			returning)
	}

	return &Statement{
		template: cleanSql(template),
		args:     args,
		model:    query.Model,
	}
}

// Generates an executable DELETE statement with the list of arguments.
func (query *QueryBuilder) DeleteStatement(ctx context.Context) *Statement {
	usings := ""
	filters := ""
	returning := ""

	if len(query.joins) > 0 {
		usingTables := lo.Map(query.joins, func(j joinClause, _ int) string {
			return fmt.Sprintf("%s AS %s", j.table, j.alias)
		})
		usings = fmt.Sprintf("USING %s", strings.Join(usingTables, ", "))
		filters = strings.Join(lo.Map(query.joins, func(j joinClause, _ int) string { return j.condition }), " AND ")

		// If there are also filters, then append another AND
		if len(query.filters) > 0 {
			filters += " AND "
		}
	}

	conditions := trimRhsOperators(query.filters)
	if len(conditions) > 0 {
		filters = fmt.Sprintf("WHERE %s", strings.Join(conditions, " "))
	}

	if auth.IsAuthenticated(ctx) {
		identity, _ := auth.GetIdentity(ctx)
		query.returning = append(query.returning, setIdentityIdClause())
		query.args = append(query.args, identity[parser.FieldNameId].(string))
	}

	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		query.returning = append(query.returning, setTraceIdClause())
		query.args = append(query.args, spanContext.TraceID().String())
	}

	if len(query.returning) > 0 {
		returning = fmt.Sprintf("RETURNING %s", strings.Join(query.returning, ", "))
	}

	template := fmt.Sprintf("DELETE FROM %s %s %s %s",
		sqlQuote(query.table),
		usings,
		filters,
		returning)

	return &Statement{
		template: cleanSql(template),
		args:     query.args,
		model:    query.Model,
	}
}

// Execute the SQL statement against the database, returning the number of rows affected.
func (statement *Statement) Execute(ctx context.Context) (int, error) {
	database, err := db.GetDatabase(ctx)
	if err != nil {
		return 0, err
	}

	result, err := database.ExecuteStatement(ctx, statement.template, statement.args...)
	if err != nil {
		return 0, toRuntimeError(err)
	}

	return int(result.RowsAffected), nil
}

type Rows = []map[string]any

type ResultInfo map[string]any

type PageInfo struct {
	// Count returns the number of rows returned for the current page
	Count int

	// TotalCount returns the total number of rows across all pages
	TotalCount int

	// HasNextPage indicates if there is a subsequent page after the current page
	HasNextPage bool

	// StartCursor is the identifier representing the first row in the set
	StartCursor string

	// EndCursor is the identifier representing the last row in the set
	EndCursor string

	// PageNumber is the number of the page returned; set only for offset pagination
	PageNumber *int
}

func (pi *PageInfo) ToMap() map[string]any {
	r := map[string]any{
		"count":       pi.Count,
		"totalCount":  pi.TotalCount,
		"startCursor": pi.StartCursor,
		"endCursor":   pi.EndCursor,
		"hasNextPage": pi.HasNextPage,
	}

	if pi.PageNumber != nil {
		r["pageNumber"] = *pi.PageNumber
	}

	return r
}

func (ri *ResultInfo) ToMap() map[string]any {
	res := map[string]any{}

	for k, v := range *ri {
		res[k] = v
	}

	return res
}

// Execute the SQL statement against the database, return the rows, number of rows affected, and a boolean to indicate if there is a next page.
func (statement *Statement) ExecuteToMany(ctx context.Context, page *Page) (Rows, *ResultInfo, *PageInfo, error) {
	database, err := db.GetDatabase(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	result, err := database.ExecuteQuery(ctx, statement.template, statement.args...)
	if err != nil {
		return nil, nil, nil, toRuntimeError(err)
	}

	var resultInfo ResultInfo
	rows := result.Rows

	// Sort out the hasNextPage value, and clean up the response.
	hasNextPage := false
	var totalCount int64
	var startCursor string
	var endCursor string
	var pageNumber *int

	if page != nil && page.IsBackwards() {
		rows = lo.Reverse(rows)
	}

	returnedCount := len(rows)
	if returnedCount > 0 {
		last := rows[returnedCount-1]
		pageNumber = page.PageNumber()
		var hasPagination bool
		hasNextPage, hasPagination = last["hasnext"].(bool)

		if hasPagination {
			totalCount = last["totalcount"].(int64)

			for i, row := range rows {
				if i == 0 {
					startCursor, _ = row["id"].(string)
				}
				if i == returnedCount-1 {
					endCursor, _ = row["id"].(string)
				}
			}
		}

		facets := rows[0]["_facets"]
		if facets != nil {
			if err := json.Unmarshal([]byte(facets.(string)), &resultInfo); err != nil {
				return nil, nil, nil, fmt.Errorf("failed to parse facets data: %w", err)
			} else {
				for k, v := range resultInfo {
					if strcase.ToLowerCamel(k) != k {
						resultInfo[strcase.ToLowerCamel(k)] = v
						delete(resultInfo, k)
					}
				}
			}
		}

		for _, row := range rows {
			delete(row, "hasnext")
			delete(row, "totalcount")
			delete(row, "_facets")
			delete(row, setIdentityIdAlias)
			delete(row, setTraceIdAlias)
		}
	}

	pageInfo := &PageInfo{
		Count:       returnedCount,
		TotalCount:  int(totalCount),
		HasNextPage: hasNextPage,
		StartCursor: startCursor,
		EndCursor:   endCursor,
		PageNumber:  pageNumber,
	}

	// For certain types, we need to parse them into a format the runtime understands.
	for _, f := range statement.model.Fields {
		if f.Type.Type == proto.Type_TYPE_MODEL {
			continue
		}

		col := strcase.ToSnake(f.Name)
		for _, row := range rows {
			if val, ok := row[col]; ok && val != nil {
				if f.Type.Repeated {
					// Array fields are currently read as a single string (e.g. '{science, technology, arts}'), and
					// therefore we need to parse them into correctly typed arrays and rewrite them to the result.

					arr := val.(string)
					switch f.Type.Type {
					case proto.Type_TYPE_STRING, proto.Type_TYPE_ENUM, proto.Type_TYPE_ID, proto.Type_TYPE_MARKDOWN, proto.Type_TYPE_DURATION:
						row[col], err = ParsePostgresArray[string](arr, func(s string) (string, error) {
							return s, nil
						})
					case proto.Type_TYPE_INT:
						row[col], err = ParsePostgresArray[int](arr, func(s string) (int, error) {
							return strconv.Atoi(s)
						})
					case proto.Type_TYPE_DECIMAL, proto.Type_TYPE_VECTOR:
						row[col], err = ParsePostgresArray[float64](arr, func(s string) (float64, error) {
							return strconv.ParseFloat(s, 64)
						})
					case proto.Type_TYPE_BOOL:
						row[col], err = ParsePostgresArray[bool](arr, func(s string) (bool, error) {
							return strconv.ParseBool(s)
						})
					case proto.Type_TYPE_DATE:
						row[col], err = ParsePostgresArray[time.Time](arr, func(s string) (time.Time, error) {
							return time.Parse("2006-01-02", s)
						})
					case proto.Type_TYPE_TIMESTAMP, proto.Type_TYPE_DATETIME:
						row[col], err = ParsePostgresArray[time.Time](arr, func(s string) (time.Time, error) {
							return time.Parse("2006-01-02 15:04:05.999999999-07", s)
						})
					case proto.Type_TYPE_FILE:
						row[col], err = ParsePostgresArray[storage.FileInfo](arr, func(s string) (storage.FileInfo, error) {
							fi := storage.FileInfo{}
							if err := json.Unmarshal([]byte(s), &fi); err != nil {
								return storage.FileInfo{}, fmt.Errorf("failed to unmarshal file data: %w", err)
							}
							return fi, nil
						})
					default:
						return nil, nil, nil, fmt.Errorf("missing parsing implementation for array type %s", f.Type.Type)
					}
					if err != nil {
						return nil, nil, nil, err
					}
				} else {
					switch f.Type.Type {
					case proto.Type_TYPE_VECTOR:
						row[col], err = ParsePostgresArray[float64](val.(string), func(s string) (float64, error) {
							return strconv.ParseFloat(s, 64)
						})
					case proto.Type_TYPE_FILE:
						fi := storage.FileInfo{}
						if err = json.Unmarshal([]byte(val.(string)), &fi); err == nil {
							row[col] = fi
						}
					}
					if err != nil {
						return nil, nil, nil, err
					}
				}
			}
		}
	}

	return toLowerCamelMaps(rows), &resultInfo, pageInfo, nil
}

// Execute the SQL statement against the database and expects a single row, returns the single row or nil if no data is found.
func (statement *Statement) ExecuteToSingle(ctx context.Context) (map[string]any, error) {
	results, _, pageInfo, err := statement.ExecuteToMany(ctx, nil)
	if err != nil {
		return nil, err
	}

	if pageInfo.Count > 1 {
		return nil, fmt.Errorf("%v results returned for ExecuteToSingle which expects 0 or 1 result", pageInfo.Count)
	} else if pageInfo.Count == 0 {
		return nil, nil
	}

	return results[0], nil
}

func ParsePostgresArray[T any](array string, parse func(string) (T, error)) ([]T, error) {
	out := []T{}
	var arrayOpened, quoteOpened, escapeOpened bool
	item := &bytes.Buffer{}
	for _, r := range array {
		switch {
		case !arrayOpened:
			if r != '{' && r != '[' {
				return nil, errors.New("not a postgres array or vector as doesn't start with an opening curly brace or square brace")
			}
			arrayOpened = true
		case escapeOpened:
			item.WriteRune(r)
			escapeOpened = false
		case quoteOpened:
			switch r {
			case '\\':
				escapeOpened = true
			case '"':
				quoteOpened = false
			default:
				item.WriteRune(r)
			}
		case r == '"':
			quoteOpened = true
		case r == ',':
			// end of item
			val, err := parse(item.String())
			if err != nil {
				return nil, err
			}

			out = append(out, val)
			item.Reset()
		case r == '}', r == ']':
			// done
			if item.Len() == 0 {
				return out, nil
			}

			val, err := parse(item.String())
			if err != nil {
				return nil, err
			}

			out = append(out, val)
			return out, nil
		default:
			item.WriteRune(r)
		}
	}
	return nil, errors.New("not a postgres array as premature end of string")
}

// Builds a condition SQL template using the ? placeholder for values.
func (query *QueryBuilder) generateConditionTemplate(lhs *QueryOperand, operator ActionOperator, rhs *QueryOperand) (string, []any, error) {
	var template string
	var lhsSqlOperand, rhsSqlOperand any
	args := []any{}

	if rhs.IsValue() {
		switch operator {
		case StartsWith:
			rhs.value = rhs.value.(string) + "%%"
		case EndsWith:
			rhs.value = "%%" + rhs.value.(string)
		case Contains, NotContains:
			rhs.value = "%%" + rhs.value.(string) + "%%"
		case BeforeRelative, AfterRelative, EqualsRelative:
			timePeriod, err := timeperiod.Parse(rhs.value.(string))
			// if we're filtering by time period expressions, turn rhs operand into a timeperiod struct
			if err != nil {
				return "", nil, fmt.Errorf("operand: %v is not a valid time period: %w", rhs, err)
			}
			rhs.value = timePeriod
		}
	}

	switch {
	case lhs.IsField(), lhs.IsValue(), lhs.IsArrayValue(), lhs.IsNull(), lhs.IsInlineQuery(), lhs.IsRaw():
		lhsSqlOperand = lhs.toSqlOperandString(query)
		lhsArgs := lhs.toSqlArgs()

		args = append(args, lhsArgs...)
	default:
		return "", nil, errors.New("no handling for lhs QueryOperand type")
	}

	switch {
	case rhs.IsField(), rhs.IsValue(), rhs.IsArrayValue(), rhs.IsNull(), rhs.IsInlineQuery(), rhs.IsRaw():
		rhsSqlOperand = rhs.toSqlOperandString(query)
		rhsArgs := rhs.toSqlArgs()

		args = append(args, rhsArgs...)

	default:
		return "", nil, errors.New("no handling for rhs QueryOperand type")
	}

	// If the operand is not an array value nor an inline query,
	// then we know it's a nested relationship lookup and
	// so rather use Equals and NotEquals because we are joining.
	if !rhs.IsArrayField() && !rhs.IsArrayValue() && !rhs.IsInlineQuery() {
		if operator == OneOf {
			operator = Equals
		}
		if operator == NotOneOf {
			operator = NotEquals
		}
	}

	switch operator {
	case Equals:
		template = fmt.Sprintf("%s IS NOT DISTINCT FROM %s", lhsSqlOperand, rhsSqlOperand)
	case NotEquals:
		template = fmt.Sprintf("%s IS DISTINCT FROM %s", lhsSqlOperand, rhsSqlOperand)
	case StartsWith, EndsWith, Contains:
		template = fmt.Sprintf("%s LIKE %s", lhsSqlOperand, rhsSqlOperand)
	case NotContains:
		template = fmt.Sprintf("%s NOT LIKE %s", lhsSqlOperand, rhsSqlOperand)
	case OneOf:
		if rhs.IsInlineQuery() {
			template = fmt.Sprintf("%s IN %s", lhsSqlOperand, rhsSqlOperand)
		} else {
			template = fmt.Sprintf("%s = ANY(%s)", lhsSqlOperand, rhsSqlOperand)
		}
	case NotOneOf:
		if rhs.IsInlineQuery() {
			template = fmt.Sprintf("%s NOT IN %s", lhsSqlOperand, rhsSqlOperand)
		} else {
			template = fmt.Sprintf("NOT %s = ANY(%s)", lhsSqlOperand, rhsSqlOperand)
		}
	case LessThan, Before:
		template = fmt.Sprintf("%s < %s", lhsSqlOperand, rhsSqlOperand)
	case LessThanEquals, OnOrBefore:
		template = fmt.Sprintf("%s <= %s", lhsSqlOperand, rhsSqlOperand)
	case GreaterThan, After:
		template = fmt.Sprintf("%s > %s", lhsSqlOperand, rhsSqlOperand)
	case GreaterThanEquals, OnOrAfter:
		template = fmt.Sprintf("%s >= %s", lhsSqlOperand, rhsSqlOperand)

	/* Any query operators */
	case AnyEquals:
		template = fmt.Sprintf("%s = ANY(%s)", rhsSqlOperand, lhsSqlOperand)
	case AnyNotEquals:
		template = fmt.Sprintf("NOT %s = ANY(%s)", rhsSqlOperand, lhsSqlOperand)
	case AnyLessThan, AnyBefore:
		template = fmt.Sprintf("%s > ANY(%s)", rhsSqlOperand, lhsSqlOperand)
	case AnyLessThanEquals, AnyOnOrBefore:
		template = fmt.Sprintf("%s >= ANY(%s)", rhsSqlOperand, lhsSqlOperand)
	case AnyGreaterThan, AnyAfter:
		template = fmt.Sprintf("%s < ANY(%s)", rhsSqlOperand, lhsSqlOperand)
	case AnyGreaterThanEquals, AnyOnOrAfter:
		template = fmt.Sprintf("%s <= ANY(%s)", rhsSqlOperand, lhsSqlOperand)

	/* All query operators */
	case AllEquals:
		template = fmt.Sprintf("(%s = ALL(%s) AND %s IS DISTINCT FROM '{}')", rhsSqlOperand, lhsSqlOperand, lhsSqlOperand)
	case AllNotEquals:
		template = fmt.Sprintf("(NOT %s = ALL(%s) OR %s IS NOT DISTINCT FROM '{}')", rhsSqlOperand, lhsSqlOperand, lhsSqlOperand)
	case AllLessThan, AllBefore:
		template = fmt.Sprintf("%s > ALL(%s)", rhsSqlOperand, lhsSqlOperand)
	case AllLessThanEquals, AllOnOrBefore:
		template = fmt.Sprintf("%s >= ALL(%s)", rhsSqlOperand, lhsSqlOperand)
	case AllGreaterThan, AllAfter:
		template = fmt.Sprintf("%s < ALL(%s)", rhsSqlOperand, lhsSqlOperand)
	case AllGreaterThanEquals, AllOnOrAfter:
		template = fmt.Sprintf("%s <= ALL(%s)", rhsSqlOperand, lhsSqlOperand)

	/* Relative date operators */
	case BeforeRelative:
		template = fmt.Sprintf("%s < %s", lhsSqlOperand, rhsSqlOperand)
	case AfterRelative:
		if !rhs.IsTimePeriodValue() {
			return "", nil, fmt.Errorf("operand: %+v is not a valid time period", rhs)
		}
		tp, _ := rhs.value.(timeperiod.TimePeriod)
		end := rhsSqlOperand
		if tp.Value != 0 {
			end = fmt.Sprintf("(%s + INTERVAL '%d %s')", end, tp.Value, tp.Period)
		}
		template = fmt.Sprintf("%s >= %s", lhsSqlOperand, end)
	case EqualsRelative:
		if !rhs.IsTimePeriodValue() {
			return "", nil, fmt.Errorf("operand: %+v is not a valid time period", rhs)
		}
		tp, _ := rhs.value.(timeperiod.TimePeriod)
		end := rhsSqlOperand
		if tp.Value != 0 {
			end = fmt.Sprintf("(%s + INTERVAL '%d %s')", end, tp.Value, tp.Period)
		}

		template = fmt.Sprintf("%s >= %s AND %s < %s", lhsSqlOperand, rhsSqlOperand, lhsSqlOperand, end)

	default:
		return "", nil, fmt.Errorf("operator: %v is not yet supported", operator)
	}

	return template, args, nil
}

func copySlice[T any](a []T) []T {
	tmp := make([]T, len(a))
	copy(tmp, a)
	return tmp
}

// toLowerCamelMap returns a copy of the given map, in which all
// of the key strings are converted to LowerCamelCase.
// It is good for converting identifiers typically used as database
// table or column names, to the case requirements stipulated by the Keel schema.
func toLowerCamelMap(m map[string]any) map[string]any {
	res := map[string]any{}
	for key, value := range m {
		res[casing.ToLowerCamel(key)] = value
	}
	return res
}

// toLowerCamelMaps is a convenience wrapper around toLowerCamelMap
// that operates on a list of input maps - rather than just a single map.
func toLowerCamelMaps(maps []map[string]any) []map[string]any {
	res := []map[string]any{}
	for _, m := range maps {
		res = append(res, toLowerCamelMap(m))
	}
	return res
}

// given a variadic list of tokens (e.g sqlQuote("person", "id")),
// returns sql friendly quoted tokens: "person"."id"
func sqlQuote(tokens ...string) string {
	quotedTokens := []string{}

	for _, token := range tokens {
		switch token {
		case "*":
			// if the token is * then it doesnt need to be quoted e.g "post".*
			quotedTokens = append(quotedTokens, token)
		default:
			quotedTokens = append(quotedTokens, db.QuoteIdentifier(token))
		}
	}
	return strings.Join(quotedTokens, ".")
}

func toRuntimeError(err error) error {
	var value *db.DbError
	if errors.As(err, &value) {
		switch value.PgErrCode {
		case db.PgNotNullConstraintViolation:
			return common.NewNotNullError(value.Columns[0])
		case db.PgUniqueConstraintViolation:
			return common.NewUniquenessError(value.Columns)
		case db.PgForeignKeyConstraintViolation:
			return common.NewForeignKeyConstraintError(value.Columns[0])
		default:
			return common.RuntimeError{
				Code:    common.ErrInternal,
				Message: "action failed to complete",
			}
		}
	}

	return err
}

const (
	setIdentityIdAlias = "__keel_identity_id"
	setTraceIdAlias    = "__keel_trace_id"
)

func setIdentityIdClause() string {
	return fmt.Sprintf("set_identity_id(?) AS %s", setIdentityIdAlias)
}

func setTraceIdClause() string {
	return fmt.Sprintf("set_trace_id(?) AS %s", setTraceIdAlias)
}

// cleanSql removes redundant whitespace from SQL statements while preserving
// required spaces between keywords and identifiers
func cleanSql(sql string) string {
	// Replace multiple spaces with single space
	sql = strings.Join(strings.Fields(sql), " ")

	// Remove spaces around parentheses
	sql = strings.ReplaceAll(sql, "( ", "(")
	sql = strings.ReplaceAll(sql, " )", ")")

	return sql
}
