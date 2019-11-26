// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package interpreter

import (
	"fmt"

	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/packages"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/interpreter/functions"

	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// interpretablePlanner creates an Interpretable evaluation plan from a proto Expr value.
type interpretablePlanner interface {
	// Plan generates an Interpretable value (or error) from the input proto Expr.
	Plan(expr *exprpb.Expr) (Interpretable, error)
}

// newPlanner creates an interpretablePlanner which references a Dispatcher, TypeProvider,
// TypeAdapter, Packager, and CheckedExpr value. These pieces of data are used to resolve
// functions, types, and namespaced identifiers at plan time rather than at runtime since
// it only needs to be done once and may be semi-expensive to compute.
func newPlanner(disp Dispatcher,
	provider ref.TypeProvider,
	adapter ref.TypeAdapter,
	pkg packages.Packager,
	checked *exprpb.CheckedExpr,
	decorators ...InterpretableDecorator) interpretablePlanner {
	return &planner{
		disp:       disp,
		provider:   provider,
		adapter:    adapter,
		pkg:        pkg,
		identMap:   make(map[string]Interpretable),
		refMap:     checked.GetReferenceMap(),
		typeMap:    checked.GetTypeMap(),
		decorators: decorators,
	}
}

// newUncheckedPlanner creates an interpretablePlanner which references a Dispatcher, TypeProvider,
// TypeAdapter, and Packager to resolve functions and types at plan time. Namespaces present in
// Select expressions are resolved lazily at evaluation time.
func newUncheckedPlanner(disp Dispatcher,
	provider ref.TypeProvider,
	adapter ref.TypeAdapter,
	pkg packages.Packager,
	decorators ...InterpretableDecorator) interpretablePlanner {
	return &planner{
		disp:       disp,
		provider:   provider,
		adapter:    adapter,
		pkg:        pkg,
		identMap:   make(map[string]Interpretable),
		refMap:     make(map[int64]*exprpb.Reference),
		typeMap:    make(map[int64]*exprpb.Type),
		decorators: decorators,
	}
}

// planner is an implementatio of the interpretablePlanner interface.
type planner struct {
	disp       Dispatcher
	provider   ref.TypeProvider
	adapter    ref.TypeAdapter
	pkg        packages.Packager
	identMap   map[string]Interpretable
	refMap     map[int64]*exprpb.Reference
	typeMap    map[int64]*exprpb.Type
	decorators []InterpretableDecorator
}

// Plan implements the interpretablePlanner interface. This implementation of the Plan method also
// applies decorators to each Interpretable generated as part of the overall plan. Decorators are
// useful for layering functionality into the evaluation that is not natively understood by CEL,
// such as state-tracking, expression re-write, and possibly efficient thread-safe memoization of
// repeated expressions.
func (p *planner) Plan(expr *exprpb.Expr) (Interpretable, error) {
	switch expr.ExprKind.(type) {
	case *exprpb.Expr_CallExpr:
		return p.decorate(p.planCall(expr))
	case *exprpb.Expr_IdentExpr:
		return p.decorate(p.planIdent(expr))
	case *exprpb.Expr_SelectExpr:
		return p.decorate(p.planSelect(expr))
	case *exprpb.Expr_ListExpr:
		return p.decorate(p.planCreateList(expr))
	case *exprpb.Expr_StructExpr:
		return p.decorate(p.planCreateStruct(expr))
	case *exprpb.Expr_ComprehensionExpr:
		return p.decorate(p.planComprehension(expr))
	case *exprpb.Expr_ConstExpr:
		return p.decorate(p.planConst(expr))
	}
	return nil, fmt.Errorf("unsupported expr: %v", expr)
}

// decorate applies the InterpretableDecorator functions to the given Interpretable.
// Both the Interpretable and error generated by a Plan step are accepted as arguments
// for convenience.
func (p *planner) decorate(i Interpretable, err error) (Interpretable, error) {
	if err != nil {
		return nil, err
	}
	for _, dec := range p.decorators {
		i, err = dec(i)
		if err != nil {
			return nil, err
		}
	}
	return i, nil
}

// planIdent creates an Interpretable that resolves an identifier from an Activation.
func (p *planner) planIdent(expr *exprpb.Expr) (Interpretable, error) {
	ident := expr.GetIdentExpr()
	idName := ident.Name
	i, found := p.identMap[idName]
	if found {
		return i, nil
	}
	var resolver func(Activation) (ref.Val, bool)
	if p.pkg.Package() != "" {
		resolver = p.idResolver(idName)
	}
	i = &evalIdent{
		id:        expr.Id,
		name:      idName,
		provider:  p.provider,
		resolveID: resolver,
	}
	p.identMap[idName] = i
	return i, nil
}

// planSelect creates an Interpretable with either:
//  a) selects a field from a map or proto.
//  b) creates a field presence test for a select within a has() macro.
//  c) resolves the select expression to a namespaced identifier.
func (p *planner) planSelect(expr *exprpb.Expr) (Interpretable, error) {
	sel := expr.GetSelectExpr()
	// If the Select was marked TestOnly, this is a presence test.
	//
	// Note: presence tests are defined for structured (e.g. proto) and dynamic values (map, json)
	// as follows:
	//  - True if the object field has a non-default value, e.g. obj.str != ""
	//  - True if the dynamic value has the field defined, e.g. key in map
	//
	// However, presence tests are not defined for qualified identifier names with primitive types.
	// If a string named 'a.b.c' is declared in the environment and referenced within `has(a.b.c)`,
	// it is not clear whether has should error or follow the convention defined for structured
	// values.
	if sel.TestOnly {
		op, err := p.Plan(sel.GetOperand())
		if err != nil {
			return nil, err
		}
		return &evalTestOnly{
			id:    expr.Id,
			field: types.String(sel.Field),
			op:    op,
		}, nil
	}

	// If the Select id appears in the reference map from the CheckedExpr proto then it is either
	// a namespaced identifier or enum value.
	idRef, found := p.refMap[expr.Id]
	if found {
		idName := idRef.Name
		// If the reference has a value, this id represents an enum.
		if idRef.Value != nil {
			return p.Plan(&exprpb.Expr{Id: expr.Id,
				ExprKind: &exprpb.Expr_ConstExpr{
					ConstExpr: idRef.Value,
				}})
		}
		// If the identifier has already been encountered before, return the previous Iterable.
		i, found := p.identMap[idName]
		if found {
			return i, nil
		}
		// Otherwise, generate an evalIdent Interpretable.
		i = &evalIdent{
			id:       expr.Id,
			name:     idName,
			provider: p.provider,
		}
		p.identMap[idName] = i
		return i, nil
	}

	// Lastly, create a field selection Interpretable.
	op, err := p.Plan(sel.GetOperand())
	if err != nil {
		return nil, err
	}
	var resolver func(Activation) (ref.Val, bool)
	if qualID, isID := p.getQualifiedID(sel); isID {
		resolver = p.idResolver(qualID)
	}
	return &evalSelect{
		id:        expr.Id,
		field:     types.String(sel.Field),
		op:        op,
		resolveID: resolver,
	}, nil
}

// planCall creates a callable Interpretable while specializing for common functions and invocation
// patterns. Specifically, conditional operators &&, ||, ?:, and (in)equality functions result in
// optimized Interpretable values.
func (p *planner) planCall(expr *exprpb.Expr) (Interpretable, error) {
	call := expr.GetCallExpr()
	fnName := call.Function
	argCount := len(call.GetArgs())
	var offset int
	if call.Target != nil {
		argCount++
		offset++
	}
	args := make([]Interpretable, argCount, argCount)
	if call.Target != nil {
		arg, err := p.Plan(call.Target)
		if err != nil {
			return nil, err
		}
		args[0] = arg
	}
	for i, argExpr := range call.GetArgs() {
		arg, err := p.Plan(argExpr)
		if err != nil {
			return nil, err
		}
		args[i+offset] = arg
	}
	var oName string
	if oRef, found := p.refMap[expr.Id]; found &&
		len(oRef.GetOverloadId()) == 1 {
		oName = oRef.GetOverloadId()[0]
	}

	// Generate specialized Interpretable operators by function name if possible.
	switch fnName {
	case operators.LogicalAnd:
		return p.planCallLogicalAnd(expr, args)
	case operators.LogicalOr:
		return p.planCallLogicalOr(expr, args)
	case operators.Conditional:
		return p.planCallConditional(expr, args)
	case operators.Equals:
		return p.planCallEqual(expr, args)
	case operators.NotEquals:
		return p.planCallNotEqual(expr, args)
	}

	// Otherwise, generate Interpretable calls specialized by argument count.
	// Try to find the specific function by overload id.
	var fnDef *functions.Overload
	if oName != "" {
		fnDef, _ = p.disp.FindOverload(oName)
	}
	// If the overload id couldn't resolve the function, try the simple function name.
	if fnDef == nil {
		fnDef, _ = p.disp.FindOverload(fnName)
	}
	switch argCount {
	case 0:
		return p.planCallZero(expr, fnName, oName, fnDef)
	case 1:
		return p.planCallUnary(expr, fnName, oName, fnDef, args)
	case 2:
		return p.planCallBinary(expr, fnName, oName, fnDef, args)
	default:
		return p.planCallVarArgs(expr, fnName, oName, fnDef, args)
	}
}

// planCallZero generates a zero-arity callable Interpretable.
func (p *planner) planCallZero(expr *exprpb.Expr,
	function string,
	overload string,
	impl *functions.Overload) (Interpretable, error) {
	if impl == nil || impl.Function == nil {
		return nil, fmt.Errorf("no such overload: %s()", function)
	}
	return &evalZeroArity{
		id:   expr.Id,
		impl: impl.Function,
	}, nil
}

// planCallUnary generates a unary callable Interpretable.
func (p *planner) planCallUnary(expr *exprpb.Expr,
	function string,
	overload string,
	impl *functions.Overload,
	args []Interpretable) (Interpretable, error) {
	var fn functions.UnaryOp
	var trait int
	if impl != nil {
		if impl.Unary == nil {
			return nil, fmt.Errorf("no such overload: %s(arg)", function)
		}
		fn = impl.Unary
		trait = impl.OperandTrait
	}
	return &evalUnary{
		id:       expr.Id,
		function: function,
		overload: overload,
		arg:      args[0],
		trait:    trait,
		impl:     fn,
	}, nil
}

// planCallBinary generates a binary callable Interpretable.
func (p *planner) planCallBinary(expr *exprpb.Expr,
	function string,
	overload string,
	impl *functions.Overload,
	args []Interpretable) (Interpretable, error) {
	var fn functions.BinaryOp
	var trait int
	if impl != nil {
		if impl.Binary == nil {
			return nil, fmt.Errorf("no such overload: %s(lhs, rhs)", function)
		}
		fn = impl.Binary
		trait = impl.OperandTrait
	}
	return &evalBinary{
		id:       expr.Id,
		function: function,
		overload: overload,
		lhs:      args[0],
		rhs:      args[1],
		trait:    trait,
		impl:     fn,
	}, nil
}

// planCallVarArgs generates a variable argument callable Interpretable.
func (p *planner) planCallVarArgs(expr *exprpb.Expr,
	function string,
	overload string,
	impl *functions.Overload,
	args []Interpretable) (Interpretable, error) {
	var fn functions.FunctionOp
	var trait int
	if impl != nil {
		if impl.Function == nil {
			return nil, fmt.Errorf("no such overload: %s(...)", function)
		}
		fn = impl.Function
		trait = impl.OperandTrait
	}
	return &evalVarArgs{
		id:       expr.Id,
		function: function,
		overload: overload,
		args:     args,
		trait:    trait,
		impl:     fn,
	}, nil
}

// planCallEqual generates an equals (==) Interpretable.
func (p *planner) planCallEqual(expr *exprpb.Expr,
	args []Interpretable) (Interpretable, error) {
	return &evalEq{
		id:  expr.Id,
		lhs: args[0],
		rhs: args[1],
	}, nil
}

// planCallNotEqual generates a not equals (!=) Interpretable.
func (p *planner) planCallNotEqual(expr *exprpb.Expr,
	args []Interpretable) (Interpretable, error) {
	return &evalNe{
		id:  expr.Id,
		lhs: args[0],
		rhs: args[1],
	}, nil
}

// planCallLogicalAnd generates a logical and (&&) Interpretable.
func (p *planner) planCallLogicalAnd(expr *exprpb.Expr,
	args []Interpretable) (Interpretable, error) {
	return &evalAnd{
		id:  expr.Id,
		lhs: args[0],
		rhs: args[1],
	}, nil
}

// planCallLogicalOr generates a logical or (||) Interpretable.
func (p *planner) planCallLogicalOr(expr *exprpb.Expr,
	args []Interpretable) (Interpretable, error) {
	return &evalOr{
		id:  expr.Id,
		lhs: args[0],
		rhs: args[1],
	}, nil
}

// planCallConditional generates a conditional / ternary (c ? t : f) Interpretable.
func (p *planner) planCallConditional(expr *exprpb.Expr,
	args []Interpretable) (Interpretable, error) {
	return &evalConditional{
		id:     expr.Id,
		expr:   args[0],
		truthy: args[1],
		falsy:  args[2],
	}, nil
}

// planCreateList generates a list construction Interpretable.
func (p *planner) planCreateList(expr *exprpb.Expr) (Interpretable, error) {
	list := expr.GetListExpr()
	elems := make([]Interpretable, len(list.GetElements()), len(list.GetElements()))
	for i, elem := range list.GetElements() {
		elemVal, err := p.Plan(elem)
		if err != nil {
			return nil, err
		}
		elems[i] = elemVal
	}
	return &evalList{
		id:      expr.Id,
		elems:   elems,
		adapter: p.adapter,
	}, nil
}

// planCreateStruct generates a map or object construction Interpretable.
func (p *planner) planCreateStruct(expr *exprpb.Expr) (Interpretable, error) {
	str := expr.GetStructExpr()
	if len(str.MessageName) != 0 {
		return p.planCreateObj(expr)
	}
	entries := str.GetEntries()
	keys := make([]Interpretable, len(entries))
	vals := make([]Interpretable, len(entries))
	for i, entry := range entries {
		keyVal, err := p.Plan(entry.GetMapKey())
		if err != nil {
			return nil, err
		}
		keys[i] = keyVal

		valVal, err := p.Plan(entry.GetValue())
		if err != nil {
			return nil, err
		}
		vals[i] = valVal
	}
	return &evalMap{
		id:      expr.Id,
		keys:    keys,
		vals:    vals,
		adapter: p.adapter,
	}, nil
}

// planCreateObj generates an object construction Interpretable.
func (p *planner) planCreateObj(expr *exprpb.Expr) (Interpretable, error) {
	obj := expr.GetStructExpr()
	typeName := obj.MessageName
	var defined bool
	for _, qualifiedTypeName := range p.pkg.ResolveCandidateNames(typeName) {
		if _, found := p.provider.FindType(qualifiedTypeName); found {
			typeName = qualifiedTypeName
			defined = true
			break
		}
	}
	if !defined {
		return nil, fmt.Errorf("unknown type: %s", typeName)
	}
	entries := obj.GetEntries()
	fields := make([]string, len(entries))
	vals := make([]Interpretable, len(entries))
	for i, entry := range entries {
		fields[i] = entry.GetFieldKey()
		val, err := p.Plan(entry.GetValue())
		if err != nil {
			return nil, err
		}
		vals[i] = val
	}
	return &evalObj{
		id:       expr.Id,
		typeName: typeName,
		fields:   fields,
		vals:     vals,
		provider: p.provider,
	}, nil
}

// planComprehension generates an Interpretable fold operation.
func (p *planner) planComprehension(expr *exprpb.Expr) (Interpretable, error) {
	fold := expr.GetComprehensionExpr()
	accu, err := p.Plan(fold.GetAccuInit())
	if err != nil {
		return nil, err
	}
	iterRange, err := p.Plan(fold.GetIterRange())
	if err != nil {
		return nil, err
	}
	cond, err := p.Plan(fold.GetLoopCondition())
	if err != nil {
		return nil, err
	}
	step, err := p.Plan(fold.GetLoopStep())
	if err != nil {
		return nil, err
	}
	result, err := p.Plan(fold.GetResult())
	if err != nil {
		return nil, err
	}
	return &evalFold{
		id:        expr.Id,
		accuVar:   fold.AccuVar,
		accu:      accu,
		iterVar:   fold.IterVar,
		iterRange: iterRange,
		cond:      cond,
		step:      step,
		result:    result,
	}, nil
}

// planConst generates a constant valued Interpretable.
func (p *planner) planConst(expr *exprpb.Expr) (Interpretable, error) {
	val, err := p.constValue(expr.GetConstExpr())
	if err != nil {
		return nil, err
	}
	return &evalConst{
		id:  expr.Id,
		val: val,
	}, nil
}

// constValue converts a proto Constant value to a ref.Val.
func (p *planner) constValue(c *exprpb.Constant) (ref.Val, error) {
	switch c.ConstantKind.(type) {
	case *exprpb.Constant_BoolValue:
		return types.Bool(c.GetBoolValue()), nil
	case *exprpb.Constant_BytesValue:
		return types.Bytes(c.GetBytesValue()), nil
	case *exprpb.Constant_DoubleValue:
		return types.Double(c.GetDoubleValue()), nil
	case *exprpb.Constant_Int64Value:
		return types.Int(c.GetInt64Value()), nil
	case *exprpb.Constant_NullValue:
		return types.Null(c.GetNullValue()), nil
	case *exprpb.Constant_StringValue:
		return types.String(c.GetStringValue()), nil
	case *exprpb.Constant_Uint64Value:
		return types.Uint(c.GetUint64Value()), nil
	}
	return nil, fmt.Errorf("unknown constant type: %v", c)
}

// getQualifiedId converts a Select expression to a qualified identifier suitable for identifier
// resolution. If the expression is not an identifier, the second return will be false.
func (p *planner) getQualifiedID(sel *exprpb.Expr_Select) (string, bool) {
	validIdent := true
	resolvedIdent := false
	ident := sel.Field
	op := sel.Operand
	for validIdent && !resolvedIdent {
		switch op.ExprKind.(type) {
		case *exprpb.Expr_IdentExpr:
			ident = op.GetIdentExpr().Name + "." + ident
			resolvedIdent = true
		case *exprpb.Expr_SelectExpr:
			nested := op.GetSelectExpr()
			ident = nested.GetField() + "." + ident
			op = nested.Operand
		default:
			validIdent = false
		}
	}
	return ident, validIdent
}

// idResolver returns a function that resolves an identifier to its appropriate namespace.
func (p *planner) idResolver(ident string) func(Activation) (ref.Val, bool) {
	return func(ctx Activation) (ref.Val, bool) {
		for _, id := range p.pkg.ResolveCandidateNames(ident) {
			if object, found := ctx.ResolveName(id); found {
				return object, found
			}
			if typeIdent, found := p.provider.FindIdent(id); found {
				return typeIdent, found
			}
		}
		return nil, false
	}
}
