# cho

Generic typed HTTP framework for Go 1.22+.

A thin facade over [chi](https://github.com/go-chi/chi) whose only job is to
**parameterize the context**: handlers receive a typed `T` (your own context
struct) instead of raw `http.ResponseWriter` + `*http.Request`. Routing,
middleware, grouping and the rest are all chi's — reused as-is, not reimplemented.

~460 lines of code across 2 files. One dependency
([gorilla/schema](https://github.com/gorilla/schema) for form binding).

## Install

```
go get codeberg.org/kran/cho
```

## Quick start

```go
package main

import (
    "net/http"

    "codeberg.org/kran/cho"
)

// Your context: embed BaseContext, add request-scoped fields.
type AppContext struct {
    cho.BaseContext
    UserID int64
}

func main() {
    app := cho.New(func(w http.ResponseWriter, r *http.Request) *AppContext {
        return &AppContext{BaseContext: *cho.MakeBaseContext(w, r)}
    })

    app.Get("/hello", func(ctx *AppContext) {
        ctx.String(200, "hello")
    })

    app.Run(8080) // blocks until SIGINT/SIGTERM, then graceful shutdown
}
```

## Core concepts

### T is created once per request

A built-in middleware at position 0 of the chain calls your `CtxMaker` once,
stores the result in `request.Context`, and **every subsequent middleware and
the handler share the same T instance**:

```go
// Read the typed context from a *http.Request (e.g. in a CtxMw):
ctx := cho.CtxFrom[*AppContext](r)
```

### Handlers return nothing

```go
type Handler[T Context] func(T)
```

Like `net/http`, a handler **writes its own response and returns nothing** —
there is no framework-level error-to-response conversion and no error concept
in the framework. Error responses are your custom Context's job:

```go
type AppContext struct {
    cho.BaseContext
}

func (c *AppContext) Fail(status int, msg string) {
    c.Error(status, msg) // {"error": msg}
}

app.Get("/orders/:id", func(ctx *AppContext) {
    if !authorized(ctx) {
        ctx.Fail(403, "forbidden")
        return
    }
    ctx.Json(200, loadOrder(ctx))
})
```

**Why**: error handling policy (status mapping, error logging, response shape)
is application-specific. Putting it in the framework would mean inventing
framework concepts (`HTTPError`, error handlers) that every project would
immediately customize anyway. The framework stays concept-free; your context
carries the policy.

## API

### Types

```go
type Handler[T Context]   func(T)                                  // typed handler
type StdMw                = func(http.Handler) http.Handler        // standard decorator (chi/net/http)
type CtxMw[T Context]     func(ctx T, next http.Handler)           // typed middleware
type CtxMaker[T Context]  func(http.ResponseWriter, *http.Request) T

type Context interface {   // implemented by BaseContext (embed it)
    Req() *http.Request
    Res() http.ResponseWriter
}
```

### Router

```go
app := cho.New(maker)          // maker: CtxMaker[T], once per request

app.Get(path, handler)
app.Post(path, handler)
app.Put(path, handler)
app.Delete(path, handler)
app.Patch(path, handler)
app.Handle(method, path, handler)      // arbitrary method

app.NotFound = func(ctx *AppContext) { ctx.Json(404, map[string]string{"error": "not found"}) }

// chi itself for advanced use (Routes, Mount, MethodNotAllowed, ...)
app.Router().MethodNotAllowed(...)
```

Path syntax is chi's: `/users/{id}` with `ctx.PathValue("id")`.

### Middleware

Two kinds, freely mixed:

```go
// Standard decorators — any chi or net/http middleware, zero adaptation:
app.UseStd(middleware.Logger, middleware.Recoverer)

// Typed middleware — receives T, may mutate it, continues via next:
app.UseCtx(func(ctx *AppContext, next http.Handler) {
    ctx.UserID = 42          // visible to handler and later CtxMw's
    next.ServeHTTP(ctx.Res(), ctx.Req())
    // return without calling next to short-circuit
})

// Inline middleware (applies only to the routes registered on the returned router):
sub := app.With(middleware.Logger)
sub.Get("/scoped", handler)

// Groups — own middleware scope, inherit the parent's:
app.Group("/api", func(g *Cho[*AppContext]) {
    g.UseCtx(requireAuth)
    g.Get("/users", listUsers)
})
```

`UseStd`/`UseCtx` must be called before routes are registered (chi enforces
this with a panic).

### Server

```go
// Background: get *http.Server back
srv, errc, err := app.Start(8080)
err = <-errc        // observe a server dying on its own (incl. ErrServerClosed)

// With custom server config
srv, errc, err := app.Start(8080, &http.Server{
    ReadTimeout: 30 * time.Second,
    IdleTimeout: 120 * time.Second,
    // Avoid WriteTimeout if using SSE/WebSocket
})

// Block until signal, then graceful shutdown (10s timeout)
err := app.Run(8080)

// Start(0) picks a random free port; actual address is in srv.Addr.
```

### Testing

```go
w := app.Test("GET", "/hello", nil)          // *httptest.ResponseRecorder
w := app.Test("POST", "/data", strings.NewReader(`{"key":"value"}`))
```

## BaseContext methods

Request:

| Method | Signature | Description |
|--------|-----------|-------------|
| `Req` | `() *http.Request` | Raw request |
| `Res` | `() http.ResponseWriter` | Raw response writer |
| `Query` | `(key) string` | Query parameter |
| `QueryInt64` | `(key, def ...int64) int64` | Query parameter as int64; def applies only when absent (empty/unparsable → 0) |
| `QueryInt` | `(key, def ...int) int` | Query parameter as int; def applies only when absent (empty/unparsable → 0) |
| `PathValue` | `(key) string` | Path parameter (chi `{name}` syntax) |
| `Form` | `(key) string` | Form value |
| `Header` | `(key) string` | Request header |
| `Cookie` | `(name) (*http.Cookie, error)` | Request cookie |
| `Method` | `() string` | HTTP method |
| `Path` | `() string` | URL path |

Binding (all run validation — see below):

| Method | Signature | Description |
|--------|-----------|-------------|
| `BindJson` | `(v any) error` | Decode JSON body (`json` tags) |
| `BindQuery` | `(v any) error` | Decode URL query (`schema` tags) |
| `BindForm` | `(v any) error` | Form body: url-encoded or multipart (`schema` tags, 32 MB memory limit) |
| `FormFile` | `(key) (multipart.File, *multipart.FileHeader, error)` | Single upload (after BindForm) |
| `FormFiles` | `(key) ([]*multipart.FileHeader, error)` | Multiple uploads (after BindForm) |

Response:

| Method | Signature | Description |
|--------|-----------|-------------|
| `Json` | `(status, v) error` | JSON response |
| `String` | `(status, s) error` | Plain text — `%` is literal (no format interpretation, user content safe) |
| `Error` | `(status, msg) error` | JSON `{"error": msg}` |
| `NoContent` | `(status) error` | Empty response |
| `SetHeader` | `(key, value)` | Response header |
| `SetCookie` | `(cookie *http.Cookie)` | Set-Cookie header |
| `Redirect` | `(status, url)` | HTTP redirect |
| `ServeFile` | `(filepath)` | Serve a file |

Streaming:

| Method | Signature | Description |
|--------|-----------|-------------|
| `SSE` | `(fn func(send func(event, data string)), keepAlive ...time.Duration)` | Server-Sent Events: flush per event, close on fn return or client disconnect, optional periodic keep-alive |

## Validation

Two mechanisms, usable independently or together:

**Pluggable validator** — set once in your CtxMaker, runs after every bind:

```go
import "github.com/go-playground/validator/v10"

var validate = validator.New()

app := cho.New(func(w http.ResponseWriter, r *http.Request) *AppContext {
    ctx := &AppContext{BaseContext: *cho.MakeBaseContext(w, r)}
    ctx.SetValidator(validate.Struct)
    return ctx
})

type LoginReq struct {
    Email    string `json:"email"    validate:"required,email"`
    Password string `json:"password" validate:"required,min=8"`
}
```

**Validatable interface** — per-struct custom logic:

```go
func (r *CreateOrderReq) Validate() error {
    if len(r.Items) == 0 {
        return errors.New("at least one item required")
    }
    return nil
}
```

When both are present, `Validate()` runs first, then the pluggable validator.

## SSE example

```go
app.Post("/chat", func(ctx *AppContext) {
    ctx.SSE(func(send func(event, data string)) {
        for _, chunk := range streamUpstream() {
            send("", chunk)     // flushed immediately
        }
    }, 15*time.Second)          // optional keep-alive
})
```

Client disconnection cancels the stream via `r.Context().Done()`.

## Tests

```
go test ./...          # 26 tests
go test -race ./...    # race detector
```

## Design notes

- **Thin on purpose**: cho does exactly one thing — hand a typed `T` to
  handlers and middleware. Everything else is chi's (routing, matching,
  middleware ecosystem, mounts), reachable via `Router()` when needed.
- **No framework concepts**: no error type, no error handler, no controller
  mount, no RPC. Policy lives in your context; features live in your
  middleware. The framework adds nothing you would want to replace.
- **One T per request**: created by the built-in #0 middleware, shared by all
  middleware and the handler — no re-creation, no sync primitives.
- **std middleware compatible**: any `func(http.Handler) http.Handler` works
  as-is (`UseStd`/`With`).
