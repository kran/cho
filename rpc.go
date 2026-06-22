package cho

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"reflect"
	"time"
)

const maxRpcBodySize = 4 << 20

var goContextType = reflect.TypeOf((*context.Context)(nil)).Elem()

type rpcEnvelope struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

type RpcClientOption struct {
	Client *http.Client
	Header http.Header
}

// MountRpc registers every exported method on impl whose last return value
// is error as a POST endpoint under /<pathPrefix>/<serviceName>/<MethodName>.
func (c *Cho[T]) MountRpc(pathPrefix string, serviceName string, impl any) {
	if impl == nil {
		panic(fmt.Sprintf("cho: MountRPC impl cannot be nil for service %s", serviceName))
	}

	val := reflect.ValueOf(impl)
	typ := val.Type()
	errType := reflect.TypeOf((*error)(nil)).Elem()

	for i := 0; i < typ.NumMethod(); i++ {
		method := typ.Method(i)
		mType := method.Type

		if mType.NumOut() < 1 || mType.Out(mType.NumOut()-1) != errType {
			continue
		}

		routePath := path.Join(pathPrefix, serviceName, method.Name)
		methodVal := val.Method(i)

		hasCtxParam := mType.NumIn() > 1 && mType.In(1) == goContextType
		jsonArgStart := 0
		if hasCtxParam {
			jsonArgStart = 1
		}
		expectedJsonArgs := mType.NumIn() - 1 - jsonArgStart

		handler := func(ctx T) {
			defer func() {
				if r := recover(); r != nil {
					c.replyRpc(ctx.Res(), 500, "internal server error")
				}
			}()

			body, err := io.ReadAll(io.LimitReader(ctx.Req().Body, maxRpcBodySize))
			ctx.Req().Body.Close()
			if err != nil {
				c.replyRpc(ctx.Res(), 400, "failed to read request body")
				return
			}

			var rawArgs []json.RawMessage
			if len(body) > 0 {
				if err := json.Unmarshal(body, &rawArgs); err != nil {
					c.replyRpc(ctx.Res(), 400, "invalid JSON array payload")
					return
				}
			}

			if len(rawArgs) != expectedJsonArgs {
				c.replyRpc(ctx.Res(), 400, fmt.Sprintf(
					"expected %d arguments, got %d", expectedJsonArgs, len(rawArgs)))
				return
			}

			callArgs := make([]reflect.Value, mType.NumIn()-1)
			if hasCtxParam {
				callArgs[0] = reflect.ValueOf(ctx.Req().Context())
			}

			for j := jsonArgStart; j < len(callArgs); j++ {
				argPtr := reflect.New(mType.In(j + 1))
				if err := json.Unmarshal(rawArgs[j-jsonArgStart], argPtr.Interface()); err != nil {
					c.replyRpc(ctx.Res(), 400, fmt.Sprintf("argument %d: %v", j-jsonArgStart, err))
					return
				}
				callArgs[j] = argPtr.Elem()
			}

			results := methodVal.Call(callArgs)

			errVal := results[len(results)-1]
			if !errVal.IsNil() {
				c.replyRpc(ctx.Res(), 500, "internal server error")
				return
			}

			numRes := len(results) - 1
			resList := make([]any, numRes)
			for j := 0; j < numRes; j++ {
				resList[j] = results[j].Interface()
			}

			dataBytes, _ := json.Marshal(resList)
			ctx.Res().Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(ctx.Res()).Encode(rpcEnvelope{Data: dataBytes})
		}

		c.Handle(http.MethodPost, routePath, handler)
	}
}

// MakeRpcClient populates function-typed fields of targetStructPtr so that
// each call performs an HTTP POST to the corresponding RPC endpoint.
func MakeRpcClient(baseURL, serviceName string, targetStructPtr any, opts ...RpcClientOption) {
	val := reflect.ValueOf(targetStructPtr)
	if val.Kind() != reflect.Ptr || val.Elem().Kind() != reflect.Struct {
		panic("cho: MakeRpcClient targetStructPtr must be a pointer to a struct")
	}

	var opt RpcClientOption
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.Client == nil {
		opt.Client = &http.Client{Timeout: 30 * time.Second}
	}

	structVal := val.Elem()
	structType := structVal.Type()
	errType := reflect.TypeOf((*error)(nil)).Elem()

	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if field.Type.Kind() != reflect.Func {
			continue
		}

		numOut := field.Type.NumOut()
		if numOut < 1 || !field.Type.Out(numOut-1).Implements(errType) {
			continue
		}

		url := fmt.Sprintf("%s/%s/%s", baseURL, serviceName, field.Name)
		hasCtxParam := field.Type.NumIn() > 0 && field.Type.In(0) == goContextType

		fn := reflect.MakeFunc(field.Type, func(in []reflect.Value) []reflect.Value {
			out := make([]reflect.Value, numOut)

			handleErr := func(err error) []reflect.Value {
				for j := 0; j < len(out)-1; j++ {
					out[j] = reflect.Zero(field.Type.Out(j))
				}
				out[len(out)-1] = reflect.ValueOf(err).Convert(errType)
				return out
			}

			reqCtx := context.Background()
			argsStart := 0
			if hasCtxParam {
				reqCtx = in[0].Interface().(context.Context)
				argsStart = 1
			}

			reqArgs := make([]any, len(in)-argsStart)
			for j := argsStart; j < len(in); j++ {
				reqArgs[j-argsStart] = in[j].Interface()
			}

			reqBytes, err := json.Marshal(reqArgs)
			if err != nil {
				return handleErr(fmt.Errorf("marshal request args: %w", err))
			}

			req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(reqBytes))
			if err != nil {
				return handleErr(fmt.Errorf("create request: %w", err))
			}
			req.Header.Set("Content-Type", "application/json")
			for k, vs := range opt.Header {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}

			resp, err := opt.Client.Do(req)
			if err != nil {
				return handleErr(err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return handleErr(fmt.Errorf("read response body: %w", err))
			}

			var envelope rpcEnvelope
			if err := json.Unmarshal(body, &envelope); err != nil {
				return handleErr(fmt.Errorf("rpc error %d: %s", resp.StatusCode, string(body)))
			}
			if envelope.Error != "" {
				return handleErr(fmt.Errorf("rpc: %s", envelope.Error))
			}

			var rawResults []json.RawMessage
			if len(envelope.Data) > 0 {
				if err := json.Unmarshal(envelope.Data, &rawResults); err != nil {
					return handleErr(fmt.Errorf("invalid rpc response data: %w", err))
				}
			}

			for j := 0; j < len(out)-1; j++ {
				if j < len(rawResults) {
					ptr := reflect.New(field.Type.Out(j))
					if err := json.Unmarshal(rawResults[j], ptr.Interface()); err != nil {
						return handleErr(fmt.Errorf("unmarshal result %d: %w", j, err))
					}
					out[j] = ptr.Elem()
				} else {
					out[j] = reflect.Zero(field.Type.Out(j))
				}
			}
			out[len(out)-1] = reflect.Zero(errType)

			return out
		})

		structVal.Field(i).Set(fn)
	}
}

func (c *Cho[T]) replyRpc(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rpcEnvelope{Error: msg})
}
