package lsp

import (
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// evalCostLimit bounds the evaluation behind constantValue. Requests are
// handled one at a time, so an expression that iterates over large or nested
// lists would otherwise hold up every request behind it.
const evalCostLimit = 1_000_000

// constantValue returns the value of the file's expression when it has one
// without any input, which may be an error the expression always fails with.
//
// It reports false when the expression does not type-check, when its value
// depends on a variable, when it calls a function the environment declares
// without an implementation (as a configuration declares every function it
// names), or when evaluating it costs more than evalCostLimit.
//
// The result is cached alongside the type-check it was computed from.
func (f *file) constantValue(celEnv *cel.Env) (ref.Val, bool) {
	if f.evaluatedEnv != celEnv {
		f.evaluatedEnv = celEnv
		f.constant, f.isConstant = evaluateConstant(f, celEnv)
	}
	return f.constant, f.isConstant
}

func evaluateConstant(f *file, celEnv *cel.Env) (ref.Val, bool) {
	checked, _ := f.check(celEnv)
	if checked == nil || callsUnimplemented(celEnv, checked) {
		return nil, false
	}
	prog, err := celEnv.Program(checked,
		cel.CostLimit(evalCostLimit),
		cel.EvalOptions(cel.OptPartialEval),
	)
	if err != nil {
		return nil, false
	}
	// Every variable is unknown, so a value that depends on one comes out
	// unknown rather than as an error for the missing input.
	var patterns []*cel.AttributePatternType
	for _, variable := range celEnv.Variables() {
		patterns = append(patterns, cel.AttributePattern(variable.Name()))
	}
	vars, err := cel.PartialVars(map[string]any{}, patterns...)
	if err != nil {
		return nil, false
	}
	val, _, err := prog.Eval(vars)
	if err != nil {
		// An evaluation that was cut short has no value, not even an error.
		if _, isErr := val.(*types.Err); !isErr {
			return nil, false
		}
	}
	if types.IsUnknown(val) {
		return nil, false
	}
	return val, true
}

// callsUnimplemented reports whether checked calls an overload that has no
// implementation in celEnv, other than those of the standard library the
// interpreter implements itself.
func callsUnimplemented(celEnv *cel.Env, checked *cel.Ast) bool {
	var called []string
	for _, reference := range checked.NativeRep().ReferenceMap() {
		called = append(called, reference.OverloadIDs...)
	}
	if len(called) == 0 {
		return false
	}
	unimplemented := make(map[string]bool)
	for _, fn := range celEnv.Functions() {
		for _, overload := range fn.OverloadDecls() {
			if !overload.HasBinding() && !builtinOverloads()[overload.ID()] {
				unimplemented[overload.ID()] = true
			}
		}
	}
	for _, id := range called {
		if unimplemented[id] {
			return true
		}
	}
	return false
}

// builtinOverloads holds the overloads of the standard library that have no
// binding of their own, because the interpreter implements them directly: the
// logical operators and the conditional, among others.
var builtinOverloads = sync.OnceValue(func() map[string]bool {
	ids := make(map[string]bool)
	base, err := cel.NewEnv()
	if err != nil {
		return ids
	}
	for _, fn := range base.Functions() {
		for _, overload := range fn.OverloadDecls() {
			if !overload.HasBinding() {
				ids[overload.ID()] = true
			}
		}
	}
	return ids
})
