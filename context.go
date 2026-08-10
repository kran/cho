package cho

import (
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi/v5"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/schema"
)

var formDecoder = func() *schema.Decoder {
	d := schema.NewDecoder()
	// Tolerate extra query/form keys not present in the target struct
	// (e.g. cache-busting params) instead of failing the whole bind.
	d.IgnoreUnknownKeys(true)
	return d
}()

// Validatable can be implemented by request structs to provide custom validation logic.
// If a bound struct implements this interface, Validate is called automatically after binding.
type Validatable interface {
	Validate() error
}

// BaseContext is the default request context. Embed it in your own context
// struct to gain request/response access (W/R fields) and the helper methods.
type BaseContext struct {
	W         http.ResponseWriter
	R         *http.Request
	validator func(any) error

	// MaxFormMemory limits multipart form parsing in BindForm; 0 = 32 MB default.
	MaxFormMemory int64
}

// SetValidator configures a validation function that runs automatically
// after BindJson, BindQuery, and BindForm.
func (b *BaseContext) SetValidator(fn func(any) error) {
	b.validator = fn
}

func (b *BaseContext) runValidation(v any) error {
	if va, ok := v.(Validatable); ok {
		if err := va.Validate(); err != nil {
			return err
		}
	}
	if b.validator != nil {
		return b.validator(v)
	}
	return nil
}

func MakeBaseContext(writer http.ResponseWriter, request *http.Request) *BaseContext {
	return &BaseContext{W: writer, R: request}
}

// SetResponseWriter 更新 W (CtxFrom 在每个访问点同步当前值 — 中间件
// 包装后的形态)。实现 CtxIface。
func (b *BaseContext) SetResponseWriter(w http.ResponseWriter) { b.W = w }

// SetRequest 更新 R (CtxFrom 在每个访问点同步当前值 — WithContext 派生
// 后的形态)。实现 CtxIface。
func (b *BaseContext) SetRequest(r *http.Request) { b.R = r }

// PathValue 路径参数 — 从 chi 的 RouteContext 读 (参数的真正来源)。
// 不用 net/http 的 r.PathValue: chi 5.2+ 靠 routeHTTP 里的 SetPathValue
// 同步, 在 inline (With) 路由 + 中间件场景下同步有失效时序问题
// (实测: chi URLParams 有值但 r.PathValue 为空)。
func (b *BaseContext) PathValue(key string) string {
	rctx := chi.RouteContext(b.R.Context())
	if rctx == nil {
		return ""
	}
	for i, k := range rctx.URLParams.Keys {
		if k == key {
			return rctx.URLParams.Values[i]
		}
	}
	return ""
}

func (b *BaseContext) Query(key string) string {
	return b.R.URL.Query().Get(key)
}

func (b *BaseContext) Form(key string) string {
	return b.R.FormValue(key)
}

// Header returns a request header value.
func (b *BaseContext) Header(key string) string {
	return b.R.Header.Get(key)
}

// SetHeader sets a response header.
func (b *BaseContext) SetHeader(key, value string) {
	b.W.Header().Set(key, value)
}

// AddHeader adds a response header.
func (b *BaseContext) AddHeader(key, value string) {
	b.W.Header().Add(key, value)
}

// Cookie returns a named cookie from the request.
func (b *BaseContext) Cookie(name string) (*http.Cookie, error) {
	return b.R.Cookie(name)
}

// SetCookie adds a Set-Cookie header to the response.
func (b *BaseContext) SetCookie(cookie *http.Cookie) {
	http.SetCookie(b.W, cookie)
}

// Method returns the HTTP method of the request.
func (b *BaseContext) Method() string {
	return b.R.Method
}

// Path returns the URL path of the request.
func (b *BaseContext) Path() string {
	return b.R.URL.Path
}

// Redirect sends an HTTP redirect.
func (b *BaseContext) Redirect(status int, url string) {
	http.Redirect(b.W, b.R, url, status)
}

// ServeFile serves a file from the filesystem.
func (b *BaseContext) ServeFile(filepath string) {
	http.ServeFile(b.W, b.R, filepath)
}

// QueryInt64 parses the query parameter as int64. When the parameter is
// absent, def (or 0) is returned. A present-but-empty or unparsable value
// yields 0 — Has() distinguishes "absent" from "explicitly passed".
func (b *BaseContext) QueryInt64(key string, def ...int64) int64 {
	q := b.R.URL.Query()
	if !q.Has(key) {
		if len(def) > 0 {
			return def[0]
		}
		return 0
	}
	n, _ := strconv.ParseInt(q.Get(key), 10, 64)
	return n
}

// QueryInt is QueryInt64 for int. Delegates to QueryInt64 — the parse/absent
// semantics live in one place. On 32-bit platforms an out-of-range value
// truncates instead of yielding 0 (documented platform difference).
func (b *BaseContext) QueryInt(key string, def ...int) int {
	def64 := make([]int64, len(def))
	for i, d := range def {
		def64[i] = int64(d)
	}
	return int(b.QueryInt64(key, def64...))
}

func (b *BaseContext) BindJson(v any) error {
	defer b.R.Body.Close()
	if err := json.NewDecoder(b.R.Body).Decode(v); err != nil {
		return fmt.Errorf("bind json error: %w", err)
	}
	return b.runValidation(v)
}

// BindQuery decodes URL query parameters into v using `schema` struct tags.
func (b *BaseContext) BindQuery(v any) error {
	if err := formDecoder.Decode(v, b.R.URL.Query()); err != nil {
		return fmt.Errorf("bind query error: %w", err)
	}
	return b.runValidation(v)
}

// FormFile returns the first uploaded file for the given field name.
// Must be called after BindForm (or after manually calling r.ParseMultipartForm).
func (b *BaseContext) FormFile(key string) (multipart.File, *multipart.FileHeader, error) {
	return b.R.FormFile(key)
}

// FormFiles returns all uploaded files for the given field name.
func (b *BaseContext) FormFiles(key string) ([]*multipart.FileHeader, error) {
	if b.R.MultipartForm == nil {
		return nil, fmt.Errorf("multipart form not parsed")
	}
	return b.R.MultipartForm.File[key], nil
}

// BindForm decodes form body (application/x-www-form-urlencoded or multipart/form-data)
// into v using `schema` struct tags. For multipart/form-data, use a 32 MB default memory limit.
func (b *BaseContext) BindForm(v any) error {
	ct := b.R.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		limit := b.MaxFormMemory
		if limit <= 0 {
			limit = 32 << 20
		}
		if err := b.R.ParseMultipartForm(limit); err != nil {
			return fmt.Errorf("bind form error: %w", err)
		}
	} else {
		if err := b.R.ParseForm(); err != nil {
			return fmt.Errorf("bind form error: %w", err)
		}
	}
	if err := formDecoder.Decode(v, b.R.Form); err != nil {
		return fmt.Errorf("bind form error: %w", err)
	}
	return b.runValidation(v)
}

func (b *BaseContext) Json(status int, v any) error {
	b.W.Header().Set("Content-Type", "application/json; charset=utf-8")
	b.W.WriteHeader(status)
	return json.NewEncoder(b.W).Encode(v)
}

// String writes plain text verbatim. Any '%' is literal — no format
// interpretation, so user-supplied content is safe. For formatting, build the
// string with fmt.Sprintf and pass it here.
func (b *BaseContext) String(status int, s string) error {
	b.W.Header().Set("Content-Type", "text/plain; charset=utf-8")
	b.W.WriteHeader(status)
	_, err := b.W.Write([]byte(s))
	return err
}

func (b *BaseContext) Error(status int, msg string) error {
	return b.Json(status, map[string]string{"error": msg})
}

func (b *BaseContext) NoContent(status int) error {
	b.W.WriteHeader(status)
	return nil
}

// SSE sets up a Server-Sent Events stream. The send callback writes an event
// to the client. The stream flushes after each event and closes when fn returns
// or the client disconnects. If keepAlive > 0, periodic keep-alive comments
// are sent to prevent proxy/client timeouts.
func (b *BaseContext) SSE(fn func(send func(event, data string)), keepAlive ...time.Duration) {
	flusher, ok := b.W.(http.Flusher)
	if !ok {
		b.W.WriteHeader(http.StatusInternalServerError)
		return
	}

	b.W.Header().Set("Content-Type", "text/event-stream")
	b.W.Header().Set("Cache-Control", "no-cache")
	b.W.Header().Set("Connection", "keep-alive")
	b.W.WriteHeader(http.StatusOK)
	flusher.Flush()

	done := b.R.Context().Done()
	stop := make(chan struct{})
	defer close(stop) // stop the keep-alive goroutine when fn returns
	var mu sync.Mutex

	// Keep-alive goroutine — exits on client disconnect OR handler return
	// (writing to W after the handler returned is undefined behavior).
	var ticker *time.Ticker
	if len(keepAlive) > 0 && keepAlive[0] > 0 {
		ticker = time.NewTicker(keepAlive[0])
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-stop:
					return
				case <-ticker.C:
					mu.Lock()
					fmt.Fprint(b.W, ": keepalive\n\n")
					flusher.Flush()
					mu.Unlock()
				}
			}
		}()
	}

	fn(func(event, data string) {
		select {
		case <-done:
			return
		default:
		}
		// Reset outside the lock: Ticker.Reset is concurrency-safe and does not
		// touch W — the mutex only guards W writes (event vs keep-alive).
		// A keep-alive racing in after a reset is harmless (SSE clients ignore it).
		if ticker != nil {
			ticker.Reset(keepAlive[0])
		}
		mu.Lock()
		defer mu.Unlock()
		if event != "" {
			fmt.Fprintf(b.W, "event: %s\n", event)
		}
		for _, line := range strings.Split(data, "\n") {
			fmt.Fprintf(b.W, "data: %s\n", line)
		}
		fmt.Fprint(b.W, "\n")
		flusher.Flush()
	})
}
