package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/samber/lo"
	"github.com/teamkeel/keel/expressions/resolve"
	"github.com/teamkeel/keel/proto"
	"github.com/teamkeel/keel/runtime/auth"
	"github.com/teamkeel/keel/schema/parser"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// AuthoriseAction checks authorisation for rows using the permission and role rules applicable for an action,
// which could be defined at model- and action- levels.
func AuthoriseAction(scope *Scope, input map[string]any, rowsToAuthorise []map[string]any) (authorised bool, err error) {
	if scope.Action == nil {
		return false, errors.New("cannot authorise with AuthoriseAction if no operation is provided in scope")
	}

	if scope.Action.Type == proto.ActionType_ACTION_TYPE_UPDATE || scope.Action.Type == proto.ActionType_ACTION_TYPE_LIST {
		var ok bool
		input, ok = input["where"].(map[string]any)
		if !ok {
			input = map[string]any{}
		}
	}

	permissions := proto.PermissionsForAction(scope.Schema, scope.Action)
	return authorise(scope, permissions, input, rowsToAuthorise)
}

// AuthoriseForActionType checks authorisation for rows using permission and role rules defined for some action type,
// i.e. agnostic to any action.
func AuthoriseForActionType(scope *Scope, opType proto.ActionType, rowsToAuthorise []map[string]any) (authorised bool, err error) {
	permissions := proto.PermissionsForActionType(scope.Schema, scope.Model.Name, opType)
	return authorise(scope, permissions, map[string]any{}, rowsToAuthorise)
}

// authorise checks authorisation for rows using the slice of permission rules provided.
func authorise(scope *Scope, permissions []*proto.PermissionRule, inputs map[string]any, rowsToAuthorise []map[string]any) (authorised bool, err error) {
	ctx, span := tracer.Start(scope.Context, "Check permissions")
	defer span.End()

	scope = scope.WithContext(ctx)

	// No permissions declared means no permission can be granted.
	if len(permissions) == 0 {
		span.SetAttributes(attribute.Bool("result", false))
		span.SetAttributes(attribute.String("reason", "no permission rules"))
		return false, nil
	}

	canAuthorise, authorised, err := TryResolveAuthorisationEarly(scope, inputs, permissions)
	if err != nil {
		span.RecordError(err, trace.WithStackTrace(true))
		span.SetStatus(codes.Error, err.Error())
		return false, err
	}

	// If access can be concluded by role permissions alone
	if canAuthorise {
		return authorised, nil
	}

	span.SetAttributes(attribute.String("reason", "permission rules"))

	// If there are no expression permissions to satisfy, then access cannot be granted.
	if len(proto.PermissionsWithExpression(permissions)) == 0 {
		span.SetAttributes(attribute.Bool("result", false))
		return false, nil
	}

	idsToAuthorise := lo.Map(rowsToAuthorise, func(row map[string]interface{}, _ int) string {
		return row["id"].(string)
	})

	// Generate SQL for the permission expressions.
	stmt, err := GeneratePermissionStatement(scope, permissions, inputs, idsToAuthorise)
	if err != nil {
		span.RecordError(err, trace.WithStackTrace(true))
		span.SetStatus(codes.Error, err.Error())
		return false, err
	}

	// Execute permission query against the database.
	permissionResults, _, _, err := stmt.ExecuteToMany(scope.Context, nil)
	if err != nil {
		span.RecordError(err, trace.WithStackTrace(true))
		span.SetStatus(codes.Error, err.Error())
		return false, err
	}

	if len(permissionResults) != 1 {
		return false, errors.New("could not parse permission result as there are multiple rows")
	}

	authorised, ok := permissionResults[0]["authorised"].(bool)
	if !ok {
		return false, errors.New("could not parse authorised result")
	}

	span.SetAttributes(attribute.Bool("result", authorised))
	return authorised, nil
}

func TryResolveExpressionEarly(ctx context.Context, schema *proto.Schema, model *proto.Model, action *proto.Action, expression string, inputs map[string]any) (bool, bool) {
	env, err := cel.NewEnv()
	if err != nil {
		return false, false
	}

	ast, issues := env.Parse(expression)
	if issues != nil && len(issues.Errors()) > 0 {
		return false, false
	}

	prg, err := env.Program(ast)
	if err != nil {
		return false, false
	}

	out, _, err := prg.Eval(&OperandResolver{
		context: ctx,
		schema:  schema,
		model:   model,
		action:  action,
		inputs:  inputs,
	})
	if err != nil {
		return false, false
	}

	return true, out.Value().(bool)
}

// TryResolveAuthorisationEarly will attempt to check authorisation early without row-based querying.
// This will take into account logical conditions and multiple expression and role permission attributes.
func TryResolveAuthorisationEarly(scope *Scope, inputs map[string]any, permissions []*proto.PermissionRule) (canResolveAll bool, authorised bool, err error) {
	hasDatabaseCheck := false
	canResolveAll = false
	for _, permission := range permissions {
		canResolve := false
		authorised := false
		switch {
		case permission.Expression != nil:

			// Try resolve the permission early.
			canResolve, authorised = TryResolveExpressionEarly(scope.Context, scope.Schema, scope.Model, scope.Action, permission.Expression.Source, inputs)

			if !canResolve {
				hasDatabaseCheck = true
			}

		case permission.RoleNames != nil:
			// Roles can always be resolved early.
			canResolve = true

			// Check if this role permission is satisfied.
			authorised, err = resolveRolePermissionRule(scope.Context, scope.Schema, permission)
			if err != nil {
				return false, false, err
			}
		}

		// If this permission can be resolved now and is satisfied,
		// then we know the permission will be granted because
		// permission attributes are ORed.
		if canResolve && authorised {
			return true, true, nil
		}

		// If this permission can be resolved now and
		// there hasn't been a row/db permission, then
		// assume we can still resolve the entire action.
		canResolveAll = canResolve && !hasDatabaseCheck
	}

	return canResolveAll, false, nil
}

// resolveRolePermissionRule returns true if there is a role-based permission among the
// given list of permissions that passes.
func resolveRolePermissionRule(ctx context.Context, schema *proto.Schema, permission *proto.PermissionRule) (bool, error) {
	// If there is no authenticated user, then no role permissions can be satisfied.
	if !auth.IsAuthenticated(ctx) {
		return false, nil
	}

	identityEmail, identityDomain, verified, err := getEmailAndDomain(ctx)
	if err != nil {
		return false, err
	}

	// Can only use the email for roles if it's verified
	if !verified {
		return false, err
	}

	authorised := false
	for _, roleName := range permission.RoleNames {
		role := proto.FindRole(roleName, schema)
		for _, email := range role.Emails {
			if email == identityEmail {
				authorised = true
			}
		}

		for _, domain := range role.Domains {
			if domain == identityDomain {
				authorised = true
			}
		}
	}

	return authorised, nil
}

func GeneratePermissionStatement(scope *Scope, permissions []*proto.PermissionRule, input map[string]any, idsToAuthorise []string) (*Statement, error) {
	permissions = proto.PermissionsWithExpression(permissions)
	query := NewQuery(scope.Model, WithJoinType(JoinTypeLeft))

	// We should never have an empty list of permissions as this is checked
	// higher up in the code path, but just to be safe
	if len(permissions) == 0 {
		return nil, errors.New("no permission rules provided")
	}

	// Append SQL where conditions for each permission attribute.
	query.OpenParenthesis()
	for _, permission := range permissions {
		expression, err := parser.ParseExpression(permission.Expression.Source)
		if err != nil {
			return nil, err
		}

		_, err = resolve.RunCelVisitor(expression, GenerateFilterQuery(scope.Context, query, scope.Schema, scope.Model, scope.Action, input))
		if err != nil {
			return nil, err
		}

		// Or with the next permission attribute
		query.Or()
	}
	query.CloseParenthesis()

	query.And()

	// Filter by the ids we want to authorise
	err := query.Where(IdField(), OneOf, Value(idsToAuthorise))
	if err != nil {
		return nil, err
	}

	// Check that the number of authorised rows matches
	query.SelectClause(fmt.Sprintf("COUNT(DISTINCT %s) = %v AS authorised", IdField().toSqlOperandString(query), len(idsToAuthorise)))

	return query.SelectStatement(), nil
}

// getEmailAndDomain requires that the the given scope's context
// contains an authenticated user
func getEmailAndDomain(ctx context.Context) (email string, domain string, verified bool, err error) {
	// Use the authenticated identity's id to lookup their email address.
	identity, err := auth.GetIdentity(ctx)
	if err != nil {
		return "", "", false, err
	}

	if identity == nil {
		return "", "", false, ErrIdentityNotFound
	}

	e := identity[parser.IdentityFieldNameEmail].(string)
	if e == "" {
		return "", "", false, nil
	}

	segments := strings.Split(e, "@")
	domain = segments[1]
	return e, domain, identity[parser.IdentityFieldNameEmailVerified].(bool), nil
}
