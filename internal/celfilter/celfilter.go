// Package celfilter evaluates the `filter` of a List request: a CEL expression
// over `this` (the object as its proto message) and `now` (the server's
// current time), as OSAC's fulfillment-service declares them. The language is
// cel-go's standard library plus its strings extension (`ext.Strings()`).
//
// Fields carry their proto names (`this.metadata.labels`, `this.spec.priority`),
// because `this` is bound to the message type with cel.Types. CEL is strict:
// indexing a map with a missing key is a `no such key` error, not false, so
// a label term needs the `in` guard
// (`"k" in this.metadata.labels && this.metadata.labels["k"] == "v"`).
package celfilter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	connect "connectrpc.com/connect/v2"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"google.golang.org/protobuf/proto"
)

// CostLimit bounds the runtime cost of one evaluation (one object). It is the
// Kubernetes API server's per-call limit for CEL
// (k8s.io/apiserver pkg/apis/cel/config.go PerCallLimit).
const CostLimit = 1_000_000

// interruptEvery is how many comprehension iterations cel-go runs between
// checks of the request's context (cel.InterruptCheckFrequency).
const interruptEvery = 100

// Filter is a compiled filter. A nil *Filter (from an empty expression)
// matches every object.
type Filter struct {
	src string
	prg cel.Program
}

var (
	envMu sync.Mutex
	envs  = map[string]*cel.Env{}
)

func env(msg proto.Message) (*cel.Env, error) {
	name := string(msg.ProtoReflect().Descriptor().FullName())
	envMu.Lock()
	defer envMu.Unlock()
	if e, ok := envs[name]; ok {
		return e, nil
	}
	e, err := cel.NewEnv(
		cel.Types(msg),
		cel.Variable("this", cel.ObjectType(name)),
		cel.Variable("now", cel.TimestampType),
		ext.Strings(),
	)
	if err != nil {
		return nil, err
	}
	envs[name] = e
	return e, nil
}

// Compile parses and type-checks src with `this` of msg's type. An empty or
// whitespace-only expression is no filter (nil, nil).
func Compile(src string, msg proto.Message) (*Filter, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	e, err := env(msg)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "CEL environment for %s: %v", msg.ProtoReflect().Descriptor().FullName(), err)
	}
	ast, iss := e.Compile(src)
	if iss.Err() != nil {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "filter %s is not a valid CEL expression: %v", src, iss.Err())
	}
	if out := ast.OutputType(); !out.IsExactType(cel.BoolType) && !out.IsExactType(cel.DynType) {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "filter %s must evaluate to a bool, not %s", src, out)
	}
	prg, err := e.Program(ast, cel.CostLimit(CostLimit), cel.InterruptCheckFrequency(interruptEvery))
	if err != nil {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "filter %s is not a valid CEL expression: %v", src, err)
	}
	return &Filter{src: src, prg: prg}, nil
}

// Match evaluates the filter for one object at time now.
func (f *Filter) Match(ctx context.Context, obj proto.Message, id string, now time.Time) (bool, error) {
	if f == nil {
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, contextError(err)
	}
	out, _, err := f.prg.ContextEval(ctx, map[string]any{"this": obj, "now": now})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, contextError(ctxErr)
		}
		return false, connect.Errorf(connect.CodeInvalidArgument, "filter %s failed on '%s': %v", f.src, id, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, connect.Errorf(connect.CodeInvalidArgument, "filter %s must evaluate to a bool, not %s", f.src, out.Type().TypeName())
	}
	return b, nil
}

func contextError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err.Error())
	default:
		return connect.Errorf(connect.CodeInternal, "%v", err)
	}
}
