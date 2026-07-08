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
	"path"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
)

// Handler is a typed HTTP handler. T is the only place the generic context
// lives. Like net/http, a handler writes its own response and returns nothing —
// there is no framework error-to-response conversion.
type Handler[T Context] func(T)

// Middleware is the standard net/http decorator. chi and any net/http
// middleware are usable as-is — no generics.
type Middleware = func(http.Handler) http.Handler

// Mw is a typed middleware. ctx carries the request-scoped T (with W and R
// available via the Context interface). Call next.ServeHTTP(ctx.W, ctx.R) to
// continue the chain; return without calling next to short-circuit.
type Mw[T Context] func(ctx T, next http.Handler)

// ContextMaker creates a fresh T from the raw writer/request, once per request.
type ContextMaker[T Context] func(http.ResponseWriter, *http.Request) T

// Cho is a thin generic facade over chi.Router. Its only job is to adapt
// func(T) handlers into http.HandlerFunc; routing, middleware and grouping are
// all chi's.
type Cho[T Context] struct {
	r     chi.Router
	maker ContextMaker[T]

	NotFound Handler[T]

	once sync.Once
}

// choCtxKey is the key for the typed context in request.Context.
// Unexported — only accessible through CtxFrom.
type choCtxKey struct{}

// CtxFrom extracts the typed context T from the request. The context is
// created by a built-in middleware at position 0 in the chain, so it is
// available to all subsequent middleware and the handler.
func CtxFrom[T Context](r *http.Request) T {
	return r.Context().Value(choCtxKey{}).(T)
}

// New creates a Cho backed by a fresh chi router. A built-in middleware at
// position 0 creates the typed context via ctxMaker and stores it in
// request.Context, making it available to all subsequent middleware and
// the handler via CtxFrom.
func New[T Context](em ContextMaker[T]) *Cho[T] {
	c := &Cho[T]{r: chi.NewRouter(), maker: em}

	// Built-in #0 mw: create Ctx once, store in request context.
	c.r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := em(w, r)
			*r = *r.WithContext(context.WithValue(r.Context(), choCtxKey{}, ctx))
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

// Use appends global middleware. Must be called before any route is registered
// on this router (chi enforces this with a panic).
func (c *Cho[T]) Use(mws ...Middleware) { c.r.Use(mws...) }

// UseCtx appends typed middleware. Each Mw is called per-request with the
// T instance and the next handler in the chain. The Mw may mutate T (e.g. set
// auth info) and must call next.ServeHTTP(ctx.W, ctx.R) to continue, or return
// to short-circuit.
func (c *Cho[T]) UseCtx(mws ...Mw[T]) {
	for _, mw := range mws {
		mw := mw
		c.r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mw(CtxFrom[T](r), next)
			})
		})
	}
}

// With returns a Cho whose routes carry the given inline middleware.
func (c *Cho[T]) With(mws ...Middleware) *Cho[T] {
	return &Cho[T]{r: c.r.With(mws...), maker: c.maker}
}

// Group mounts a sub-router at prefix with its own middleware scope.
func (c *Cho[T]) Group(prefix string, fn func(*Cho[T])) {
	c.r.Route(prefix, func(r chi.Router) {
		fn(&Cho[T]{r: r, maker: c.maker})
	})
}

// Handle registers a typed handler for an arbitrary method/path.
func (c *Cho[T]) Handle(method, path string, h Handler[T]) {
	c.r.Method(method, path, c.wrap(h))
}

func (c *Cho[T]) Get(path string, h Handler[T])    { c.r.Get(path, c.wrap(h)) }
func (c *Cho[T]) Post(path string, h Handler[T])   { c.r.Post(path, c.wrap(h)) }
func (c *Cho[T]) Put(path string, h Handler[T])    { c.r.Put(path, c.wrap(h)) }
func (c *Cho[T]) Delete(path string, h Handler[T]) { c.r.Delete(path, c.wrap(h)) }
func (c *Cho[T]) Patch(path string, h Handler[T])  { c.r.Patch(path, c.wrap(h)) }

// Mount auto-mounts exported methods of ctrl as routes.
// GetUserInfo -> GET /user-info, PostLogin -> POST /login. Methods must have
// signature func(T); others are skipped.
func (c *Cho[T]) Mount(pathPrefix string, ctrl any) {
	typ := reflect.TypeOf(ctrl)
	val := reflect.ValueOf(ctrl)

	var dummy T
	ctxType := reflect.TypeOf(&dummy).Elem()

	for i := 0; i < typ.NumMethod(); i++ {
		method := typ.Method(i)
		httpMethod, routePath := parseMethodName(method.Name)
		if httpMethod == "" {
			continue // name doesn't look like a handler
		}
		mType := method.Type
		// First param isn't T -> not a handler (a helper that merely shares the
		// Get/Post/... prefix). Skip silently.
		if mType.NumIn() != 2 || mType.In(1) != ctxType {
			continue
		}
		// Handler-shaped name with a T param but a non-empty return: almost
		// certainly a handler written against the old func(T) error contract.
		// Fail loudly instead of silently dropping the route.
		if mType.NumOut() != 0 {
			panic(fmt.Sprintf("cho: Mount: %s.%s must be func(%s), got %s",
				typ, method.Name, ctxType, mType))
		}
		// Resolve to a static func(T) once at registration time, so the request
		// path is a plain call with no reflect.Call.
		fn := val.Method(i).Interface().(func(T))
		c.Handle(httpMethod, path.Join(pathPrefix, routePath), fn)
	}
}

// ServeHTTP implements http.Handler.
func (c *Cho[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.once.Do(c.build)
	c.r.ServeHTTP(w, r)
}

// build registers deferred config (NotFound) onto chi before first serve.
func (c *Cho[T]) build() {
	if c.NotFound != nil {
		c.r.NotFound(c.wrap(c.NotFound))
	}
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
	c.once.Do(c.build)
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

// --- helpers (used by Mount and MountRpc) ---

var reCamelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func camelToKebab(s string) string {
	return strings.ToLower(reCamelBoundary.ReplaceAllString(s, "${1}-${2}"))
}

func parseMethodName(name string) (string, string) {
	for _, m := range []string{"Get", "Post", "Put", "Delete", "Patch"} {
		if strings.HasPrefix(name, m) {
			suffix := strings.TrimPrefix(name, m)
			if suffix == "" {
				return strings.ToUpper(m), "/"
			}
			return strings.ToUpper(m), "/" + camelToKebab(suffix)
		}
	}
	return "", ""
}
