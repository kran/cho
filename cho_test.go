package cho

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	app.Get("/hello", func(ctx *testCtx) error {
		return ctx.String(200, "hello")
	})
	app.Post("/data", func(ctx *testCtx) error {
		return ctx.String(200, "posted")
	})
	app.Put("/data", func(ctx *testCtx) error {
		return ctx.String(200, "put")
	})
	app.Delete("/data", func(ctx *testCtx) error {
		return ctx.String(200, "deleted")
	})

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
		g.Get("/users", func(ctx *testCtx) error {
			return ctx.String(200, "users")
		})
		g.Group("/v2", func(g2 *Cho[*testCtx]) {
			g2.Get("/items", func(ctx *testCtx) error {
				return ctx.String(200, "items-v2")
			})
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

// --- Error Handling ---

func TestHandlerErrorHTTPError(t *testing.T) {
	app := newTestApp()
	app.Get("/err", func(ctx *testCtx) error {
		return NewHTTPError(http.StatusForbidden, "access denied")
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

func TestHandlerErrorGeneric(t *testing.T) {
	app := newTestApp()
	app.Get("/err", func(ctx *testCtx) error {
		return errors.New("some internal detail")
	})

	w := app.Test("GET", "/err", nil)
	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "internal server error" {
		t.Errorf("error = %q, want %q", body["error"], "internal server error")
	}
}

func TestCustomErrorHandler(t *testing.T) {
	app := newTestApp()
	app.ErrorHandler = func(ctx *testCtx, err error) {
		ctx.String(500, "custom: %s", err.Error())
	}
	app.Get("/err", func(ctx *testCtx) error {
		return errors.New("boom")
	})

	w := app.Test("GET", "/err", nil)
	if w.Body.String() != "custom: boom" {
		t.Errorf("body = %q, want %q", w.Body.String(), "custom: boom")
	}
}

func TestHTTPErrorDefaultMessage(t *testing.T) {
	e := NewHTTPError(404)
	if e.Message != "Not Found" {
		t.Errorf("message = %q, want %q", e.Message, "Not Found")
	}
	if e.Error() != "Not Found" {
		t.Errorf("Error() = %q, want %q", e.Error(), "Not Found")
	}
}

// --- NotFound ---

func TestNotFoundHandler(t *testing.T) {
	app := newTestApp()
	app.NotFound = func(ctx *testCtx) error {
		return ctx.Json(404, map[string]string{"error": "not found"})
	}
	app.Get("/exists", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

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
	app.Get("/exists", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	// Without NotFound handler, ServeMux default 404
	w := app.Test("GET", "/unknown", nil)
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// --- Middleware ---

func TestMiddlewareOrder(t *testing.T) {
	app := newTestApp()
	var order []string

	app.Use(func(ctx *testCtx, next Handler[*testCtx]) error {
		order = append(order, "mw1-before")
		err := next(ctx)
		order = append(order, "mw1-after")
		return err
	})
	app.Use(func(ctx *testCtx, next Handler[*testCtx]) error {
		order = append(order, "mw2-before")
		err := next(ctx)
		order = append(order, "mw2-after")
		return err
	})
	app.Get("/", func(ctx *testCtx) error {
		order = append(order, "handler")
		return ctx.String(200, "ok")
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
	app.Use(func(ctx *testCtx, next Handler[*testCtx]) error {
		return ctx.String(401, "blocked")
	})
	app.Get("/", func(ctx *testCtx) error {
		t.Fatal("handler should not be called")
		return nil
	})

	w := app.Test("GET", "/", nil)
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestMiddlewareAddedAfterRouteApplies(t *testing.T) {
	app := newTestApp()
	app.Get("/", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})
	// Middleware added AFTER route registration
	app.Use(func(ctx *testCtx, next Handler[*testCtx]) error {
		ctx.Res().Header().Set("X-After", "yes")
		return next(ctx)
	})

	w := app.Test("GET", "/", nil)
	if w.Header().Get("X-After") != "yes" {
		t.Error("middleware added after route should still apply")
	}
}

func TestGroupMiddlewareIsolation(t *testing.T) {
	app := newTestApp()
	app.Group("/a", func(g *Cho[*testCtx]) {
		g.Use(func(ctx *testCtx, next Handler[*testCtx]) error {
			ctx.Res().Header().Set("X-Group", "a")
			return next(ctx)
		})
		g.Get("/test", func(ctx *testCtx) error {
			return ctx.String(200, "a")
		})
	})
	app.Get("/b", func(ctx *testCtx) error {
		return ctx.String(200, "b")
	})

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

func TestMiddlewareErrorPropagation(t *testing.T) {
	app := newTestApp()
	var middlewareSawErr bool
	app.Use(func(ctx *testCtx, next Handler[*testCtx]) error {
		err := next(ctx)
		if err != nil {
			middlewareSawErr = true
		}
		return err
	})
	app.Get("/", func(ctx *testCtx) error {
		return NewHTTPError(400, "bad")
	})

	app.Test("GET", "/", nil)
	if !middlewareSawErr {
		t.Error("middleware should see the error returned by handler")
	}
}

// --- Mount ---

type testController struct{}

func (c *testController) Get(ctx *testCtx) error {
	return ctx.String(200, "list")
}

func (c *testController) GetShow(ctx *testCtx) error {
	return ctx.String(200, "show:%s", ctx.Query("id"))
}

func (c *testController) PostCreate(ctx *testCtx) error {
	return ctx.String(201, "created")
}

func (c *testController) DeleteItem(ctx *testCtx) error {
	return ctx.String(200, "deleted")
}

// This method should be SKIPPED (no error return)
func (c *testController) GetNoReturn(ctx *testCtx) {
}

// This method should be SKIPPED (wrong param)
func (c *testController) GetWrongParam(x int) error {
	return nil
}

func TestMount(t *testing.T) {
	app := newTestApp()
	app.Mount("/items", &testController{})

	tests := []struct {
		method, path string
		wantCode     int
		wantBody     string
	}{
		{"GET", "/items", 200, "list"},
		{"GET", "/items/show?id=42", 200, "show:42"},
		{"POST", "/items/create", 201, "created"},
		{"DELETE", "/items/item", 200, "deleted"},
	}
	for _, tt := range tests {
		w := app.Test(tt.method, tt.path, nil)
		if w.Code != tt.wantCode {
			t.Errorf("%s %s: status = %d, want %d", tt.method, tt.path, w.Code, tt.wantCode)
		}
		if w.Body.String() != tt.wantBody {
			t.Errorf("%s %s: body = %q, want %q", tt.method, tt.path, w.Body.String(), tt.wantBody)
		}
	}
}

func TestMountWithMiddleware(t *testing.T) {
	app := newTestApp()
	app.Use(func(ctx *testCtx, next Handler[*testCtx]) error {
		ctx.Res().Header().Set("X-MW", "applied")
		return next(ctx)
	})
	app.Mount("/ctrl", &testController{})

	w := app.Test("GET", "/ctrl", nil)
	if w.Header().Get("X-MW") != "applied" {
		t.Error("middleware should apply to mounted routes")
	}
}

type testMountError struct{}

func (c *testMountError) GetFail(ctx *testCtx) error {
	return NewHTTPError(503, "service unavailable")
}

func TestMountHandlerError(t *testing.T) {
	app := newTestApp()
	app.Mount("/svc", &testMountError{})

	w := app.Test("GET", "/svc/fail", nil)
	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// --- guardWriter ---

func TestGuardWriterPreventsDoubleHeader(t *testing.T) {
	w := httptest.NewRecorder()
	gw := &guardWriter{ResponseWriter: w}

	gw.WriteHeader(200)
	gw.WriteHeader(500) // should be silently ignored

	if w.Code != 200 {
		t.Errorf("code = %d, want 200 (second WriteHeader should be ignored)", w.Code)
	}
}

func TestGuardWriterWriteSetsCommitted(t *testing.T) {
	w := httptest.NewRecorder()
	gw := &guardWriter{ResponseWriter: w}

	gw.Write([]byte("hello"))
	gw.WriteHeader(500) // should be ignored since Write already committed

	// The default status when Write is called without WriteHeader is 200
	if w.Code != 200 {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

// --- Test helper ---

func TestTestMethod(t *testing.T) {
	app := newTestApp()
	app.Get("/ping", func(ctx *testCtx) error {
		return ctx.String(200, "pong")
	})

	w := app.Test("GET", "/ping", nil)
	if w.Code != 200 || w.Body.String() != "pong" {
		t.Errorf("Test() = %d %q", w.Code, w.Body.String())
	}
}

func TestTestMethodWithBody(t *testing.T) {
	app := newTestApp()
	app.Post("/echo", func(ctx *testCtx) error {
		var v map[string]string
		if err := ctx.BindJson(&v); err != nil {
			return NewHTTPError(400, err.Error())
		}
		return ctx.Json(200, v)
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
	app.Get("/", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	srv, err := app.Start(0) // port 0 = random free port
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	// Make a real HTTP request
	resp, err := http.Get(fmt.Sprintf("http://%s/", srv.Addr))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// --- Helpers ---

func TestCamelToKebab(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"UserInfo", "user-info"},
		{"ToggleStatus", "toggle-status"},
		{"ID", "id"},               // consecutive caps: no lowercase→uppercase boundary
		{"Login", "login"},
		{"ConvLabels", "conv-labels"},
		{"HTMLParser", "htmlparser"}, // all caps: no boundary detected
	}
	for _, tt := range tests {
		got := camelToKebab(tt.in)
		if got != tt.want {
			t.Errorf("camelToKebab(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseMethodName(t *testing.T) {
	tests := []struct {
		name       string
		wantMethod string
		wantPath   string
	}{
		{"Get", "GET", "/"},
		{"GetUsers", "GET", "/users"},
		{"PostLogin", "POST", "/login"},
		{"DeleteItem", "DELETE", "/item"},
		{"PutUserInfo", "PUT", "/user-info"},
		{"PatchStatus", "PATCH", "/status"},
		{"Invalid", "", ""},
		{"RunTask", "", ""},
	}
	for _, tt := range tests {
		method, path := parseMethodName(tt.name)
		if method != tt.wantMethod || path != tt.wantPath {
			t.Errorf("parseMethodName(%q) = (%q, %q), want (%q, %q)",
				tt.name, method, path, tt.wantMethod, tt.wantPath)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "/"},
		{"/", "/"},
		{"//", "/"},
		{"/a/b", "/a/b"},
		{"/a//b/", "/a/b"},
		{"a/b", "/a/b"},
	}
	for _, tt := range tests {
		got := normalizePath(tt.in)
		if got != tt.want {
			t.Errorf("normalizePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- Group inherits ErrorHandler ---

func TestGroupInheritsErrorHandler(t *testing.T) {
	app := newTestApp()
	app.ErrorHandler = func(ctx *testCtx, err error) {
		ctx.String(500, "custom-err")
	}
	app.Group("/api", func(g *Cho[*testCtx]) {
		g.Get("/fail", func(ctx *testCtx) error {
			return errors.New("boom")
		})
	})

	w := app.Test("GET", "/api/fail", nil)
	if w.Body.String() != "custom-err" {
		t.Errorf("body = %q, want %q", w.Body.String(), "custom-err")
	}
}
