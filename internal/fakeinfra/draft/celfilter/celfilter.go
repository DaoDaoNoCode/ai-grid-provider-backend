// Package celfilter evaluates the `filter` of a List request the way the
// draft proto says the backend does (ListTenantsRequest.filter,
// ListTenantsRequest.filter): a CEL expression over `this`, the object as its
// proto message, and `now`, the server's current time, as OSAC's
// fulfillment-service declares them (docs/draft/research/backend-conventions.md
// section 4, "Request/response shapes"), evaluated in memory over the whole
// list (section 5.3, "Filter": hub lists are small). The language
// is cel-go's standard library plus its strings extension (`ext.Strings()`),
// so `lowerAscii()`, `upperAscii()`, `trim()`, `split()` and the rest of that
// extension are available next to the built-in `contains`, `startsWith`,
// `endsWith`, `matches`, `in`, `has()`, `exists` and `all`.
//
// Fields carry their proto names (`this.metadata.labels`, `this.domains`),
// because `this` is bound to the message type with cel.Types. CEL is strict:
// indexing a map with a missing key is a `no such key` error, not false, so
// a label term needs the `in` guard
// (`"k" in this.metadata.labels && this.metadata.labels["k"] == "v"`).
//
// Only the fakes import this package (here the draft fake; in the console,
// its backendtest); the BFF forwards a client's filter unchanged and never
// evaluates one.
package celfilter

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// CostLimit bounds the runtime cost of one evaluation (one object). It is the
// Kubernetes API server's per-call limit for CEL
// (k8s.io/apiserver v0.35.8 pkg/apis/cel/config.go:22, `PerCallLimit`,
// "roughly 0.1 second"), so a filter that would loop over a large
// comprehension stops with InvalidArgument instead of holding the store.
const CostLimit = 1_000_000

// interruptEvery is how many comprehension iterations cel-go runs between
// checks of the request's context (cel.InterruptCheckFrequency), so a
// cancelled or timed-out List stops inside a long evaluation too.
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

// env returns the CEL environment for the message type of `this`, built once
// per type: the type itself (so nested messages, maps and enums resolve with
// their proto names), `this`, `now` and the strings extension.
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
// whitespace-only expression is no filter (nil, nil): the proto says empty
// means all. A syntax or type error, and an expression whose type is known
// not to be bool, are InvalidArgument naming the filter and quoting cel-go's
// error (console wording around cel-go's own text).
func Compile(src string, msg proto.Message) (*Filter, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	e, err := env(msg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "CEL environment for %s: %v", msg.ProtoReflect().Descriptor().FullName(), err)
	}
	ast, iss := e.Compile(src)
	if iss.Err() != nil {
		return nil, status.Errorf(codes.InvalidArgument, "filter %s is not a valid CEL expression: %v", src, iss.Err())
	}
	if out := ast.OutputType(); !out.IsExactType(cel.BoolType) && !out.IsExactType(cel.DynType) {
		return nil, status.Errorf(codes.InvalidArgument, "filter %s must evaluate to a bool, not %s", src, out)
	}
	prg, err := e.Program(ast, cel.CostLimit(CostLimit), cel.InterruptCheckFrequency(interruptEvery))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "filter %s is not a valid CEL expression: %v", src, err)
	}
	return &Filter{src: src, prg: prg}, nil
}

// Match evaluates the filter for one object at time now. An evaluation error
// (a missing map key, a cost over CostLimit, a type the checker could not
// rule out) is InvalidArgument naming the filter, the object (id, as the
// List answer would identify it) and cel-go's error: CEL has no "false on
// error", so one object that breaks the expression fails the whole list,
// which is what tells the caller to guard the term. The evaluation stops when
// ctx ends (the caller went away or its deadline passed), checked before each
// object and inside comprehensions; that is the context's own gRPC code
// (Canceled, DeadlineExceeded), not InvalidArgument, because the filter is
// not at fault. Callers evaluate under the store lock, so this keeps an
// abandoned List from holding it.
func (f *Filter) Match(ctx context.Context, obj proto.Message, id string, now time.Time) (bool, error) {
	if f == nil {
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, status.FromContextError(err).Err()
	}
	out, _, err := f.prg.ContextEval(ctx, map[string]any{"this": obj, "now": now})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, status.FromContextError(ctxErr).Err()
		}
		return false, status.Errorf(codes.InvalidArgument, "filter %s failed on '%s': %v", f.src, id, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, status.Errorf(codes.InvalidArgument, "filter %s must evaluate to a bool, not %s", f.src, out.Type().TypeName())
	}
	return b, nil
}
