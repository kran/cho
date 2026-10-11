package cho

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
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

func TestQueryNum(t *testing.T) {
	ctx, _ := makeCtx("GET", "/test?id=42&bad=abc&empty=&zero=0", "")
	if ctx.QueryNum("id") != 42 {
		t.Errorf("QueryNum(id) = %d", ctx.QueryNum("id"))
	}
	if ctx.QueryNum("bad") != 0 {
		t.Errorf("QueryNum(bad) = %d", ctx.QueryNum("bad"))
	}
	if ctx.QueryNum("missing") != 0 {
		t.Errorf("QueryNum(missing) = %d", ctx.QueryNum("missing"))
	}
	// default applies only when absent — not for empty/zero
	if ctx.QueryNum("missing", 7) != 7 {
		t.Errorf("QueryNum(missing, 7) = %d", ctx.QueryNum("missing", 7))
	}
	if ctx.QueryNum("empty", 7) != 0 {
		t.Errorf("QueryNum(empty, 7) = %d", ctx.QueryNum("empty", 7))
	}
	if ctx.QueryNum("zero", 7) != 0 {
		t.Errorf("QueryNum(zero, 7) = %d", ctx.QueryNum("zero", 7))
	}
}

func TestPathNum(t *testing.T) {
	app := newTestApp()
	app.Get("/users/{id}", func(ctx *testCtx) {
		ctx.String(200, fmt.Sprintf("%d", ctx.PathNum("id")))
	})
	app.Get("/users/{id}/posts/{pid}", func(ctx *testCtx) {
		ctx.String(200, fmt.Sprintf("%d", ctx.PathNum("pid")))
	})

	if w := app.Test("GET", "/users/42", nil); w.Body.String() != "42" {
		t.Errorf("PathNum(id) = %q, want 42", w.Body.String())
	}
	if w := app.Test("GET", "/users/42/posts/7", nil); w.Body.String() != "7" {
		t.Errorf("PathNum(pid) = %q, want 7", w.Body.String())
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
	err := ctx.String(201, "hello world")
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

	type person struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	v, err := ctx.BindJson[person]()
	if err != nil {
		t.Fatalf("BindJson() error: %v", err)
	}
	if v.Name != "alice" || v.Age != 30 {
		t.Errorf("got %+v", v)
	}
}

func TestBindJsonInvalid(t *testing.T) {
	ctx, _ := makeCtx("POST", "/", `{invalid}`)
	if _, err := ctx.BindJson[map[string]string](); err == nil {
		t.Error("BindJson() should return error for invalid JSON")
	}
}

// bindTarget pins the generic binder contract: T is the value type (no *T
// parameter), the result is *T, and Validate runs on the returned value.
type bindTarget struct {
	Name string `json:"name"`
}

func (b *bindTarget) Validate() error {
	if b.Name == "" {
		return errors.New("name required")
	}
	return nil
}

func TestBindJsonReturnsTypedValue(t *testing.T) {
	ctx, _ := makeCtx("POST", "/", `{"name":"bob"}`)
	v, err := ctx.BindJson[bindTarget]()
	if err != nil {
		t.Fatalf("BindJson[bindTarget]() error: %v", err)
	}
	if v.Name != "bob" {
		t.Errorf("got %+v", v)
	}
}

func TestBindJsonRunsValidation(t *testing.T) {
	ctx, _ := makeCtx("POST", "/", `{}`)
	if _, err := ctx.BindJson[bindTarget](); err == nil {
		t.Error("BindJson should surface the Validate() error")
	}
}

func TestBindFormUrlencoded(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader("name=alice&age=30"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := &BaseContext{W: w, R: r}

	v, err := ctx.BindForm[struct {
		Name string `schema:"name"`
		Age  int    `schema:"age"`
	}]()
	if err != nil {
		t.Fatalf("BindForm() error: %v", err)
	}
	if v.Name != "alice" || v.Age != 30 {
		t.Errorf("got %+v", v)
	}
}

func TestBindFormMultipart(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("name", "alice"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("avatar", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("filedata")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	ctx := &BaseContext{W: w, R: r}

	v, err := ctx.BindForm[struct {
		Name string `schema:"name"`
	}]()
	if err != nil {
		t.Fatalf("BindForm() error: %v", err)
	}
	if v.Name != "alice" {
		t.Errorf("Name = %q, want alice", v.Name)
	}

	f, fh, err := ctx.FormFile("avatar")
	if err != nil {
		t.Fatalf("FormFile() error: %v", err)
	}
	defer f.Close()
	if fh.Filename != "a.txt" {
		t.Errorf("Filename = %q, want a.txt", fh.Filename)
	}
	files, err := ctx.FormFiles("avatar")
	if err != nil || len(files) != 1 {
		t.Errorf("FormFiles() = %v, %v", files, err)
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
	app.Get("/events", func(ctx *testCtx) {
		ctx.SSE(func(send func(event, data string)) {
			send("msg", "hello")
			send("", "world")
			send("multi", "line1\nline2")
		})
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

func TestMakeBaseContext(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	ctx := MakeBaseContext(w, r)
	if ctx.R != r || ctx.W != w {
		t.Error("MakeBaseContext didn't set fields correctly")
	}
}

func TestBindQueryIgnoresUnknownKeys(t *testing.T) {
	ctx, _ := makeCtx("GET", "/?name=alice&_cacheBust=123", "")
	v, err := ctx.BindQuery[struct {
		Name string `schema:"name"`
	}]()
	if err != nil {
		t.Fatalf("BindQuery should ignore unknown keys, got: %v", err)
	}
	if v.Name != "alice" {
		t.Errorf("Name = %q, want alice", v.Name)
	}
}

func TestStringNoFormatInjection(t *testing.T) {
	ctx, w := makeCtx("GET", "/", "")
	verbatim := "100% off, %s and %d untouched"
	if err := ctx.String(200, verbatim); err != nil {
		t.Fatalf("String() error: %v", err)
	}
	if w.Body.String() != verbatim {
		t.Errorf("body = %q, want verbatim %q", w.Body.String(), verbatim)
	}
}
