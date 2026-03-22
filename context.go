package cho

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Context is the interface that all Cho handler contexts must implement.
type Context interface {
	Req() *http.Request
	Res() http.ResponseWriter
}

// BaseContext is the default Context implementation. Embed it in your own context struct
// to satisfy the Context interface and gain helper methods.
type BaseContext struct {
	W http.ResponseWriter
	R *http.Request
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

// RemoteIP returns the client IP, checking X-Forwarded-For and X-Real-IP first.
func (b *BaseContext) RemoteIP() string {
	if ip := b.R.Header.Get("X-Forwarded-For"); ip != "" {
		if i := strings.IndexByte(ip, ','); i > 0 {
			return strings.TrimSpace(ip[:i])
		}
		return ip
	}
	if ip := b.R.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	host, _, _ := net.SplitHostPort(b.R.RemoteAddr)
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
	return nil
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
