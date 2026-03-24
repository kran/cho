package cho

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Recovery returns a middleware that recovers from panics in handlers.
func Recovery[T Context](onPanic ...func(r any)) Middleware[T] {
	return func(ctx T, next Handler[T]) (retErr error) {
		defer func() {
			if r := recover(); r != nil {
				if len(onPanic) > 0 && onPanic[0] != nil {
					onPanic[0](r)
				}
				ctx.Res().WriteHeader(http.StatusInternalServerError)
				retErr = nil
			}
		}()
		return next(ctx)
	}
}

// MaxBodySize returns a middleware that limits the request body size.
func MaxBodySize[T Context](limit int64) Middleware[T] {
	return func(ctx T, next Handler[T]) error {
		ctx.Req().Body = http.MaxBytesReader(ctx.Res(), ctx.Req().Body, limit)
		return next(ctx)
	}
}

// CORS returns a middleware that sets CORS headers. Pass allowed origins,
// or "*" to allow all. Preflight OPTIONS requests are handled automatically.
func CORS[T Context](allowedOrigins ...string) Middleware[T] {
	allowAll := len(allowedOrigins) == 0 || (len(allowedOrigins) == 1 && allowedOrigins[0] == "*")

	return func(ctx T, next Handler[T]) error {
		r := ctx.Req()
		w := ctx.Res()

		origin := r.Header.Get("Origin")
		if origin == "" {
			return next(ctx)
		}

		if allowAll {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			matched := false
			for _, o := range allowedOrigins {
				if o == origin {
					matched = true
					break
				}
			}
			if !matched {
				return next(ctx)
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}

		// Preflight
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", r.Header.Get("Access-Control-Request-Headers"))
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return nil
		}

		return next(ctx)
	}
}

// Logger returns a middleware that logs each request with method, path,
// status code, and duration. An optional prefix is prepended to each log line.
func Logger[T Context](prefix ...string) Middleware[T] {
	p := ""
	if len(prefix) > 0 {
		p = prefix[0] + " "
	}
	return func(ctx T, next Handler[T]) error {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: ctx.Res()}

		if bc, ok := any(ctx).(interface{ SetRes(http.ResponseWriter) }); ok {
			bc.SetRes(sw)
			defer bc.SetRes(sw.ResponseWriter)
		}

		err := next(ctx)

		status := sw.status
		if status == 0 {
			status = 200
		}
		log.Printf("%s%s %s %d %s", p, ctx.Req().Method, ctx.Req().URL.Path, status, time.Since(start).Round(time.Microsecond))
		return err
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// RequestID returns a middleware that assigns a unique request ID to each
// request via the X-Request-ID header. If the incoming request already
// carries one, it is preserved.
func RequestID[T Context]() Middleware[T] {
	return func(ctx T, next Handler[T]) error {
		id := ctx.Req().Header.Get("X-Request-ID")
		if id == "" {
			id = generateID()
		}
		ctx.Res().Header().Set("X-Request-ID", id)
		return next(ctx)
	}
}

// Timeout returns a middleware that cancels the request context after d.
// Handlers should respect ctx.Req().Context().Done() for early exit.
func Timeout[T Context](d time.Duration) Middleware[T] {
	return func(ctx T, next Handler[T]) error {
		reqCtx, cancel := context.WithTimeout(ctx.Req().Context(), d)
		defer cancel()
		if rs, ok := any(ctx).(interface{ SetReq(*http.Request) }); ok {
			rs.SetReq(ctx.Req().WithContext(reqCtx))
		}
		return next(ctx)
	}
}

// RateLimit returns a middleware that limits requests using a token bucket algorithm.
// rps is the refill rate (requests per second), burst is the max tokens (allows short bursts).
// An optional keyFunc extracts the rate limit key from the context; defaults to client IP.
func RateLimit[T Context](rps float64, burst int, keyFunc ...func(T) string) Middleware[T] {
	rl := &rateLimiter{
		visitors: make(map[string]*visitor),
		rps:      rps,
		burst:    float64(burst),
	}
	go rl.cleanup(3 * time.Minute)

	var getKey func(T) string
	if len(keyFunc) > 0 && keyFunc[0] != nil {
		getKey = keyFunc[0]
	} else {
		getKey = func(ctx T) string {
			host, _, _ := net.SplitHostPort(ctx.Req().RemoteAddr)
			return host
		}
	}

	return func(ctx T, next Handler[T]) error {
		if !rl.allow(getKey(ctx)) {
			retryAfter := 1.0 / rps
			ctx.Res().Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter))))
			return NewHTTPError(http.StatusTooManyRequests, fmt.Sprintf("rate limit exceeded, try again in %.0fs", retryAfter))
		}
		return next(ctx)
	}
}

type visitor struct {
	tokens   float64
	lastSeen time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	rps      float64
	burst    float64
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	v, exists := rl.visitors[key]
	if !exists {
		rl.visitors[key] = &visitor{tokens: rl.burst - 1, lastSeen: now}
		return true
	}

	// Refill tokens based on elapsed time
	v.tokens += now.Sub(v.lastSeen).Seconds() * rl.rps
	if v.tokens > rl.burst {
		v.tokens = rl.burst
	}
	v.lastSeen = now

	if v.tokens < 1 {
		return false
	}
	v.tokens--
	return true
}

func (rl *rateLimiter) cleanup(maxAge time.Duration) {
	for {
		time.Sleep(time.Minute)
		rl.mu.Lock()
		now := time.Now()
		for k, v := range rl.visitors {
			if now.Sub(v.lastSeen) > maxAge {
				delete(rl.visitors, k)
			}
		}
		rl.mu.Unlock()
	}
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// SetRes allows middleware to swap the underlying response writer (e.g. for status capture).
func (b *BaseContext) SetRes(w http.ResponseWriter) {
	b.W = w
}

// SetReq allows middleware to swap the underlying request (e.g. for context propagation).
func (b *BaseContext) SetReq(r *http.Request) {
	b.R = r
}
