package cho

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testCtx is a minimal context for testing.
type testCtx struct {
	BaseContext
}

func newTestApp() *Cho[*testCtx] {
	return New(func(w http.ResponseWriter, r *http.Request) *testCtx {
		return &testCtx{BaseContext: BaseContext{W: w, R: r}}
	})
}

// --- Routing ---

func TestBasicRouting(t *testing.T) {
	app := newTestApp()
	app.Get("/hello", func(ctx *testCtx) { ctx.String(200, "hello") })
	app.Post("/data", func(ctx *testCtx) { ctx.String(200, "posted") })
	app.Put("/data", func(ctx *testCtx) { ctx.String(200, "put") })
	app.Delete("/data", func(ctx *testCtx) { ctx.String(200, "deleted") })

	tests := []struct {
		method, path, want string
	}{
		{"GET", "/hello", "hello"},
		{"POST", "/data", "posted"},
		{"PUT", "/data", "put"},
		{"DELETE", "/data", "deleted"},
	}
	for _, tt := range tests {
		w := app.Test(tt.method, tt.path, nil)
		if w.Body.String() != tt.want {
			t.Errorf("%s %s = %q, want %q", tt.method, tt.path, w.Body.String(), tt.want)
		}
	}
}

func TestGroup(t *testing.T) {
	app := newTestApp()
	app.Group("/api", func(g *Cho[*testCtx]) {
		g.Get("/users", func(ctx *testCtx) { ctx.String(200, "users") })
		g.Group("/v2", func(g2 *Cho[*testCtx]) {
			g2.Get("/items", func(ctx *testCtx) { ctx.String(200, "items-v2") })
		})
	})

	tests := []struct {
		path, want string
	}{
		{"/api/users", "users"},
		{"/api/v2/items", "items-v2"},
	}
	for _, tt := range tests {
		w := app.Test("GET", tt.path, nil)
		if w.Body.String() != tt.want {
			t.Errorf("GET %s = %q, want %q", tt.path, w.Body.String(), tt.want)
		}
	}
}

// --- Error responses (written by the handler itself) ---

func TestHandlerWritesErrorResponse(t *testing.T) {
	app := newTestApp()
	app.Get("/err", func(ctx *testCtx) {
		ctx.Error(http.StatusForbidden, "access denied")
	})

	w := app.Test("GET", "/err", nil)
	if w.Code != 403 {
		t.Errorf("status = %d, want 403", w.Code)
	}
	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "access denied" {
		t.Errorf("error = %q, want %q", body["error"], "access denied")
	}
}

// --- NotFound ---

func TestNotFoundHandler(t *testing.T) {
	app := newTestApp()
	app.NotFound = func(ctx *testCtx) {
		ctx.Json(404, map[string]string{"error": "not found"})
	}
	app.Get("/exists", func(ctx *testCtx) { ctx.String(200, "ok") })

	// Existing route works
	w := app.Test("GET", "/exists", nil)
	if w.Code != 200 {
		t.Errorf("existing route status = %d, want 200", w.Code)
	}

	// Unknown route triggers NotFound
	w = app.Test("GET", "/unknown", nil)
	if w.Code != 404 {
		t.Errorf("not found status = %d, want 404", w.Code)
	}
	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "not found" {
		t.Errorf("body = %v", body)
	}
}

func TestNotFoundDefaultBehavior(t *testing.T) {
	app := newTestApp()
	app.Get("/exists", func(ctx *testCtx) { ctx.String(200, "ok") })

	// Without NotFound handler, chi default 404
	w := app.Test("GET", "/unknown", nil)
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// --- StdMw (standard net/http decorators) ---

func TestMiddlewareOrder(t *testing.T) {
	app := newTestApp()
	var order []string

	app.UseStd(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "mw1-before")
			next.ServeHTTP(w, r)
			order = append(order, "mw1-after")
		})
	})
	app.UseStd(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "mw2-before")
			next.ServeHTTP(w, r)
			order = append(order, "mw2-after")
		})
	})
	app.Get("/", func(ctx *testCtx) {
		order = append(order, "handler")
		ctx.String(200, "ok")
	})

	app.Test("GET", "/", nil)

	want := "mw1-before,mw2-before,handler,mw2-after,mw1-after"
	got := strings.Join(order, ",")
	if got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestMiddlewareShortCircuit(t *testing.T) {
	app := newTestApp()
	app.UseStd(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			w.Write([]byte("blocked"))
		})
	})
	app.Get("/", func(ctx *testCtx) {
		t.Fatal("handler should not be called")
	})

	w := app.Test("GET", "/", nil)
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGroupMiddlewareIsolation(t *testing.T) {
	app := newTestApp()
	app.Group("/a", func(g *Cho[*testCtx]) {
		g.UseStd(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Group", "a")
				next.ServeHTTP(w, r)
			})
		})
		g.Get("/test", func(ctx *testCtx) { ctx.String(200, "a") })
	})
	app.Get("/b", func(ctx *testCtx) { ctx.String(200, "b") })

	// Group A should have the header
	w := app.Test("GET", "/a/test", nil)
	if w.Header().Get("X-Group") != "a" {
		t.Error("group middleware should apply to group routes")
	}

	// Route B should NOT have the header
	w = app.Test("GET", "/b", nil)
	if w.Header().Get("X-Group") != "" {
		t.Error("group middleware should not leak to parent routes")
	}
}

// --- Mount ---

type testController struct{}

func (c *testController) Get(ctx *testCtx)     { ctx.String(200, "list") }
func (c *testController) GetShow(ctx *testCtx) { ctx.String(200, "show:"+ctx.Query("id")) }
func (c *testController) PostCreate(ctx *testCtx) {
	ctx.String(201, "created")
}
func (c *testController) DeleteItem(ctx *testCtx) { ctx.String(200, "deleted") }

// This method should be SKIPPED (wrong param type)
func (c *testController) GetWrongParam(x int) {}

// --- Test helper ---

func TestTestMethod(t *testing.T) {
	app := newTestApp()
	app.Get("/ping", func(ctx *testCtx) { ctx.String(200, "pong") })

	w := app.Test("GET", "/ping", nil)
	if w.Code != 200 || w.Body.String() != "pong" {
		t.Errorf("Test() = %d %q", w.Code, w.Body.String())
	}
}

func TestTestMethodWithBody(t *testing.T) {
	app := newTestApp()
	app.Post("/echo", func(ctx *testCtx) {
		var v map[string]string
		if err := ctx.BindJson(&v); err != nil {
			ctx.Error(400, err.Error())
			return
		}
		ctx.Json(200, v)
	})

	body := strings.NewReader(`{"key":"value"}`)
	w := app.Test("POST", "/echo", body)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["key"] != "value" {
		t.Errorf("resp = %v", resp)
	}
}

// --- Start ---

func TestStartAndShutdown(t *testing.T) {
	app := newTestApp()
	app.Get("/", func(ctx *testCtx) { ctx.String(200, "ok") })

	srv, _, err := app.Start(0) // port 0 = random free port
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	resp, err := http.Get(fmt.Sprintf("http://%s/", srv.Addr))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestStartServeErrorPropagates(t *testing.T) {
	app := newTestApp()
	app.Get("/", func(ctx *testCtx) { ctx.String(200, "ok") })

	srv, errc, err := app.Start(0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	srv.Close()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve error = %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve result was not propagated to errc")
	}
}

// --- UseCtx: typed middleware ---

func TestUseCtxSetsAndReadsFields(t *testing.T) {
	app := newTestApp()
	app.UseCtx(func(ctx *testCtx, next http.Handler) {
		ctx.SetHeader("X-From-Ctx", "set-by-typed-mw")
		next.ServeHTTP(ctx.W, ctx.R)
	})
	app.Get("/", func(ctx *testCtx) { ctx.String(200, "ok") })

	w := app.Test("GET", "/", nil)
	if w.Header().Get("X-From-Ctx") != "set-by-typed-mw" {
		t.Error("typed middleware should have access to T")
	}
}

func TestUseCtxChainOrder(t *testing.T) {
	app := newTestApp()
	var order []string

	app.UseCtx(func(ctx *testCtx, next http.Handler) {
		order = append(order, "a-before")
		next.ServeHTTP(ctx.W, ctx.R)
		order = append(order, "a-after")
	})
	app.UseCtx(func(ctx *testCtx, next http.Handler) {
		order = append(order, "b-before")
		next.ServeHTTP(ctx.W, ctx.R)
		order = append(order, "b-after")
	})
	app.Get("/", func(ctx *testCtx) {
		order = append(order, "handler")
		ctx.String(200, "ok")
	})

	app.Test("GET", "/", nil)
	want := "a-before,b-before,handler,b-after,a-after"
	got := strings.Join(order, ",")
	if got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestUseCtxAndStandardMxMixed(t *testing.T) {
	app := newTestApp()
	var order []string

	app.UseStd(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "std-before")
			next.ServeHTTP(w, r)
			order = append(order, "std-after")
		})
	})
	app.UseCtx(func(ctx *testCtx, next http.Handler) {
		order = append(order, "typed-before")
		next.ServeHTTP(ctx.W, ctx.R)
		order = append(order, "typed-after")
	})
	app.Get("/", func(ctx *testCtx) {
		order = append(order, "handler")
		ctx.String(200, "ok")
	})

	app.Test("GET", "/", nil)
	want := "std-before,typed-before,handler,typed-after,std-after"
	got := strings.Join(order, ",")
	if got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestUseCtxShortCircuit(t *testing.T) {
	app := newTestApp()
	app.UseCtx(func(ctx *testCtx, next http.Handler) {
		ctx.String(401, "blocked")
	})
	app.Get("/", func(ctx *testCtx) { t.Fatal("handler should not be called") })

	w := app.Test("GET", "/", nil)
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}
