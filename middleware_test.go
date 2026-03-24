package cho

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecovery(t *testing.T) {
	app := newTestApp()
	app.Use(Recovery[*testCtx]())
	app.Get("/panic", func(ctx *testCtx) error {
		panic("boom")
	})

	w := app.Test("GET", "/panic", nil)
	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestRecoveryWithCallback(t *testing.T) {
	var recovered any
	app := newTestApp()
	app.Use(Recovery[*testCtx](func(r any) {
		recovered = r
	}))
	app.Get("/panic", func(ctx *testCtx) error {
		panic("test panic")
	})

	app.Test("GET", "/panic", nil)
	if recovered != "test panic" {
		t.Errorf("recovered = %v, want %q", recovered, "test panic")
	}
}

func TestRecoveryDoesNotAffectNormalHandlers(t *testing.T) {
	app := newTestApp()
	app.Use(Recovery[*testCtx]())
	app.Get("/ok", func(ctx *testCtx) error {
		return ctx.String(200, "fine")
	})

	w := app.Test("GET", "/ok", nil)
	if w.Code != 200 || w.Body.String() != "fine" {
		t.Errorf("got %d %q", w.Code, w.Body.String())
	}
}

func TestMaxBodySize(t *testing.T) {
	app := newTestApp()
	app.Use(MaxBodySize[*testCtx](10))
	app.Post("/", func(ctx *testCtx) error {
		_, err := io.ReadAll(ctx.Req().Body)
		if err != nil {
			return ctx.Error(413, "body too large")
		}
		return ctx.String(200, "ok")
	})

	// Small body should work
	w := app.Test("POST", "/", strings.NewReader("small"))
	if w.Code != 200 {
		t.Errorf("small body: status = %d, want 200", w.Code)
	}

	// Large body should fail
	w = app.Test("POST", "/", strings.NewReader("this body is way too large for the limit"))
	if w.Code != 413 {
		t.Errorf("large body: status = %d, want 413", w.Code)
	}
}

func TestCORSAllowAll(t *testing.T) {
	app := newTestApp()
	app.Use(CORS[*testCtx]())
	app.Get("/", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	// With Origin header
	w := app.Test("GET", "/", nil)
	// No origin in request → no CORS header
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("should not set CORS without Origin header")
	}

	// With Origin
	r := newReqWithOrigin("GET", "/", "http://example.com")
	w2 := sendReq(app, r)
	if w2.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("Allow-Origin = %q, want %q", w2.Header().Get("Access-Control-Allow-Origin"), "*")
	}
}

func TestCORSSpecificOrigin(t *testing.T) {
	app := newTestApp()
	app.Use(CORS[*testCtx]("http://allowed.com"))
	app.Get("/", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	// Allowed origin
	r := newReqWithOrigin("GET", "/", "http://allowed.com")
	w := sendReq(app, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "http://allowed.com" {
		t.Errorf("allowed origin: Allow-Origin = %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
	if w.Header().Get("Vary") != "Origin" {
		t.Error("should set Vary: Origin for specific origins")
	}

	// Disallowed origin
	r = newReqWithOrigin("GET", "/", "http://evil.com")
	w = sendReq(app, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("should not set Allow-Origin for disallowed origin")
	}
}

func TestCORSPreflight(t *testing.T) {
	app := newTestApp()
	app.Use(CORS[*testCtx]())
	app.Get("/api", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	// OPTIONS request should be handled by CORS middleware automatically,
	// without needing an explicit OPTIONS handler.
	r := newReqWithOrigin("OPTIONS", "/api", "http://example.com")
	r.Header.Set("Access-Control-Request-Headers", "Content-Type, Authorization")
	w := sendReq(app, r)

	if w.Code != 204 {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("missing Allow-Methods")
	}
	if w.Header().Get("Access-Control-Allow-Headers") != "Content-Type, Authorization" {
		t.Errorf("Allow-Headers = %q", w.Header().Get("Access-Control-Allow-Headers"))
	}
	if w.Header().Get("Access-Control-Max-Age") != "86400" {
		t.Errorf("Max-Age = %q", w.Header().Get("Access-Control-Max-Age"))
	}
}

func TestLogger(t *testing.T) {
	app := newTestApp()
	app.Use(Logger[*testCtx]("TEST"))
	app.Get("/ping", func(ctx *testCtx) error {
		return ctx.String(200, "pong")
	})

	w := app.Test("GET", "/ping", nil)
	if w.Code != 200 || w.Body.String() != "pong" {
		t.Errorf("Logger should not affect response: %d %q", w.Code, w.Body.String())
	}
}

func TestLoggerCapturesStatus(t *testing.T) {
	app := newTestApp()
	app.Use(Logger[*testCtx]())
	app.Get("/notfound", func(ctx *testCtx) error {
		return ctx.Error(404, "not found")
	})

	w := app.Test("GET", "/notfound", nil)
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestRequestIDGenerates(t *testing.T) {
	app := newTestApp()
	app.Use(RequestID[*testCtx]())
	app.Get("/", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	w := app.Test("GET", "/", nil)
	id := w.Header().Get("X-Request-ID")
	if id == "" {
		t.Error("X-Request-ID should be generated")
	}
	if len(id) != 16 { // 8 bytes hex = 16 chars
		t.Errorf("X-Request-ID = %q, expected 16 hex chars", id)
	}
}

func TestRequestIDPreservesExisting(t *testing.T) {
	app := newTestApp()
	app.Use(RequestID[*testCtx]())
	app.Get("/", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-ID", "my-custom-id")
	w := sendReq(app, r)

	if w.Header().Get("X-Request-ID") != "my-custom-id" {
		t.Errorf("X-Request-ID = %q, want %q", w.Header().Get("X-Request-ID"), "my-custom-id")
	}
}

func TestRequestIDUnique(t *testing.T) {
	app := newTestApp()
	app.Use(RequestID[*testCtx]())
	app.Get("/", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	w1 := app.Test("GET", "/", nil)
	w2 := app.Test("GET", "/", nil)
	id1 := w1.Header().Get("X-Request-ID")
	id2 := w2.Header().Get("X-Request-ID")
	if id1 == id2 {
		t.Errorf("request IDs should be unique, got %q twice", id1)
	}
}

func TestRateLimit(t *testing.T) {
	app := newTestApp()
	app.Use(RateLimit[*testCtx](100, 3)) // 100 rps, burst 3
	app.Get("/", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	// First 3 requests should succeed (burst)
	for i := 0; i < 3; i++ {
		w := app.Test("GET", "/", nil)
		if w.Code != 200 {
			t.Errorf("request %d: status = %d, want 200", i+1, w.Code)
		}
	}

	// 4th request should be rate limited
	w := app.Test("GET", "/", nil)
	if w.Code != 429 {
		t.Errorf("request 4: status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
}

func TestRateLimitCustomKey(t *testing.T) {
	app := newTestApp()
	// Rate limit by custom header, burst 1
	app.Use(RateLimit[*testCtx](1, 1, func(ctx *testCtx) string {
		return ctx.Req().Header.Get("X-API-Key")
	}))
	app.Get("/", func(ctx *testCtx) error {
		return ctx.String(200, "ok")
	})

	// Different keys should have independent limits
	r1 := httptest.NewRequest("GET", "/", nil)
	r1.Header.Set("X-API-Key", "key-a")
	w1 := sendReq(app, r1)
	if w1.Code != 200 {
		t.Errorf("key-a first request: status = %d, want 200", w1.Code)
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("X-API-Key", "key-b")
	w2 := sendReq(app, r2)
	if w2.Code != 200 {
		t.Errorf("key-b first request: status = %d, want 200", w2.Code)
	}

	// key-a should be limited now
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.Header.Set("X-API-Key", "key-a")
	w3 := sendReq(app, r3)
	if w3.Code != 429 {
		t.Errorf("key-a second request: status = %d, want 429", w3.Code)
	}
}

// --- helpers ---

func newReqWithOrigin(method, target, origin string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Origin", origin)
	return r
}

func sendReq(app *Cho[*testCtx], r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}
