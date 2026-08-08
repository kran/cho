package cho

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
)

// Handler is a typed HTTP handler. T is the only place the generic context
// lives. Like net/http, a handler writes its own response and returns nothing —
// there is no framework error-to-response conversion.
// Error responses are the custom Context's job (e.g. ctx.Error(status, msg)
// defined on your own context struct) — the framework stays concept-free.
type Handler[T any] func(T)

// StdMw is the standard net/http decorator. chi and any net/http
// middleware are usable as-is — no generics.
type StdMw = func(http.Handler) http.Handler

// CtxMw is a typed middleware. ctx carries the request-scoped T; call next()
// to continue the chain (the framework closes over the current w/r), or
// return without calling it to short-circuit. Access the raw w/r via
// ctx.Res()/ctx.Req() when needed.
type CtxMw[T any] func(ctx T, next func())

// CtxMaker creates a fresh T from the raw writer/request, once per request.
// The request passed to the maker is the derived request the chain starts
// with (cho introduces no context fork of its own). Note: std middleware
// registered via UseStd may further derive the request (WithContext) —
// values/deadlines they attach are NOT visible through T's stored request;
// read the live r from your own middleware layer when that matters.
type CtxMaker[T any] func(http.ResponseWriter, *http.Request) T

// Cho is a thin generic facade over chi.Router. Its only job is to adapt
// func(T) handlers into http.HandlerFunc; routing, middleware and grouping are
// all chi's.
type Cho[T any] struct {
	r      chi.Router
	maker  CtxMaker[T]
	prefix string  // Group 累积的路径前缀
	mws    []StdMw // 挂起中间件: With/组内 UseStd 累积, 路由注册时内联生效
	scoped bool    // 组/With 子实例: UseStd 改为挂起而非全局 Use
}

// choCtxKeyInst is the singleton key instance.
var choCtxKeyInst = &struct{}{}

// choHolder is a mutable slot in the request context: the built-in #0
// middleware stores an empty holder, derives the new request, then fills the
// holder with the CtxMaker result — so T.R is always the very request the
// rest of the chain sees (no context fork between maker and handler).
type choHolder[T any] struct{ v T }

// CtxFrom extracts the typed context T from the request. The context is
// created by a built-in middleware at position 0 in the chain, so it is
// available to all subsequent middleware and the handler.
// Panics with a pointing message when the request carries no context of
// this type (e.g. handlers mounted across Cho instances, or raw chi routes).
func CtxFrom[T any](r *http.Request) T {
	h, ok := r.Context().Value(choCtxKeyInst).(*choHolder[T])
	if !ok {
		panic("cho: no context of this type on request — handler/middleware registered outside its Cho[T] instance?")
	}
	return h.v
}

// New creates a Cho backed by a fresh chi router. A built-in middleware at
// position 0 creates the typed context via ctxMaker and stores it in
// request.Context, making it available to all subsequent middleware and
// the handler via CtxFrom.
func New[T any](em CtxMaker[T]) *Cho[T] {
	c := &Cho[T]{r: chi.NewRouter(), maker: em}

	// Built-in #0 mw: store an empty holder, derive the request context, then
	// create Ctx from the NEW request and fill the holder — T.R is the same
	// pointer the rest of the chain sees (no context fork). Never overwrite
	// *r in place — net/http may hold the request reference after the handler
	// returns (data race); pass the derived request down.
	c.r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := &choHolder[T]{}
			r = r.WithContext(context.WithValue(r.Context(), choCtxKeyInst, h))
			h.v = em(w, r)
			next.ServeHTTP(w, r)
		})
	})

	return c
}

// Router exposes the underlying chi.Router for advanced use (Routes(), Mount, etc).
func (c *Cho[T]) Router() chi.Router { return c.r }

// wrap is the single point where T is accessed. Each request reuses the
// Ctx instance created by the built-in #0 middleware.
func (c *Cho[T]) wrap(h Handler[T]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(CtxFrom[T](r))
	}
}

// UseStd appends global middleware. Must be called before any route is registered
// on this router (chi enforces this with a panic).
//
// 在 Group/With 子实例上调用时语义变为: 追加到该作用域的挂起列表, 对此后
// 注册的作用域内路由生效 (不再是全局, 也不触发 chi 的 use-after-routes panic)。
func (c *Cho[T]) UseStd(mws ...StdMw) {
	if c.scoped {
		c.mws = append(c.mws, mws...)
		return
	}
	c.r.Use(mws...)
}

// UseCtx appends typed middleware. Each CtxMw is called per-request with the
// T instance; call next() to continue the chain (the framework closes over
// the current w/r), or return without calling it to short-circuit. The CtxMw
// may mutate T (e.g. set auth info) — visible to the handler and later CtxMw's.
// 作用域语义同 UseStd。
func (c *Cho[T]) UseCtx(mws ...CtxMw[T]) {
	// Go 1.22+ loop variables are per-iteration, so mw needs no capture copy.
	for _, mw := range mws {
		std := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mw(CtxFrom[T](r), func() { next.ServeHTTP(w, r) })
			})
		}
		if c.scoped {
			c.mws = append(c.mws, std)
		} else {
			c.r.Use(std)
		}
	}
}

// With returns a Cho whose subsequently registered routes carry the given
// inline middleware. 与 chi 原生 With 不同: 返回的实例仍可 UseStd/UseCtx
// (追加挂起, 不再有 "With 后不能 Use" 的限制)。
func (c *Cho[T]) With(mws ...StdMw) *Cho[T] {
	return &Cho[T]{
		r: c.r, maker: c.maker, prefix: c.prefix, scoped: true,
		mws: append(slices.Clone(c.mws), mws...),
	}
}

// Group 纯路径前缀分组 (不经 chi.Route): 注册路径 = 前缀 + path。
//
// 为什么不用 chi.Route: Route = Mount, Mount 会在精确节点注册 mSTUB 占位
// handler 转给子路由, 同路径已注册的精确 handler 会被静默覆盖 (顺序敏感、
// 无 panic) — 前缀式 Group 没有这个坑, 精确路由与子树任意顺序共存,
// 真重复注册由 chi 原生 duplicate panic 响亮报出。
//
// 注意: 组的中间件作用域 = 组内 UseStd/UseCtx 挂起, 对此后注册的组内路由
// 生效; SetNotFound 仍是路由器级 (组内调用影响整个路由器), 与 chi Route
// 子路由的独立 NotFound 语义不同。
func (c *Cho[T]) Group(prefix string, fn func(*Cho[T])) {
	fn(&Cho[T]{
		r: c.r, maker: c.maker, prefix: c.prefix + prefix, scoped: true,
		mws: slices.Clone(c.mws),
	})
}

// Handle registers a typed handler for an arbitrary method/path.
func (c *Cho[T]) Handle(method, path string, h Handler[T]) {
	c.method(method, path, h)
}

func (c *Cho[T]) method(method, path string, h Handler[T]) {
	hf := c.wrap(h)
	if len(c.mws) > 0 {
		c.r.With(c.mws...).Method(method, c.prefix+path, hf)
		return
	}
	c.r.Method(method, c.prefix+path, hf)
}

func (c *Cho[T]) Get(path string, h Handler[T])    { c.method(http.MethodGet, path, h) }
func (c *Cho[T]) Post(path string, h Handler[T])   { c.method(http.MethodPost, path, h) }
func (c *Cho[T]) Put(path string, h Handler[T])    { c.method(http.MethodPut, path, h) }
func (c *Cho[T]) Delete(path string, h Handler[T]) { c.method(http.MethodDelete, path, h) }
func (c *Cho[T]) Patch(path string, h Handler[T])  { c.method(http.MethodPatch, path, h) }

// SetNotFound registers a typed 404 handler directly on the router
// (router-scoped in chi — set it on the root instance or any group).
func (c *Cho[T]) SetNotFound(h Handler[T]) {
	c.r.NotFound(c.wrap(h))
}

// ServeHTTP implements http.Handler.
func (c *Cho[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.r.ServeHTTP(w, r)
}

// Test sends a request through the full router and records the response.
func (c *Cho[T]) Test(method, target string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, target, body)
	c.ServeHTTP(w, r)
	return w
}

// Start serves in the background on the given port. Addr/Handler are overwritten.
// The returned channel (buffered) receives the result of srv.Serve — including
// http.ErrServerClosed on a clean shutdown — so callers can observe a server
// that dies on its own instead of silently swallowing the error.
func (c *Cho[T]) Start(port int, server ...*http.Server) (*http.Server, <-chan error, error) {
	var srv *http.Server
	if len(server) > 0 && server[0] != nil {
		srv = server[0]
	} else {
		srv = &http.Server{}
	}
	srv.Addr = fmt.Sprintf(":%d", port)
	srv.Handler = c
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return nil, nil, err
	}
	srv.Addr = ln.Addr().String()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	return srv, errc, nil
}

// Run starts the server and blocks until SIGINT/SIGTERM or a Serve failure,
// then shuts down gracefully.
func (c *Cho[T]) Run(port int, server ...*http.Server) error {
	srv, errc, err := c.Start(port, server...)
	if err != nil {
		return err
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	case err := <-errc:
		return err
	}
}
