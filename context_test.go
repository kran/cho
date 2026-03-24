package cho

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func makeCtx(method, path string, body string) (*BaseContext, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	return &BaseContext{W: w, R: r}, w
}

func TestQuery(t *testing.T) {
	ctx, _ := makeCtx("GET", "/test?name=alice&age=30", "")
	if ctx.Query("name") != "alice" {
		t.Errorf("Query(name) = %q", ctx.Query("name"))
	}
	if ctx.Query("missing") != "" {
		t.Errorf("Query(missing) = %q", ctx.Query("missing"))
	}
}

func TestQueryInt64(t *testing.T) {
	ctx, _ := makeCtx("GET", "/test?id=42&bad=abc", "")
	if ctx.QueryInt64("id") != 42 {
		t.Errorf("QueryInt64(id) = %d", ctx.QueryInt64("id"))
	}
	if ctx.QueryInt64("bad") != 0 {
		t.Errorf("QueryInt64(bad) = %d", ctx.QueryInt64("bad"))
	}
	if ctx.QueryInt64("missing") != 0 {
		t.Errorf("QueryInt64(missing) = %d", ctx.QueryInt64("missing"))
	}
}

func TestHeaderAndSetHeader(t *testing.T) {
	ctx, w := makeCtx("GET", "/", "")
	ctx.R.Header.Set("X-Custom", "hello")
	if ctx.Header("X-Custom") != "hello" {
		t.Errorf("Header = %q", ctx.Header("X-Custom"))
	}

	ctx.SetHeader("X-Response", "world")
	if w.Header().Get("X-Response") != "world" {
		t.Errorf("SetHeader didn't set response header")
	}
}

func TestRemoteIPSafeDefault(t *testing.T) {
	// Without trusted proxies, headers are ignored
	ctx, _ := makeCtx("GET", "/", "")
	ctx.R.Header.Set("X-Forwarded-For", "1.2.3.4")
	ctx.R.RemoteAddr = "9.9.9.9:1234"
	if got := ctx.RemoteIP(); got != "9.9.9.9" {
		t.Errorf("RemoteIP() = %q, want %q (should ignore X-Forwarded-For)", got, "9.9.9.9")
	}
}

func TestRemoteIPTrustedProxy(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	_, proxy2, _ := net.ParseCIDR("9.9.9.9/32")

	tests := []struct {
		name, xff, xri, remoteAddr, want string
	}{
		{"XFF single hop", "1.2.3.4", "", "10.0.0.1:1234", "1.2.3.4"},
		{"XFF multi hop, skip trusted", "1.2.3.4, 10.0.0.2", "", "10.0.0.1:1234", "1.2.3.4"},
		{"XFF spoofed + real", "spoofed, 5.6.7.8, 10.0.0.2", "", "10.0.0.1:1234", "5.6.7.8"},
		{"X-Real-IP fallback", "", "2.3.4.5", "10.0.0.1:1234", "2.3.4.5"},
		{"Untrusted peer ignores headers", "1.2.3.4", "", "99.99.99.99:1234", "99.99.99.99"},
		{"RemoteAddr only", "", "", "10.0.0.1:5678", "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := makeCtx("GET", "/", "")
			ctx.SetTrustedProxies([]*net.IPNet{trusted, proxy2})
			if tt.xff != "" {
				ctx.R.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				ctx.R.Header.Set("X-Real-IP", tt.xri)
			}
			ctx.R.RemoteAddr = tt.remoteAddr
			if got := ctx.RemoteIP(); got != tt.want {
				t.Errorf("RemoteIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMethodAndPath(t *testing.T) {
	ctx, _ := makeCtx("POST", "/api/users", "")
	if ctx.Method() != "POST" {
		t.Errorf("Method() = %q", ctx.Method())
	}
	if ctx.Path() != "/api/users" {
		t.Errorf("Path() = %q", ctx.Path())
	}
}

func TestJson(t *testing.T) {
	ctx, w := makeCtx("GET", "/", "")
	err := ctx.Json(200, map[string]int{"count": 5})
	if err != nil {
		t.Fatalf("Json() error: %v", err)
	}
	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var resp map[string]int
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["count"] != 5 {
		t.Errorf("body = %v", resp)
	}
}

func TestString(t *testing.T) {
	ctx, w := makeCtx("GET", "/", "")
	err := ctx.String(201, "hello %s", "world")
	if err != nil {
		t.Fatalf("String() error: %v", err)
	}
	if w.Code != 201 {
		t.Errorf("status = %d", w.Code)
	}
	if w.Body.String() != "hello world" {
		t.Errorf("body = %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestError(t *testing.T) {
	ctx, w := makeCtx("GET", "/", "")
	ctx.Error(400, "bad request")
	if w.Code != 400 {
		t.Errorf("status = %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "bad request" {
		t.Errorf("body = %v", resp)
	}
}

func TestNoContent(t *testing.T) {
	ctx, w := makeCtx("GET", "/", "")
	ctx.NoContent(204)
	if w.Code != 204 {
		t.Errorf("status = %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", w.Body.String())
	}
}

func TestBindJson(t *testing.T) {
	ctx, _ := makeCtx("POST", "/", `{"name":"alice","age":30}`)
	ctx.R.Header.Set("Content-Type", "application/json")

	var v struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	if err := ctx.BindJson(&v); err != nil {
		t.Fatalf("BindJson() error: %v", err)
	}
	if v.Name != "alice" || v.Age != 30 {
		t.Errorf("got %+v", v)
	}
}

func TestBindJsonInvalid(t *testing.T) {
	ctx, _ := makeCtx("POST", "/", `{invalid}`)
	var v map[string]string
	if err := ctx.BindJson(&v); err == nil {
		t.Error("BindJson() should return error for invalid JSON")
	}
}

func TestCookie(t *testing.T) {
	ctx, w := makeCtx("GET", "/", "")
	ctx.R.AddCookie(&http.Cookie{Name: "session", Value: "abc123"})

	cookie, err := ctx.Cookie("session")
	if err != nil || cookie.Value != "abc123" {
		t.Errorf("Cookie() = %v, %v", cookie, err)
	}

	ctx.SetCookie(&http.Cookie{Name: "token", Value: "xyz"})
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "token=xyz") {
		t.Errorf("Set-Cookie = %q", setCookie)
	}
}

func TestRedirect(t *testing.T) {
	ctx, w := makeCtx("GET", "/old", "")
	ctx.Redirect(http.StatusFound, "/new")
	if w.Code != 302 {
		t.Errorf("status = %d", w.Code)
	}
	if w.Header().Get("Location") != "/new" {
		t.Errorf("Location = %q", w.Header().Get("Location"))
	}
}

func TestSSEBasic(t *testing.T) {
	app := newTestApp()
	app.Get("/events", func(ctx *testCtx) error {
		ctx.SSE(func(send func(event, data string)) {
			send("msg", "hello")
			send("", "world")
			send("multi", "line1\nline2")
		})
		return nil
	})

	w := app.Test("GET", "/events", nil)
	body := w.Body.String()

	if !strings.Contains(body, "event: msg\ndata: hello\n\n") {
		t.Errorf("missing event msg, body:\n%s", body)
	}
	if !strings.Contains(body, "data: world\n\n") {
		t.Errorf("missing unnamed event, body:\n%s", body)
	}
	if !strings.Contains(body, "event: multi\ndata: line1\ndata: line2\n\n") {
		t.Errorf("missing multiline event, body:\n%s", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestSetRes(t *testing.T) {
	ctx, _ := makeCtx("GET", "/", "")
	w2 := httptest.NewRecorder()
	ctx.SetRes(w2)
	if ctx.Res() != w2 {
		t.Error("SetRes didn't change the response writer")
	}
}

func TestMakeBaseContext(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	ctx := MakeBaseContext(w, r)
	if ctx.Req() != r || ctx.Res() != w {
		t.Error("MakeBaseContext didn't set fields correctly")
	}
}
