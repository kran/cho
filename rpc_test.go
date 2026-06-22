package cho

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// --- Test RPC service ---

type MathService struct{}

func (s *MathService) Add(a, b int) (int, error) {
	return a + b, nil
}

func (s *MathService) Divide(a, b int) (float64, error) {
	if b == 0 {
		return 0, errors.New("division by zero")
	}
	return float64(a) / float64(b), nil
}

func (s *MathService) Greet(ctx context.Context, name string) (string, error) {
	return "hello " + name, nil
}

func (s *MathService) Panic() error {
	panic("rpc panic")
}

// NoError has no error return — should be skipped
func (s *MathService) NoError() string {
	return "skip"
}

func TestRpcBasic(t *testing.T) {
	app := newTestApp()
	app.MountRpc("/rpc", "math", &MathService{})

	// Add(2, 3) = 5
	w := app.Test("POST", "/rpc/math/Add", strings.NewReader("[2, 3]"))
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var env rpcEnvelope
	json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error != "" {
		t.Fatalf("rpc error: %s", env.Error)
	}

	var results []json.RawMessage
	json.Unmarshal(env.Data, &results)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	var sum int
	json.Unmarshal(results[0], &sum)
	if sum != 5 {
		t.Errorf("Add(2,3) = %d, want 5", sum)
	}
}

func TestRpcWithContext(t *testing.T) {
	app := newTestApp()
	app.MountRpc("/rpc", "math", &MathService{})

	w := app.Test("POST", "/rpc/math/Greet", strings.NewReader(`["world"]`))
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var env rpcEnvelope
	json.Unmarshal(w.Body.Bytes(), &env)
	var results []json.RawMessage
	json.Unmarshal(env.Data, &results)
	var greeting string
	json.Unmarshal(results[0], &greeting)
	if greeting != "hello world" {
		t.Errorf("Greet = %q, want %q", greeting, "hello world")
	}
}

func TestRpcErrorDoesNotLeak(t *testing.T) {
	app := newTestApp()
	app.MountRpc("/rpc", "math", &MathService{})

	w := app.Test("POST", "/rpc/math/Divide", strings.NewReader("[1, 0]"))
	if w.Code != 500 {
		t.Fatalf("status = %d", w.Code)
	}

	var env rpcEnvelope
	json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error != "internal server error" {
		t.Errorf("error = %q, should not leak 'division by zero'", env.Error)
	}
}

func TestRpcWrongArgCount(t *testing.T) {
	app := newTestApp()
	app.MountRpc("/rpc", "math", &MathService{})

	w := app.Test("POST", "/rpc/math/Add", strings.NewReader("[1]"))
	if w.Code != 400 {
		t.Fatalf("status = %d", w.Code)
	}

	var env rpcEnvelope
	json.Unmarshal(w.Body.Bytes(), &env)
	if !strings.Contains(env.Error, "expected 2") {
		t.Errorf("error = %q", env.Error)
	}
}

func TestRpcInvalidJson(t *testing.T) {
	app := newTestApp()
	app.MountRpc("/rpc", "math", &MathService{})

	w := app.Test("POST", "/rpc/math/Add", strings.NewReader("{invalid"))
	if w.Code != 400 {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestRpcPanicRecovery(t *testing.T) {
	app := newTestApp()
	app.MountRpc("/rpc", "math", &MathService{})

	w := app.Test("POST", "/rpc/math/Panic", strings.NewReader("[]"))
	if w.Code != 500 {
		t.Fatalf("status = %d", w.Code)
	}

	var env rpcEnvelope
	json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error != "internal server error" {
		t.Errorf("error = %q", env.Error)
	}
}

func TestRpcEmptyBody(t *testing.T) {
	app := newTestApp()
	app.MountRpc("/rpc", "math", &MathService{})

	// Panic() takes no args, so empty body should work
	w := app.Test("POST", "/rpc/math/Panic", nil)
	// It panics and recovers, returning 500
	if w.Code != 500 {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestRpcNilImpl(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MountRpc(nil) should panic")
		}
	}()
	app := newTestApp()
	app.MountRpc("/rpc", "test", nil)
}

func TestRpcClientRoundTrip(t *testing.T) {
	app := newTestApp()
	app.MountRpc("/rpc", "math", &MathService{})

	srv, _, err := app.Start(0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close()

	type MathClient struct {
		Add    func(a, b int) (int, error)
		Divide func(a, b int) (float64, error)
		Greet  func(ctx context.Context, name string) (string, error)
	}

	var client MathClient
	baseURL := "http://" + srv.Addr + "/rpc"
	MakeRpcClient(baseURL, "math", &client, RpcClientOption{
		Client: &http.Client{},
	})

	// Add
	sum, err := client.Add(10, 20)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum != 30 {
		t.Errorf("Add(10,20) = %d, want 30", sum)
	}

	// Divide
	result, err := client.Divide(10, 3)
	if err != nil {
		t.Fatalf("Divide: %v", err)
	}
	if result < 3.33 || result > 3.34 {
		t.Errorf("Divide(10,3) = %f", result)
	}

	// Divide by zero → error (generic message from server)
	_, err = client.Divide(1, 0)
	if err == nil {
		t.Error("Divide(1,0) should return error")
	}

	// Greet with context
	greeting, err := client.Greet(context.Background(), "cho")
	if err != nil {
		t.Fatalf("Greet: %v", err)
	}
	if greeting != "hello cho" {
		t.Errorf("Greet = %q", greeting)
	}
}

func TestRpcClientWithHeaders(t *testing.T) {
	app := newTestApp()

	type HeaderService struct{}
	type headerSvc struct{}

	app.MountRpc("/rpc", "headers", &headerSvc{})

	// We can't easily test custom headers without inspecting the request
	// on the server side, so we just verify MakeRpcClient doesn't panic
	// with custom headers.
	type Client struct {
		// No matching methods, but that's fine
	}
	var client Client
	MakeRpcClient("http://localhost:9999/rpc", "headers", &client, RpcClientOption{
		Header: http.Header{"X-Custom": []string{"value"}},
	})
}

func TestMakeRpcClientPanicsOnNonPointer(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MakeRpcClient(non-pointer) should panic")
		}
	}()
	type Client struct{}
	var c Client
	MakeRpcClient("http://localhost", "svc", c) // not a pointer
}
