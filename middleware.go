package cho

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
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

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// SetRes allows middleware to swap the underlying response writer (e.g. for status capture).
func (b *BaseContext) SetRes(w http.ResponseWriter) {
	b.W = w
}
