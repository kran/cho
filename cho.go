package cho

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Handler is a typed HTTP handler function that returns an error.
type Handler[T Context] func(T) error

// Middleware wraps a Handler and can short-circuit or propagate errors.
type Middleware[T Context] func(T, Handler[T]) error

// ContextMaker creates a Context from raw http.ResponseWriter and *http.Request.
type ContextMaker[T Context] func(http.ResponseWriter, *http.Request) T

// ErrorHandler handles errors returned by handlers and middleware.
type ErrorHandler[T Context] func(T, error)

// HTTPError represents an HTTP error with status code and message.
type HTTPError struct {
	Code    int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

// NewHTTPError creates an HTTPError. If no message is provided, the default
// status text for the code is used.
func NewHTTPError(code int, msg ...string) *HTTPError {
	m := http.StatusText(code)
	if len(msg) > 0 {
		m = msg[0]
	}
	return &HTTPError{Code: code, Message: m}
}

// contextConfig holds app-level configuration injected into BaseContext per request.
type contextConfig struct {
	Validator      func(any) error
	TrustedProxies []*net.IPNet
}

// contextConfigurer is implemented by BaseContext to receive app-level config.
type contextConfigurer interface {
	setConfig(*contextConfig)
}

// Cho is a generic HTTP framework built on top of Go's standard net/http.
type Cho[T Context] struct {
	mux          *http.ServeMux
	contextMaker ContextMaker[T]
	middlewares  []Middleware[T]
	prefix       string
	cfg          contextConfig
	ErrorHandler ErrorHandler[T]
	NotFound     Handler[T]
}

// SetTrustedProxies configures which proxy IPs/CIDRs are trusted for
// X-Forwarded-For and X-Real-IP header parsing in RemoteIP().
// Accepts IPs ("10.0.0.1") or CIDRs ("10.0.0.0/8").
// When not set, RemoteIP() only returns RemoteAddr (safe default).
func (c *Cho[T]) SetTrustedProxies(cidrs []string) error {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, s := range cidrs {
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				return fmt.Errorf("cho: invalid trusted proxy IP: %s", s)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			_, cidr, _ := net.ParseCIDR(fmt.Sprintf("%s/%d", s, bits))
			nets = append(nets, cidr)
		} else {
			_, cidr, err := net.ParseCIDR(s)
			if err != nil {
				return fmt.Errorf("cho: invalid trusted proxy CIDR: %s", s)
			}
			nets = append(nets, cidr)
		}
	}
	c.cfg.TrustedProxies = nets
	return nil
}

// SetValidator configures a validation function that runs automatically
// after BindJson, BindQuery, and BindForm.
func (c *Cho[T]) SetValidator(fn func(any) error) {
	c.cfg.Validator = fn
}

// New creates a new Cho instance with the given context maker.
func New[T Context](em ContextMaker[T]) *Cho[T] {
	return &Cho[T]{
		mux:          http.NewServeMux(),
		contextMaker: em,
	}
}

// ServeHTTP implements http.Handler.
func (c *Cho[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Run middleware chain for OPTIONS requests without an explicit handler
	// to support CORS preflight.
	if r.Method == http.MethodOptions {
		_, pattern := c.mux.Handler(r)
		if pattern == "" && len(c.middlewares) > 0 {
			gw := &guardWriter{ResponseWriter: w}
			ctx := c.contextMaker(gw, r)
			noop := Handler[T](func(ctx T) error { return nil })
			chain := c.buildChain(noop)
			if err := chain(ctx); err != nil {
				c.handleError(ctx, err)
			}
			if gw.committed {
				return
			}
		}
	}

	if c.NotFound != nil {
		_, pattern := c.mux.Handler(r)
		if pattern == "" {
			gw := &guardWriter{ResponseWriter: w}
			ctx := c.contextMaker(gw, r)
			if err := c.NotFound(ctx); err != nil {
				c.handleError(ctx, err)
			}
			return
		}
	}
	c.mux.ServeHTTP(w, r)
}

// Start binds to the given port and begins serving HTTP requests in the background.
// An optional *http.Server can be passed to configure timeouts, TLS, etc.
// The server's Addr and Handler are always overwritten.
func (c *Cho[T]) Start(port int, server ...*http.Server) (*http.Server, error) {
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
		return nil, err
	}
	srv.Addr = ln.Addr().String()
	go srv.Serve(ln)
	return srv, nil
}

// Run starts the server and blocks until SIGINT/SIGTERM, then shuts down gracefully.
func (c *Cho[T]) Run(port int, server ...*http.Server) error {
	srv, err := c.Start(port, server...)
	if err != nil {
		return err
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// Mux returns the underlying *http.ServeMux.
func (c *Cho[T]) Mux() *http.ServeMux {
	return c.mux
}

// Use registers global/group-level middlewares.
func (c *Cho[T]) Use(mws ...Middleware[T]) {
	c.middlewares = append(c.middlewares, mws...)
}

// Group creates a route group with prefix and middleware inheritance.
func (c *Cho[T]) Group(prefix string, fn func(g *Cho[T])) {
	sub := &Cho[T]{
		mux:          c.mux,
		contextMaker: c.contextMaker,
		middlewares:  append([]Middleware[T]{}, c.middlewares...),
		prefix:       normalizePath(c.prefix + prefix),
		cfg:          c.cfg,
		ErrorHandler: c.ErrorHandler,
	}
	fn(sub)
}

// Handle registers a route with the given HTTP method and path.
// Middleware chain is built at request time, so middlewares added after
// route registration still apply.
func (c *Cho[T]) Handle(method, path string, handler Handler[T], mws ...Middleware[T]) {
	fullPath := normalizePath(c.prefix + path)

	adapter := func(w http.ResponseWriter, r *http.Request) {
		gw := &guardWriter{ResponseWriter: w}
		ctx := c.contextMaker(gw, r)
		if cc, ok := any(ctx).(contextConfigurer); ok {
			cc.setConfig(&c.cfg)
		}

		// Wrap handler: convert errors to HTTP responses at the innermost level
		// so middleware "after" code can observe the correct response status.
		// The original error is still returned for middleware inspection.
		wrapped := Handler[T](func(ctx T) error {
			err := handler(ctx)
			if err != nil {
				c.handleError(ctx, err)
			}
			return err
		})

		// Apply per-route middleware (innermost, closest to handler)
		for i := len(mws) - 1; i >= 0; i-- {
			mw := mws[i]
			next := wrapped
			wrapped = func(ctx T) error {
				return mw(ctx, next)
			}
		}

		chain := c.buildChain(wrapped)
		if err := chain(ctx); err != nil && !gw.committed {
			c.handleError(ctx, err)
		}
	}

	c.mux.HandleFunc(fmt.Sprintf("%s %s", method, fullPath), adapter)
}

// buildChain wraps handler with the current middleware stack.
// Chain is built at call time so middlewares added after route registration apply.
func (c *Cho[T]) buildChain(handler Handler[T]) Handler[T] {
	chain := handler
	mws := c.middlewares
	for i := len(mws) - 1; i >= 0; i-- {
		mw := mws[i]
		next := chain
		chain = func(ctx T) error {
			return mw(ctx, next)
		}
	}
	return chain
}

func (c *Cho[T]) Get(path string, h Handler[T], mws ...Middleware[T]) {
	c.Handle(http.MethodGet, path, h, mws...)
}
func (c *Cho[T]) Post(path string, h Handler[T], mws ...Middleware[T]) {
	c.Handle(http.MethodPost, path, h, mws...)
}
func (c *Cho[T]) Put(path string, h Handler[T], mws ...Middleware[T]) {
	c.Handle(http.MethodPut, path, h, mws...)
}
func (c *Cho[T]) Delete(path string, h Handler[T], mws ...Middleware[T]) {
	c.Handle(http.MethodDelete, path, h, mws...)
}

// Mount auto-mounts exported methods of ctrl as HTTP handlers.
// Method naming: GetUserInfo -> GET /user-info, PostLogin -> POST /login.
// Methods must have signature func(T) error.
func (c *Cho[T]) Mount(pathPrefix string, ctrl any) {
	typ := reflect.TypeOf(ctrl)
	val := reflect.ValueOf(ctrl)
	errType := reflect.TypeOf((*error)(nil)).Elem()

	var dummy T
	ctxType := reflect.TypeOf(&dummy).Elem()

	for i := 0; i < typ.NumMethod(); i++ {
		method := typ.Method(i)
		mType := method.Type

		if mType.NumIn() != 2 || mType.In(1) != ctxType {
			continue
		}
		if mType.NumOut() != 1 || mType.Out(0) != errType {
			continue
		}

		httpMethod, routePath := parseMethodName(method.Name)
		if httpMethod == "" {
			continue
		}

		fullPath := normalizePath(pathPrefix + routePath)
		methodVal := val.Method(i)
		handler := func(ctx T) error {
			results := methodVal.Call([]reflect.Value{reflect.ValueOf(ctx)})
			if !results[0].IsNil() {
				return results[0].Interface().(error)
			}
			return nil
		}

		c.Handle(httpMethod, fullPath, handler)
	}
}

// Test sends a test request and returns the recorded response.
func (c *Cho[T]) Test(method, target string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, target, body)
	c.ServeHTTP(w, r)
	return w
}

func (c *Cho[T]) handleError(ctx T, err error) {
	if c.ErrorHandler != nil {
		c.ErrorHandler(ctx, err)
		return
	}
	code := http.StatusInternalServerError
	msg := "internal server error"
	if he, ok := err.(*HTTPError); ok {
		code = he.Code
		msg = he.Message
	}
	ctx.Res().Header().Set("Content-Type", "application/json")
	ctx.Res().WriteHeader(code)
	json.NewEncoder(ctx.Res()).Encode(map[string]string{"error": msg})
}

// guardWriter prevents double WriteHeader calls.
type guardWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *guardWriter) WriteHeader(code int) {
	if w.committed {
		return
	}
	w.committed = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *guardWriter) Write(b []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(b)
}

func (w *guardWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *guardWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

func (w *guardWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// --- helpers ---

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

var reMultiSlash = regexp.MustCompile(`/+`)

func normalizePath(path string) string {
	if path == "" {
		return "/"
	}
	path = "/" + strings.Trim(path, "/")
	return reMultiSlash.ReplaceAllString(path, "/")
}
