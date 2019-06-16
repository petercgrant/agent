package plugin

import (
	"errors"
	"fmt"

	"github.com/buildkite/conditional/ast"
	"github.com/buildkite/conditional/evaluator"
	"github.com/buildkite/conditional/lexer"
	"github.com/buildkite/conditional/object"
	"github.com/buildkite/conditional/parser"
)

type Filter struct {
	Expression ast.Expression
}

func ParseFilter(condition string) (*Filter, error) {
	l := lexer.New(condition)
	p := parser.New(l)
	expr := p.Parse()

	if errs := p.Errors(); len(errs) > 0 {
		return nil, errors.New(errs[0])
	}

	return &Filter{expr}, nil
}

func (f *Filter) Match(p *Plugin) (bool, error) {
	obj := evaluator.Eval(f.Expression, buildConditionalEnvironment(p))

	err, ok := obj.(*object.Error)
	if ok {
		return false, fmt.Errorf("Failed to evaluate %v => %s", f.Expression, err.Message)
	}

	result, ok := obj.(*object.Boolean)
	if !ok {
		return false, fmt.Errorf("Conditional result is not Boolean. got=%T (%+v)", obj, obj)
	}

	return result.Value, nil
}

func buildConditionalEnvironment(p *Plugin) *object.Environment {
	env := object.NewEnvironment()
	env.Set(`plugin`, &object.Struct{Props: map[string]object.Object{
		`scheme`: &object.String{Value: p.Scheme},
	}})

	return env
}
