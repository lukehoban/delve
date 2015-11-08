package proc

import (
	"bytes"
	"debug/dwarf"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/printer"
	"go/token"
	"reflect"
)

// Returns the value of the given expression
func (scope *EvalScope) EvalExpression(expr string) (*Variable, error) {
	t, err := parser.ParseExpr(expr)
	if err != nil {
		return nil, err
	}

	ev, err := scope.evalAST(t)
	if err != nil {
		return nil, err
	}
	ev.loadValue()
	return ev, nil
}

func (scope *EvalScope) evalAST(t ast.Expr) (*Variable, error) {
	switch node := t.(type) {
	case *ast.CallExpr:
		if fnnode, ok := node.Fun.(*ast.Ident); ok && len(node.Args) == 2 && (fnnode.Name == "complex64" || fnnode.Name == "complex128") {
			// implement the special case type casts complex64(f1, f2) and complex128(f1, f2)
			return scope.evalComplexCast(fnnode.Name, node)
		}
		// this must be a type cast because we do not support function calls
		return scope.evalTypeCast(node)

	case *ast.Ident:
		return scope.evalIdent(node)

	case *ast.ParenExpr:
		// otherwise just eval recursively
		return scope.evalAST(node.X)

	case *ast.SelectorExpr: // <expression>.<identifier>
		// try to interpret the selector as a package variable
		if maybePkg, ok := node.X.(*ast.Ident); ok {
			if v, err := scope.packageVarAddr(maybePkg.Name + "." + node.Sel.Name); err == nil {
				return v, nil
			}
		}
		// if it's not a package variable then it must be a struct member access
		return scope.evalStructSelector(node)

	case *ast.IndexExpr:
		return scope.evalIndex(node)

	case *ast.SliceExpr:
		if node.Slice3 {
			return nil, fmt.Errorf("3-index slice expressions not supported")
		}
		return scope.evalReslice(node)

	case *ast.StarExpr:
		// pointer dereferencing *<expression>
		return scope.evalPointerDeref(node)

	case *ast.UnaryExpr:
		// The unary operators we support are +, - and & (note that unary * is parsed as ast.StarExpr)
		switch node.Op {
		case token.AND:
			return scope.evalAddrOf(node)

		default:
			return scope.evalUnary(node)
		}

	case *ast.BinaryExpr:
		return scope.evalBinary(node)

	case *ast.BasicLit:
		return newConstant(constant.MakeFromLiteral(node.Value, node.Kind, 0), scope.Thread), nil

	default:
		return nil, fmt.Errorf("expression %T not implemented", t)

	}
}

func exprToString(t ast.Expr) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, token.NewFileSet(), t)
	return buf.String()
}

// Eval expressions: complex64(<float const>, <float const>) and complex128(<float const>, <float const>)
func (scope *EvalScope) evalComplexCast(typename string, node *ast.CallExpr) (*Variable, error) {
	realev, err := scope.evalAST(node.Args[0])
	if err != nil {
		return nil, err
	}
	imagev, err := scope.evalAST(node.Args[1])
	if err != nil {
		return nil, err
	}

	sz := 128
	ftypename := "float64"
	if typename == "complex64" {
		sz = 64
		ftypename = "float32"
	}

	realev.loadValue()
	imagev.loadValue()

	if realev.Unreadable != nil {
		return nil, realev.Unreadable
	}

	if imagev.Unreadable != nil {
		return nil, imagev.Unreadable
	}

	if realev.Value == nil || ((realev.Value.Kind() != constant.Int) && (realev.Value.Kind() != constant.Float)) {
		return nil, fmt.Errorf("can not convert \"%s\" to %s", exprToString(node.Args[0]), ftypename)
	}

	if imagev.Value == nil || ((imagev.Value.Kind() != constant.Int) && (imagev.Value.Kind() != constant.Float)) {
		return nil, fmt.Errorf("can not convert \"%s\" to %s", exprToString(node.Args[1]), ftypename)
	}

	typ := &dwarf.ComplexType{dwarf.BasicType{dwarf.CommonType{ByteSize: int64(sz / 8), Name: typename}, int64(sz), 0}}

	r := newVariable("", 0, typ, scope.Thread)
	r.Value = constant.BinaryOp(realev.Value, token.ADD, constant.MakeImag(imagev.Value))
	return r, nil
}

// Eval type cast expressions
func (scope *EvalScope) evalTypeCast(node *ast.CallExpr) (*Variable, error) {
	if len(node.Args) != 1 {
		return nil, fmt.Errorf("wrong number of arguments for a type cast")
	}

	argv, err := scope.evalAST(node.Args[0])
	if err != nil {
		return nil, err
	}
	argv.loadValue()
	if argv.Unreadable != nil {
		return nil, argv.Unreadable
	}

	fnnode := node.Fun

	// remove all enclosing parenthesis from the type name
	for {
		p, ok := fnnode.(*ast.ParenExpr)
		if !ok {
			break
		}
		fnnode = p.X
	}

	var typ dwarf.Type

	if snode, ok := fnnode.(*ast.StarExpr); ok {
		// Pointer types only appear in the dwarf informations when
		// a pointer to the type is used in the target program, here
		// we create a pointer type on the fly so that the user can
		// specify a pointer to any variable used in the target program
		ptyp, err := scope.findType(exprToString(snode.X))
		if err != nil {
			return nil, err
		}
		typ = &dwarf.PtrType{dwarf.CommonType{int64(scope.Thread.dbp.arch.PtrSize()), exprToString(fnnode)}, ptyp}
	} else {
		typ, err = scope.findType(exprToString(fnnode))
		if err != nil {
			return nil, err
		}
	}

	// only supports cast of integer constants into pointers
	ptyp, isptrtyp := typ.(*dwarf.PtrType)
	if !isptrtyp {
		return nil, fmt.Errorf("can not convert \"%s\" to %s", exprToString(node.Args[0]), typ.String())
	}

	switch argv.Kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// ok
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// ok
	default:
		return nil, fmt.Errorf("can not convert \"%s\" to %s", exprToString(node.Args[0]), typ.String())
	}

	n, _ := constant.Int64Val(argv.Value)

	v := newVariable("", 0, ptyp, scope.Thread)
	v.Children = []Variable{*newVariable("", uintptr(n), ptyp.Type, scope.Thread)}
	return v, nil
}

// Evaluates identifier expressions
func (scope *EvalScope) evalIdent(node *ast.Ident) (*Variable, error) {
	switch node.Name {
	case "true", "false":
		return newConstant(constant.MakeBool(node.Name == "true"), scope.Thread), nil
	case "nil":
		return nilVariable, nil
	}

	// try to interpret this as a local variable
	v, err := scope.extractVarInfo(node.Name)
	if err != nil {
		// if it's not a local variable then it could be a package variable w/o explicit package name
		origErr := err
		_, _, fn := scope.Thread.dbp.PCToLine(scope.PC)
		if fn != nil {
			if v, err := scope.packageVarAddr(fn.PackageName() + "." + node.Name); err == nil {
				v.Name = node.Name
				return v, nil
			}
		}
		return nil, origErr
	}
	return v, nil
}

// Evaluates expressions <subexpr>.<field name> where subexpr is not a package name
func (scope *EvalScope) evalStructSelector(node *ast.SelectorExpr) (*Variable, error) {
	xv, err := scope.evalAST(node.X)
	if err != nil {
		return nil, err
	}
	return xv.structMember(node.Sel.Name)
}

// Evaluates expressions <subexpr>[<subexpr>] (subscript access to arrays, slices and maps)
func (scope *EvalScope) evalIndex(node *ast.IndexExpr) (*Variable, error) {
	xev, err := scope.evalAST(node.X)
	if err != nil {
		return nil, err
	}
	if xev.Unreadable != nil {
		return nil, xev.Unreadable
	}

	idxev, err := scope.evalAST(node.Index)
	if err != nil {
		return nil, err
	}

	switch xev.Kind {
	case reflect.Slice, reflect.Array, reflect.String:
		if xev.base == 0 {
			return nil, fmt.Errorf("can not index \"%s\"", exprToString(node.X))
		}
		n, err := idxev.asInt()
		if err != nil {
			return nil, err
		}
		return xev.sliceAccess(int(n))

	case reflect.Map:
		idxev.loadValue()
		if idxev.Unreadable != nil {
			return nil, idxev.Unreadable
		}
		return xev.mapAccess(idxev)
	default:
		return nil, fmt.Errorf("invalid expression \"%s\" (type %s does not support indexing)", exprToString(node.X), xev.DwarfType.String())

	}
}

// Evaluates expressions <subexpr>[<subexpr>:<subexpr>]
// HACK: slicing a map expression with [0:0] will return the whole map
func (scope *EvalScope) evalReslice(node *ast.SliceExpr) (*Variable, error) {
	xev, err := scope.evalAST(node.X)
	if err != nil {
		return nil, err
	}
	if xev.Unreadable != nil {
		return nil, xev.Unreadable
	}

	var low, high int64

	if node.Low != nil {
		lowv, err := scope.evalAST(node.Low)
		if err != nil {
			return nil, err
		}
		low, err = lowv.asInt()
		if err != nil {
			return nil, fmt.Errorf("can not convert \"%s\" to int: %v", exprToString(node.Low), err)
		}
	}

	if node.High == nil {
		high = xev.Len
	} else {
		highv, err := scope.evalAST(node.High)
		if err != nil {
			return nil, err
		}
		high, err = highv.asInt()
		if err != nil {
			return nil, fmt.Errorf("can not convert \"%s\" to int: %v", exprToString(node.High), err)
		}
	}

	switch xev.Kind {
	case reflect.Slice, reflect.Array, reflect.String:
		if xev.base == 0 {
			return nil, fmt.Errorf("can not slice \"%s\"", exprToString(node.X))
		}
		return xev.reslice(low, high)
	case reflect.Map:
		if node.High != nil {
			return nil, fmt.Errorf("second slice argument must be empty for maps")
		}
		xev.mapSkip += int(low)
		return xev, nil
	default:
		return nil, fmt.Errorf("can not slice \"%s\" (type %s)", exprToString(node.X), xev.DwarfType.String())
	}
}

// Evaluates a pointer dereference expression: *<subexpr>
func (scope *EvalScope) evalPointerDeref(node *ast.StarExpr) (*Variable, error) {
	xev, err := scope.evalAST(node.X)
	if err != nil {
		return nil, err
	}

	if xev.DwarfType == nil {
		return nil, fmt.Errorf("expression \"%s\" can not be dereferenced", exprToString(node.X))
	}

	if xev.Kind != reflect.Ptr {
		return nil, fmt.Errorf("expression \"%s\" (%s) can not be dereferenced", exprToString(node.X), xev.DwarfType.String())
	}

	if len(xev.Children) == 1 {
		// this branch is here to support pointers constructed with typecasts from ints
		return &(xev.Children[0]), nil
	} else {
		rv := xev.maybeDereference()
		if rv.Addr == 0 {
			return nil, fmt.Errorf("nil pointer dereference")
		}
		return rv, nil
	}
}

// Evaluates expressions &<subexpr>
func (scope *EvalScope) evalAddrOf(node *ast.UnaryExpr) (*Variable, error) {
	xev, err := scope.evalAST(node.X)
	if err != nil {
		return nil, err
	}
	if xev.Addr == 0 {
		return nil, fmt.Errorf("can not take address of \"%s\"", exprToString(node.X))
	}

	xev.OnlyAddr = true

	typename := "*" + xev.DwarfType.String()
	rv := newVariable("", 0, &dwarf.PtrType{dwarf.CommonType{ByteSize: int64(scope.Thread.dbp.arch.PtrSize()), Name: typename}, xev.DwarfType}, scope.Thread)
	rv.Children = []Variable{*xev}
	rv.loaded = true

	return rv, nil
}

func constantUnaryOp(op token.Token, y constant.Value) (r constant.Value, err error) {
	defer func() {
		if ierr := recover(); ierr != nil {
			err = fmt.Errorf("%v", ierr)
		}
	}()
	r = constant.UnaryOp(op, y, 0)
	return
}

func constantBinaryOp(op token.Token, x, y constant.Value) (r constant.Value, err error) {
	defer func() {
		if ierr := recover(); ierr != nil {
			err = fmt.Errorf("%v", ierr)
		}
	}()
	switch op {
	case token.SHL, token.SHR:
		n, _ := constant.Uint64Val(y)
		r = constant.Shift(x, op, uint(n))
	default:
		r = constant.BinaryOp(x, op, y)
	}
	return
}

func constantCompare(op token.Token, x, y constant.Value) (r bool, err error) {
	defer func() {
		if ierr := recover(); ierr != nil {
			err = fmt.Errorf("%v", ierr)
		}
	}()
	r = constant.Compare(x, op, y)
	return
}

// Evaluates expressions: -<subexpr> and +<subexpr>
func (scope *EvalScope) evalUnary(node *ast.UnaryExpr) (*Variable, error) {
	xv, err := scope.evalAST(node.X)
	if err != nil {
		return nil, err
	}

	xv.loadValue()
	if xv.Unreadable != nil {
		return nil, xv.Unreadable
	}
	if xv.Value == nil {
		return nil, fmt.Errorf("operator %s can not be applied to \"%s\"", node.Op.String(), exprToString(node.X))
	}
	rc, err := constantUnaryOp(node.Op, xv.Value)
	if err != nil {
		return nil, err
	}
	if xv.DwarfType != nil {
		r := newVariable("", 0, xv.DwarfType, xv.thread)
		r.Value = rc
		return r, nil
	} else {
		return newConstant(rc, xv.thread), nil
	}
}

func negotiateType(op token.Token, xv, yv *Variable) (dwarf.Type, error) {
	if op == token.SHR || op == token.SHL {
		if xv.Value == nil || xv.Value.Kind() != constant.Int {
			return nil, fmt.Errorf("shift of type %s", xv.Kind)
		}

		switch yv.Kind {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			// ok
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if yv.DwarfType != nil || constant.Sign(yv.Value) < 0 {
				return nil, fmt.Errorf("shift count type %s, must be unsigned integer", yv.Kind.String())
			}
		default:
			return nil, fmt.Errorf("shift count type %s, must be unsigned integer", yv.Kind.String())
		}

		return xv.DwarfType, nil
	}

	if xv.DwarfType == nil && yv.DwarfType == nil {
		return nil, nil
	}

	if xv.DwarfType != nil && yv.DwarfType != nil {
		if xv.DwarfType.String() != yv.DwarfType.String() {
			return nil, fmt.Errorf("mismatched types \"%s\" and \"%s\"", xv.DwarfType.String(), yv.DwarfType.String())
		}
		return xv.DwarfType, nil
	} else if xv.DwarfType != nil && yv.DwarfType == nil {
		if err := yv.isType(xv.DwarfType, xv.Kind); err != nil {
			return nil, err
		}
		return xv.DwarfType, nil
	} else if xv.DwarfType == nil && yv.DwarfType != nil {
		if err := xv.isType(yv.DwarfType, yv.Kind); err != nil {
			return nil, err
		}
		return yv.DwarfType, nil
	}

	panic("unreachable")
}

func (scope *EvalScope) evalBinary(node *ast.BinaryExpr) (*Variable, error) {
	switch node.Op {
	case token.INC, token.DEC, token.ARROW:
		return nil, fmt.Errorf("operator %s not supported", node.Op.String())
	}

	xv, err := scope.evalAST(node.X)
	if err != nil {
		return nil, err
	}

	yv, err := scope.evalAST(node.Y)
	if err != nil {
		return nil, err
	}

	xv.loadValue()
	yv.loadValue()

	if xv.Unreadable != nil {
		return nil, xv.Unreadable
	}

	if yv.Unreadable != nil {
		return nil, yv.Unreadable
	}

	typ, err := negotiateType(node.Op, xv, yv)
	if err != nil {
		return nil, err
	}

	op := node.Op
	if typ != nil && (op == token.QUO) {
		_, isint := typ.(*dwarf.IntType)
		_, isuint := typ.(*dwarf.UintType)
		if isint || isuint {
			// forces integer division if the result type is integer
			op = token.QUO_ASSIGN
		}
	}

	switch op {
	case token.EQL, token.LSS, token.GTR, token.NEQ, token.LEQ, token.GEQ:
		v, err := compareOp(op, xv, yv)
		if err != nil {
			return nil, err
		}
		return newConstant(constant.MakeBool(v), xv.thread), nil

	default:
		if xv.Value == nil {
			return nil, fmt.Errorf("operator %s can not be applied to \"%s\"", node.Op.String(), exprToString(node.X))
		}

		if yv.Value == nil {
			return nil, fmt.Errorf("operator %s can not be applied to \"%s\"", node.Op.String(), exprToString(node.Y))
		}

		rc, err := constantBinaryOp(op, xv.Value, yv.Value)
		if err != nil {
			return nil, err
		}

		if typ == nil {
			return newConstant(rc, xv.thread), nil
		} else {
			r := newVariable("", 0, typ, xv.thread)
			r.Value = rc
			return r, nil
		}
	}
}

// Comapres xv to yv using operator op
// Both xv and yv must be loaded and have a compatible type (as determined by negotiateType)
func compareOp(op token.Token, xv *Variable, yv *Variable) (bool, error) {
	switch xv.Kind {
	case reflect.Bool:
		fallthrough
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fallthrough
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		fallthrough
	case reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return constantCompare(op, xv.Value, yv.Value)
	case reflect.String:
		if int64(len(constant.StringVal(xv.Value))) != xv.Len || int64(len(constant.StringVal(yv.Value))) != yv.Len {
			return false, fmt.Errorf("string too long for comparison")
		}
		return constantCompare(op, xv.Value, yv.Value)
	}

	if op != token.EQL && op != token.NEQ {
		return false, fmt.Errorf("operator %s not defined on %s", op.String(), xv.Kind.String())
	}

	var eql bool
	var err error

	switch xv.Kind {
	case reflect.Ptr:
		eql = xv.Children[0].Addr == yv.Children[0].Addr
	case reflect.Array:
		if int64(len(xv.Children)) != xv.Len || int64(len(yv.Children)) != yv.Len {
			return false, fmt.Errorf("array too long for comparison")
		}
		eql, err = equalChildren(xv, yv, true)
	case reflect.Struct:
		if len(xv.Children) != len(yv.Children) {
			return false, nil
		}
		if int64(len(xv.Children)) != xv.Len || int64(len(yv.Children)) != yv.Len {
			return false, fmt.Errorf("sturcture too deep for comparison")
		}
		eql, err = equalChildren(xv, yv, false)
	case reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
		if xv != nilVariable && yv != nilVariable {
			return false, fmt.Errorf("can not compare %s variables", xv.Kind.String())
		}

		eql = xv.base == yv.base
	default:
		return false, fmt.Errorf("unimplemented comparison of %s variables", xv.Kind.String())
	}

	if op == token.NEQ {
		return !eql, err
	}
	return eql, err
}

func equalChildren(xv, yv *Variable, shortcircuit bool) (bool, error) {
	r := true
	for i := range xv.Children {
		eql, err := compareOp(token.EQL, &xv.Children[i], &yv.Children[i])
		if err != nil {
			return false, err
		}
		r = r && eql
		if !r && shortcircuit {
			return false, nil
		}
	}
	return r, nil
}

func (scope *EvalScope) findType(name string) (dwarf.Type, error) {
	reader := scope.DwarfReader()
	typentry, err := reader.SeekToTypeNamed(name)
	if err != nil {
		return nil, err
	}
	return scope.Thread.dbp.dwarf.Type(typentry.Offset)
}

func (v *Variable) asInt() (int64, error) {
	if v.DwarfType == nil {
		if v.Value.Kind() != constant.Int {
			return 0, fmt.Errorf("can not convert constant %s to int", v.Value)
		}
	} else {
		v.loadValue()
		if v.Unreadable != nil {
			return 0, v.Unreadable
		}
		if _, ok := v.DwarfType.(*dwarf.IntType); !ok {
			return 0, fmt.Errorf("can not convert value of type %s to int", v.DwarfType.String())
		}
	}
	n, _ := constant.Int64Val(v.Value)
	return n, nil
}

func (v *Variable) asUint() (uint64, error) {
	if v.DwarfType == nil {
		if v.Value.Kind() != constant.Int {
			return 0, fmt.Errorf("can not convert constant %s to uint", v.Value)
		}
	} else {
		v.loadValue()
		if v.Unreadable != nil {
			return 0, v.Unreadable
		}
		if _, ok := v.DwarfType.(*dwarf.UintType); !ok {
			return 0, fmt.Errorf("can not convert value of type %s to uint", v.DwarfType.String())
		}
	}
	n, _ := constant.Uint64Val(v.Value)
	return n, nil
}

func (v *Variable) isType(typ dwarf.Type, kind reflect.Kind) error {
	if v.DwarfType != nil {
		if typ != nil && typ.String() != v.RealType.String() {
			return fmt.Errorf("can not convert value of type %s to %s", v.DwarfType.String(), typ.String())
		}
		return nil
	}

	if typ == nil {
		return nil
	}

	if v == nilVariable {
		switch kind {
		case reflect.Slice, reflect.Map, reflect.Func, reflect.Ptr, reflect.Chan, reflect.Interface:
			return nil
		default:
			return fmt.Errorf("mismatched types nil and %s", typ.String())
		}
	}

	converr := fmt.Errorf("can not convert %s constant to %s", v.Value, typ.String())

	if v.Value == nil {
		return converr
	}

	switch t := typ.(type) {
	case *dwarf.IntType:
		if v.Value.Kind() != constant.Int {
			return converr
		}
	case *dwarf.UintType:
		if v.Value.Kind() != constant.Int {
			return converr
		}
	case *dwarf.FloatType:
		if (v.Value.Kind() != constant.Int) && (v.Value.Kind() != constant.Float) {
			return converr
		}
	case *dwarf.BoolType:
		if v.Value.Kind() != constant.Bool {
			return converr
		}
	case *dwarf.StructType:
		if t.StructName != "string" {
			return converr
		}
		if v.Value.Kind() != constant.String {
			return converr
		}
	case *dwarf.ComplexType:
		if v.Value.Kind() != constant.Complex && v.Value.Kind() != constant.Float && v.Value.Kind() != constant.Int {
			return converr
		}
	default:
		return converr
	}

	return nil
}

func (v *Variable) sliceAccess(idx int) (*Variable, error) {
	if idx < 0 || int64(idx) >= v.Len {
		return nil, fmt.Errorf("index out of bounds")
	}
	return newVariable("", v.base+uintptr(int64(idx)*v.stride), v.fieldType, v.thread), nil
}

func (v *Variable) mapAccess(idx *Variable) (*Variable, error) {
	it := v.mapIterator()
	if it == nil {
		return nil, fmt.Errorf("can not access unreadable map: %v", v.Unreadable)
	}

	first := true
	for it.next() {
		key := it.key()
		key.loadValue()
		if key.Unreadable != nil {
			return nil, fmt.Errorf("can not access unreadable map: %v", key.Unreadable)
		}
		if first {
			first = false
			if err := idx.isType(key.DwarfType, key.Kind); err != nil {
				return nil, err
			}
		}
		eql, err := compareOp(token.EQL, key, idx)
		if err != nil {
			return nil, err
		}
		if eql {
			return it.value(), nil
		}
	}
	if v.Unreadable != nil {
		return nil, v.Unreadable
	}
	// go would return zero for the map value type here, we do not have the ability to create zeroes
	return nil, fmt.Errorf("key not found")
}

func (v *Variable) reslice(low int64, high int64) (*Variable, error) {
	if low < 0 || low >= v.Len || high < 0 || high > v.Len {
		return nil, fmt.Errorf("index out of bounds")
	}

	base := v.base + uintptr(int64(low)*v.stride)
	len := high - low

	if high-low < 0 {
		return nil, fmt.Errorf("index out of bounds")
	}

	typ := v.DwarfType
	if _, isarr := v.DwarfType.(*dwarf.ArrayType); isarr {
		typ = &dwarf.StructType{
			CommonType: dwarf.CommonType{
				ByteSize: 24,
				Name:     "",
			},
			StructName: fmt.Sprintf("[]%s", v.fieldType),
			Kind:       "struct",
			Field:      nil,
		}
	}

	r := newVariable("", 0, typ, v.thread)
	r.Cap = len
	r.Len = len
	r.base = base
	r.stride = v.stride
	r.fieldType = v.fieldType

	return r, nil
}
