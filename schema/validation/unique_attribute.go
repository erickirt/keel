package validation

import (
	"fmt"

	"github.com/samber/lo"
	"github.com/teamkeel/keel/expressions/resolve"
	"github.com/teamkeel/keel/schema/attributes"
	"github.com/teamkeel/keel/schema/node"
	"github.com/teamkeel/keel/schema/parser"
	"github.com/teamkeel/keel/schema/query"
	"github.com/teamkeel/keel/schema/validation/errorhandling"
)

// UniqueAttributeRule validates that unique attributes are valid according to the following rules:
// - @unique can't be used on Timestamp fields
// - @unique can't be used on has-many relations
// - @unique can't be used on array fields
// - composite @unique attributes must not have duplicate field names
// - composite @unique can't specify has-many fields
func UniqueAttributeRule(asts []*parser.AST, errs *errorhandling.ValidationErrors) Visitor {
	var currentModel *parser.ModelNode
	var currentField *parser.FieldNode
	var attribute *parser.AttributeNode

	attributeArgsErr := false

	return Visitor{
		EnterModel: func(m *parser.ModelNode) {
			currentModel = m
		},
		LeaveModel: func(m *parser.ModelNode) {
			currentModel = nil
		},
		EnterField: func(f *parser.FieldNode) {
			currentField = f
		},
		LeaveField: func(f *parser.FieldNode) {
			currentField = nil
		},
		EnterAttribute: func(attr *parser.AttributeNode) {
			attribute = attr
			attributeArgsErr = false

			if attr.Name.Value != parser.AttributeUnique {
				return
			}

			compositeUnique := currentField == nil

			if !compositeUnique && len(attr.Arguments) != 0 {
				errs.AppendError(
					errorhandling.NewValidationErrorWithDetails(
						errorhandling.AttributeArgumentError,
						errorhandling.ErrorDetails{
							Message: fmt.Sprintf("%v argument(s) provided to @unique but expected 0", len(attr.Arguments)),
						},
						attr,
					),
				)
				attributeArgsErr = true
			}

			switch {
			case compositeUnique:
				if len(attr.Arguments) != 1 {
					errs.AppendError(
						errorhandling.NewValidationErrorWithDetails(
							errorhandling.AttributeArgumentError,
							errorhandling.ErrorDetails{
								Message: fmt.Sprintf("%v argument(s) provided to @unique but expected 1", len(attr.Arguments)),
							},
							attr.Name,
						),
					)
					attributeArgsErr = true
				} else {
					operands, err := resolve.AsIdentArray(attr.Arguments[0].Expression)
					if err != nil {
						return
					}

					// fieldNames := lo.Map(operands, func(o *parser.ExpressionIdent, _ int) string {
					// 	return o.ToString()
					// })

					// check there are no duplicate field names specified in the composite uniqueness
					// constraint e.g @unique([fieldA, fieldA])
					dupes := findDuplicateConstraints(operands)

					if len(dupes) > 0 {
						for _, dupe := range dupes {
							// find the last occurrence of the duplicate in the composite uniqueness constraint values list
							// so we can highlight that node in the validation error.
							_, _, found := lo.FindLastIndexOf(operands, func(o *parser.ExpressionIdent) bool {
								return o.String() == dupe.String()
							})

							if found {
								errs.AppendError(uniqueRestrictionError(dupe.Node, fmt.Sprintf("Field '%s' has already been specified as a constraint", dupe.String())))
							}
						}
					}

					// check every field specified in the unique constraint against the standard
					// restrictions for @unique attribute usage
					for _, uniqueField := range operands {
						field := query.ModelField(currentModel, uniqueField.String())

						if field == nil {
							// the field isnt a recognised field on the model, so abort as this is covered
							// by another validation
							continue
						}
						if permitted, reason := uniquePermitted(field); !permitted {
							errs.AppendError(uniqueRestrictionError(uniqueField.Node, reason))
						}
					}
				}

			default:
				// in this case, we know we are dealing with a @unique attribute attached
				// to a field
				if permitted, reason := uniquePermitted(currentField); !permitted {
					errs.AppendError(uniqueRestrictionError(attr.Node, reason))
				}
			}
		},
		LeaveAttribute: func(n *parser.AttributeNode) {
			attribute = nil
		},
		EnterExpression: func(expression *parser.Expression) {
			if attribute.Name.Value != parser.AttributeUnique {
				return
			}

			if currentField != nil {
				// There is no need to validate field-level @unique as there will be no expression present
				return
			}

			if attributeArgsErr {
				return
			}

			issues, err := attributes.ValidateCompositeUnique(currentModel, expression)
			if err != nil {
				errs.AppendError(errorhandling.NewValidationErrorWithDetails(
					errorhandling.AttributeExpressionError,
					errorhandling.ErrorDetails{
						Message: "expression could not be parsed",
					},
					expression))
			}

			if len(issues) > 0 {
				for _, issue := range issues {
					errs.AppendError(issue)
				}
				return
			}

			idents, err := resolve.AsIdentArray(expression)
			if err != nil {
				errs.AppendError(
					errorhandling.NewValidationErrorWithDetails(
						errorhandling.ActionInputError,
						errorhandling.ErrorDetails{
							Message: "@unique argument must be an array of field names",
							Hint:    "For example, @unique([sku, supplierCode])",
						},
						expression,
					),
				)
				return
			}

			if len(idents) < 2 {
				errs.AppendError(
					errorhandling.NewValidationErrorWithDetails(
						errorhandling.AttributeArgumentError,
						errorhandling.ErrorDetails{
							Message: "at least two field names to be provided",
						},
						expression,
					),
				)
			}
		},
	}
}

func uniqueRestrictionError(node node.Node, reason string) *errorhandling.ValidationError {
	return errorhandling.NewValidationErrorWithDetails(
		errorhandling.TypeError,
		errorhandling.ErrorDetails{
			Message: reason,
		},
		node,
	)
}

func findDuplicateConstraints(constraints []*parser.ExpressionIdent) (dupes []*parser.ExpressionIdent) {
	seen := map[string]bool{}

	for _, constraint := range constraints {
		if _, found := seen[constraint.String()]; found {
			dupes = append(dupes, constraint)

			continue
		}

		seen[constraint.String()] = true
	}

	return dupes
}

func uniquePermitted(f *parser.FieldNode) (bool, string) {
	// if the field is repeated and not a scalar type, then it is a has-many relationship
	if f.Repeated {
		return false, "@unique is not permitted on has many relationships or arrays"
	}

	if f.Type.Value == parser.FieldTypeTimestamp || f.Type.Value == parser.FieldTypeDate {
		return false, "@unique is not permitted on Timestamp or Date fields"
	}

	return true, ""
}
