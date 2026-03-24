# cho

Generic typed HTTP framework for Go 1.22+. Thin wrapper over `net/http.ServeMux`.

~800 lines of code across 4 files. One dependency ([gorilla/schema](https://github.com/gorilla/schema) for form binding).

## Install

```
go get codeberg.org/kran/cho
```

## Usage

```go
package main

import (
    "net/http"
    "codeberg.org/kran/cho"
)

type AppContext struct {
    cho.BaseContext
    UserID int64
}

func main() {
    app := cho.New(func(w http.ResponseWriter, r *http.Request) *AppContext {
        return &AppContext{BaseContext: *cho.MakeBaseContext(w, r)}
    })

    app.Get("/hello", func(ctx *AppContext) error {
        return ctx.String(200, "hello")
    })

    app.Run(8080) // blocks until SIGINT/SIGTERM, then graceful shutdown
}
```

## API

### Types

```go
type Handler[T Context]      func(T) error
type Middleware[T Context]    func(T, Handler[T]) error
type ErrorHandler[T Context]  func(T, error)
type ContextMaker[T Context]  func(http.ResponseWriter, *http.Request) T
```

### Router

```go
app := cho.New(contextMaker)

app.Get(path, handler)
app.Post(path, handler)
app.Put(path, handler)
app.Delete(path, handler)
app.Handle(method, path, handler)
```

### Middleware

```go
app.Use(middleware1, middleware2)
```

Middleware executes in registration order. Chain is built at request time — middleware added after route registration still applies.

```go
app.Use(func(ctx *AppContext, next cho.Handler[*AppContext]) error {
    // before
    err := next(ctx)
    // after
    return err
})
```

Returning without calling `next` short-circuits the chain.

Handler errors are converted to HTTP responses at the innermost level (before middleware "after" code runs), so middleware like Logger can observe the correct response status. The error is still propagated for middleware inspection.

### Groups

```go
app.Group("/api", func(g *cho.Cho[*AppContext]) {
    g.Use(authMiddleware)
    g.Get("/users", listUsers)
})
```

Groups inherit parent middleware, error handler, and validator at creation time. Middleware added to a group does not affect the parent or other groups.

### Controller Mount

```go
type UserController struct{ DB *gorm.DB }

func (c *UserController) Get(ctx *AppContext) error         { /* GET /  */ }
func (c *UserController) GetShow(ctx *AppContext) error     { /* GET /show */ }
func (c *UserController) PostCreate(ctx *AppContext) error  { /* POST /create */ }
func (c *UserController) DeleteItem(ctx *AppContext) error  { /* DELETE /item */ }

app.Mount("/users", &UserController{DB: db})
```

Method name prefix determines HTTP method: `Get`, `Post`, `Put`, `Delete`, `Patch`. The remainder is converted from CamelCase to kebab-case. Methods must accept `T` and return `error`.

### Error Handling

Handlers return `error`. Unhandled errors go to the error handler.

```go
// Return an HTTPError for a specific status code and message
return cho.NewHTTPError(403, "forbidden")

// Return a plain error — becomes 500 "internal server error" (no detail leak)
return fmt.Errorf("db: %w", err)
```

Custom error handler:

```go
app.ErrorHandler = func(ctx *AppContext, err error) {
    var he *cho.HTTPError
    if errors.As(err, &he) {
        ctx.Json(he.Code, map[string]string{"error": he.Message})
    } else {
        ctx.Json(500, map[string]string{"error": "internal server error"})
    }
}
```

### Custom 404

```go
app.NotFound = func(ctx *AppContext) error {
    return ctx.Json(404, map[string]string{"error": "not found"})
}
```

### Server

```go
// Start in background, get *http.Server back
srv, err := app.Start(8080)

// With custom server configuration
srv, err := app.Start(8080, &http.Server{
    ReadTimeout:  30 * time.Second,
    WriteTimeout: 30 * time.Second,
    IdleTimeout:  120 * time.Second,
})

// Start and block until signal, then graceful shutdown (10s timeout)
err := app.Run(8080)

// Access underlying ServeMux for raw handlers
app.Mux().Handle("GET /static/", http.StripPrefix("/static/", fileServer))
```

`Start(0)` picks a random free port. The actual address is in `srv.Addr`.

### Testing

```go
w := app.Test("GET", "/hello", nil)
// w is *httptest.ResponseRecorder
fmt.Println(w.Code, w.Body.String())

w = app.Test("POST", "/data", strings.NewReader(`{"key":"value"}`))
```

## BaseContext Methods

Request:

| Method | Signature | Description |
|--------|-----------|-------------|
| `Req` | `() *http.Request` | Raw request |
| `Query` | `(key) string` | Query parameter |
| `QueryInt64` | `(key) int64` | Query parameter as int64, 0 if missing/invalid |
| `PathValue` | `(key) string` | Path parameter (Go 1.22 ServeMux `{name}` syntax) |
| `Form` | `(key) string` | Form value |
| `Header` | `(key) string` | Request header |
| `Cookie` | `(name) (*http.Cookie, error)` | Request cookie |
| `Method` | `() string` | HTTP method |
| `Path` | `() string` | URL path |
| `RemoteIP` | `() string` | Client IP (checks X-Forwarded-For, X-Real-IP, RemoteAddr) |

Binding:

| Method | Signature | Description |
|--------|-----------|-------------|
| `BindJson` | `(v any) error` | Decode JSON body (`json` tags) |
| `BindQuery` | `(v any) error` | Decode URL query parameters (`schema` tags) |
| `BindForm` | `(v any) error` | Decode form body: url-encoded or multipart (`schema` tags) |
| `FormFile` | `(key) (multipart.File, *multipart.FileHeader, error)` | Single uploaded file |
| `FormFiles` | `(key) ([]*multipart.FileHeader, error)` | Multiple uploaded files |

Response:

| Method | Signature | Description |
|--------|-----------|-------------|
| `Res` | `() http.ResponseWriter` | Raw response writer |
| `Json` | `(status, v) error` | JSON response |
| `String` | `(status, format, args...) error` | Text response |
| `Error` | `(status, msg) error` | JSON `{"error": msg}` response |
| `NoContent` | `(status) error` | Empty response |
| `SetHeader` | `(key, value)` | Set response header |
| `SetCookie` | `(cookie)` | Set-Cookie header |
| `Redirect` | `(status, url)` | HTTP redirect |
| `ServeFile` | `(filepath)` | Serve static file |
| `SSE` | `(fn, keepAlive...)` | Server-Sent Events stream |

## Validation

Two mechanisms, usable independently or together:

**Pluggable Validator** — set once, applies to all Bind methods:

```go
import "github.com/go-playground/validator/v10"

var validate = validator.New()
app.Validator = func(v any) error {
    return validate.Struct(v)
}

type LoginReq struct {
    Email    string `json:"email"    validate:"required,email"`
    Password string `json:"password" validate:"required,min=8"`
}
```

**Validatable interface** — per-struct custom logic, called automatically after binding:

```go
type CreateOrderReq struct {
    Items []int `json:"items"`
}

func (r *CreateOrderReq) Validate() error {
    if len(r.Items) == 0 {
        return cho.NewHTTPError(400, "at least one item required")
    }
    return nil
}
```

When both are present, `Validate()` runs first, then the pluggable validator.

## Built-in Middleware

```go
cho.Recovery[T](onPanic...)         // Panic recovery, returns 500
cho.MaxBodySize[T](bytes)           // Limit request body size
cho.CORS[T](origins...)             // CORS headers + preflight handling
cho.Logger[T](prefix...)            // Log method, path, status, duration
cho.RequestID[T]()                  // X-Request-ID header (generate or preserve)
cho.Timeout[T](duration)            // Cancel request context after duration
```

CORS preflight (OPTIONS) is handled automatically — no need to register explicit OPTIONS routes.

## WebSocket

cho supports WebSocket via HTTP Hijack. Use any WebSocket library:

```go
import "github.com/gorilla/websocket"

var upgrader = websocket.Upgrader{}

app.Get("/ws", func(ctx *AppContext) error {
    conn, err := upgrader.Upgrade(ctx.Res(), ctx.Req(), nil)
    if err != nil {
        return err
    }
    defer conn.Close()

    for {
        mt, msg, err := conn.ReadMessage()
        if err != nil {
            break
        }
        conn.WriteMessage(mt, msg)
    }
    return nil
})
```

## RPC

Mount a Go struct as JSON-RPC-style POST endpoints:

```go
type MathService struct{}
func (s *MathService) Add(a, b int) (int, error) { return a + b, nil }

app.MountRpc("/rpc", "math", &MathService{})
// POST /rpc/math/Add with body [2, 3] → {"data": [5]}
```

Generate a typed client:

```go
type MathClient struct {
    Add func(a, b int) (int, error)
}
var client MathClient
cho.MakeRpcClient("http://localhost:8080/rpc", "math", &client)

sum, err := client.Add(2, 3)
```

Methods with `context.Context` as first parameter receive the request context. Internal errors return `"internal server error"` without leaking details.

## Internals

- `guardWriter` wraps `http.ResponseWriter` to prevent double `WriteHeader` calls and supports `Hijack` for WebSocket
- `Cho` implements `http.Handler` — `ServeHTTP` intercepts for custom `NotFound` and CORS preflight
- Handler errors are converted to HTTP responses inside the middleware chain, then propagated for middleware inspection
- Middleware chain is built per-request from the current `middlewares` slice
- `Mount` uses reflection; method matching is by name prefix + parameter/return type
- `Start` accepts an optional `*http.Server` for custom timeouts/TLS configuration
- `Run` adds signal handling and `srv.Shutdown` on top of `Start`

## File Structure

```
cho.go          Router, middleware chain, error handling, Mount, guardWriter, helpers
context.go      Context interface, BaseContext, request/response methods, binding, SSE
middleware.go   Recovery, MaxBodySize, CORS, Logger, RequestID, Timeout
rpc.go          MountRpc, MakeRpcClient
```

## Tests

```
go test ./...           # 60 tests
go test -race ./...     # with race detector
go test -v ./...        # verbose
```
