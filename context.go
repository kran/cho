package cho

import (
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/schema"
)

var formDecoder = schema.NewDecoder()

// Context is the interface that all Cho handler contexts must implement.
type Context interface {
	Req() *http.Request
	Res() http.ResponseWriter
}

// Validatable can be implemented by request structs to provide custom validation logic.
// If a bound struct implements this interface, Validate is called automatically after binding.
type Validatable interface {
	Validate() error
}

// BaseContext is the default Context implementation. Embed it in your own context struct
// to satisfy the Context interface and gain helper methods.
type BaseContext struct {
	W              http.ResponseWriter
	R              *http.Request
	validator      func(any) error
	trustedProxies []*net.IPNet
}

// SetValidator configures a validation function that runs automatically
// after BindJson, BindQuery, and BindForm.
func (b *BaseContext) SetValidator(fn func(any) error) {
	b.validator = fn
}

// SetTrustedProxies configures which proxy IPs are trusted for
// X-Forwarded-For and X-Real-IP header parsing in RemoteIP().
func (b *BaseContext) SetTrustedProxies(nets []*net.IPNet) {
	b.trustedProxies = nets
}

func (b *BaseContext) isTrusted(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range b.trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
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

func (b *BaseContext) Req() *http.Request       { return b.R }
func (b *BaseContext) Res() http.ResponseWriter { return b.W }

func (b *BaseContext) PathValue(key string) string {
	return b.R.PathValue(key)
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

// Cookie returns a named cookie from the request.
func (b *BaseContext) Cookie(name string) (*http.Cookie, error) {
	return b.R.Cookie(name)
}

// SetCookie adds a Set-Cookie header to the response.
func (b *BaseContext) SetCookie(cookie *http.Cookie) {
	http.SetCookie(b.W, cookie)
}

// RemoteIP returns the client IP address.
// If TrustedProxies is configured and the direct peer is trusted,
// it walks X-Forwarded-For from right to left, returning the first
// untrusted IP. Otherwise it returns RemoteAddr directly (safe default).
func (b *BaseContext) RemoteIP() string {
	host, _, _ := net.SplitHostPort(b.R.RemoteAddr)

	// No trusted proxies configured — safe default, ignore headers
	if len(b.trustedProxies) == 0 {
		return host
	}

	// Direct peer not trusted — return RemoteAddr
	if !b.isTrusted(host) {
		return host
	}

	// Check X-Forwarded-For: walk from right to left, skip trusted IPs
	if xff := b.R.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := strings.TrimSpace(parts[i])
			if !b.isTrusted(ip) {
				return ip
			}
		}
	}

	// Check X-Real-IP
	if xri := b.R.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	return host
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

// QueryInt64 returns a query parameter parsed as int64, or 0 if missing/invalid.
func (b *BaseContext) QueryInt64(key string) int64 {
	v := b.R.URL.Query().Get(key)
	if v == "" {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
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
		if err := b.R.ParseMultipartForm(32 << 20); err != nil {
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

func (b *BaseContext) String(status int, format string, values ...any) error {
	b.W.Header().Set("Content-Type", "text/plain; charset=utf-8")
	b.W.WriteHeader(status)
	_, err := fmt.Fprintf(b.W, format, values...)
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
	var mu sync.Mutex

	// Keep-alive goroutine
	var ticker *time.Ticker
	if len(keepAlive) > 0 && keepAlive[0] > 0 {
		ticker = time.NewTicker(keepAlive[0])
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-done:
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
		mu.Lock()
		defer mu.Unlock()
		if ticker != nil {
			ticker.Reset(keepAlive[0])
		}
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
