package qoder

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginPKCEAndAtomicCredentialStorage(t *testing.T) {
	var browser *url.URL
	var polls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		q := r.URL.Query()
		hash := sha256.Sum256([]byte(q.Get("verifier")))
		if q.Get("nonce") != browser.Query().Get("nonce") || base64.RawURLEncoding.EncodeToString(hash[:]) != browser.Query().Get("challenge") {
			t.Error("PKCE challenge or nonce mismatch")
		}
		if polls == 1 {
			w.WriteHeader(202)
			return
		}
		fmt.Fprint(w, `{"token":"secret","refresh_token":"refresh","user_id":"user","expires_in":3600}`)
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "private", "credential.json")
	err := login(context.Background(), path, func(u string) { browser, _ = url.Parse(u) }, upstream.URL, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var credential Credential
	if json.Unmarshal(data, &credential) != nil || credential.AccessToken != "secret" || credential.MachineID != browser.Query().Get("machine_id") {
		t.Fatal("invalid stored credential")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 || polls != 2 {
		t.Fatal("credential mode or poll count")
	}
}

func TestTruncatedToolNeverEmitsExecutableBlock(t *testing.T) {
	stream := sse(toolDelta("call", "read", `{"path":"a.go"}`, "length"))
	_, err := Translate(strings.NewReader(stream), "test-model", func(_ string, value any) error {
		data, _ := json.Marshal(value)
		if strings.Contains(string(data), `"type":"tool_use"`) {
			t.Error("truncated tool emitted")
		}
		return nil
	})
	if err == nil {
		t.Fatal("truncation succeeded")
	}
}

func TestNestedQueueIsRateLimitNotAuthenticationFailure(t *testing.T) {
	queue, _ := json.Marshal(map[string]any{"isQueued": true, "retryAfterSeconds": 30})
	nested, _ := json.Marshal(map[string]any{"message": string(queue)})
	envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 403, "body": string(nested)})
	_, err := Translate(strings.NewReader("data: "+string(envelope)+"\n\n"), "test-model", nil)
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.Status != 429 || upstream.RetryAfter != "30" {
		t.Fatal(err)
	}
}

func sse(parts ...any) string {
	var out strings.Builder
	for _, part := range parts {
		b, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": part})
		fmt.Fprintf(&out, "data: %s\n\n", b)
	}
	return out.String()
}
func delta(content, finish string) any {
	return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}, "finish_reason": finish}}}
}
func toolDelta(id, name, args, finish string) any {
	return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": id, "function": map[string]any{"name": name, "arguments": args}}}}, "finish_reason": finish}}}
}

func TestTranslateTextToolsAndUsage(t *testing.T) {
	stream := sse(delta("你", ""), delta("好", ""), toolDelta("call_1", "read_file", `{"pa`, ""), toolDelta("", "", `th":"a.go"}`, "tool_calls"), map[string]any{"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 12, "prompt_tokens_details": map[string]int{"cached_tokens": 20}}})
	var names []string
	out, err := Translate(strings.NewReader(stream), "test-model", func(event string, v any) error { names = append(names, event); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != "tool_use" || out.Usage.Input != 80 || out.Usage.CacheRead != 20 || out.Usage.Output != 12 {
		t.Fatalf("unexpected result: %+v", out)
	}
	if len(out.Content) != 2 || out.Content[0]["text"] != "你好" || out.Content[1]["name"] != "read_file" {
		t.Fatalf("content: %v", out.Content)
	}
	args := out.Content[1]["input"].(map[string]any)
	if args["path"] != "a.go" {
		t.Fatal(args)
	}
	if names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Fatal(names)
	}
}

func TestTranslateDoesNotSucceedOnTruncationOrInvalidTools(t *testing.T) {
	for name, stream := range map[string]string{
		"truncated":    sse(delta("partial", "")),
		"bad envelope": "data: {bad}\n\n",
		"bad tool":     sse(toolDelta("call", "read", "{", "tool_calls")),
		"missing tool": sse(delta("", "tool_calls")),
		"queue":        "data: {" + `"statusCodeValue":429,"body":{"message":"queued"}` + "}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			stopped := false
			_, err := Translate(strings.NewReader(stream), "test-model", func(event string, _ any) error { stopped = stopped || event == "message_stop"; return nil })
			if err == nil || stopped {
				t.Fatalf("err=%v, success=%v", err, stopped)
			}
		})
	}
}

func TestConvertToolRoundTripAndRejectUnsupportedContent(t *testing.T) {
	var r Request
	raw := `{"model":"test-model","max_tokens":128,"system":"system","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"read","input":{"path":"a.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":[{"type":"text","text":"source"}]},{"type":"text","text":"continue"}]}],"tools":[{"name":"read","input_schema":{"type":"object"}}]}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	body, err := Convert(r, Model{ID: "test-model", Config: map[string]any{"source": "system"}})
	if err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	if len(messages) != 4 || messages[2].(map[string]any)["role"] != "tool" {
		t.Fatal(messages)
	}
	if messages[2].(map[string]any)["tool_call_id"] != "call" {
		t.Fatal(messages[2])
	}
	r.Messages = []Message{{Role: "user", Content: json.RawMessage(`[{"type":"document","source":{}}]`)}}
	if _, err := Convert(r, Model{ID: "test-model"}); err == nil {
		t.Fatal("document silently discarded")
	}
	r.Messages = []Message{{Role: "user", Content: json.RawMessage(`"hello"`)}}
	r.ToolChoice = json.RawMessage(`{"type":"any"}`)
	if _, err := Convert(r, Model{ID: "test-model"}); err == nil {
		t.Fatal("unsupported tool choice accepted")
	}
}

func TestProtocolEncodingAndSignature(t *testing.T) {
	for _, raw := range []string{"", "a", "hello", `{"text":"你好"}`} {
		encoded := qoderEncodeBody([]byte(raw))
		var b strings.Builder
		for _, ch := range []byte(encoded) {
			if ch == '$' {
				b.WriteByte('=')
				continue
			}
			i := strings.IndexByte(qoderCustomBase64, ch)
			if i < 0 {
				t.Fatalf("unexpected byte %c", ch)
			}
			b.WriteByte(qoderStandardBase64[i])
		}
		x := b.String()
		n := len(x) / 3
		decoded, err := base64.StdEncoding.DecodeString(x[len(x)-n:] + x[n:len(x)-n] + x[:n])
		if err != nil || string(decoded) != raw {
			t.Fatalf("%q => %q: %v", raw, decoded, err)
		}
	}
	body := []byte("test")
	h, err := qoderBuildAuthHeaders(body, "https://api3.qoder.sh/algo/api/v2/model/list?Encode=1", qoderSigningCredential{userID: "user", accessToken: "never-log-me", machineID: "device"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range h {
		if strings.Contains(strings.Join(v, ""), "never-log-me") {
			t.Fatal("plaintext credential in signature headers")
		}
	}
	auth := strings.Split(strings.TrimPrefix(h.Get("Authorization"), "Bearer COSY."), ".")
	sum := md5.Sum([]byte(auth[0] + "\n" + h.Get("Cosy-Key") + "\n" + h.Get("Cosy-Date") + "\n" + string(body) + "\n/api/v2/model/list"))
	if auth[1] != hex.EncodeToString(sum[:]) {
		t.Fatal("signature mismatch")
	}
}

func TestConcurrentRefreshIsSingleFlight(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer old" {
			t.Error("incorrect refresh authorization")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "new", "refresh_token": "refresh-new", "expires_in": 3600})
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := saveCredential(path, Credential{"old", "refresh", "user", "device", time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	store := &Store{Path: path, Client: upstream.Client(), RefreshURL: upstream.URL}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := store.Token(context.Background(), "old")
			if err != nil || c.AccessToken != "new" {
				t.Errorf("%v %v", c.AccessToken, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh count %d", calls.Load())
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
}

func TestClientDiscoversAndSignsRequestWithoutModelFallback(t *testing.T) {
	var catalogCalls, chatCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
			t.Error("missing signature")
		}
		switch r.URL.Path {
		case "/algo/api/v2/model/list":
			catalogCalls.Add(1)
			fmt.Fprint(w, `{"chat":[{"key":"model-exact","display_name":"Test Model","enable":true,"source":"system"}]}`)
		default:
			chatCalls.Add(1)
			if r.Header.Get("X-Model-Key") != "model-exact" {
				t.Error("wrong upstream model")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, sse(delta("hello", "stop")))
		}
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "credential.json")
	_ = saveCredential(path, Credential{"token", "refresh", "user", "device", time.Now().Add(time.Hour)})
	client := NewClient(path)
	client.APIURL = upstream.URL
	r := Request{Model: "model-exact", MaxTokens: 128, Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	resp, err := client.Generate(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := Translate(resp.Body, r.Model, nil)
	if err != nil || out.Content[0]["text"] != "hello" {
		t.Fatalf("%+v %v", out, err)
	}
	r.Model = "claude-unknown"
	if _, err := client.Generate(context.Background(), r); err == nil {
		t.Fatal("unknown model accepted")
	}
	if catalogCalls.Load() != 1 || chatCalls.Load() != 1 {
		t.Fatal("cache or rejection failed")
	}
}

type fakeBackend struct {
	stream   string
	canceled chan struct{}
}

func (f *fakeBackend) Models(context.Context) ([]Model, error) {
	return []Model{{ID: "test-model", Name: "Test Model"}}, nil
}
func (f *fakeBackend) Generate(ctx context.Context, r Request) (*http.Response, error) {
	if f.canceled != nil {
		<-ctx.Done()
		close(f.canceled)
		return nil, ctx.Err()
	}
	return &http.Response{Body: io.NopCloser(strings.NewReader(f.stream))}, nil
}
func testServer(b Backend) *Server {
	return &Server{Backend: b, Key: "local-key", Timeout: time.Second, Slots: make(chan struct{}, 2)}
}

func TestServerAuthenticationOriginAndStreamingErrors(t *testing.T) {
	s := testServer(&fakeBackend{stream: sse(delta("partial", ""))})
	for _, tc := range []struct {
		key, origin, path, method string
		status                    int
	}{
		{"", "", "/healthz", "GET", 200}, {"", "", "/v1/models", "GET", 401}, {"local-key", "https://evil.example", "/v1/models", "GET", 403},
		{"local-key", "", "/v1/models", "GET", 200}, {"local-key", "", "/v1/messages/count_tokens", "POST", 501},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("X-Api-Key", tc.key)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s => %d", tc.path, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"test-model","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	r.Header.Set("X-Api-Key", "local-key")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: message_stop") {
		t.Fatal(w.Body.String())
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	b := &fakeBackend{canceled: make(chan struct{})}
	s := testServer(b)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"test-model","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	r.Header.Set("X-Api-Key", "local-key")
	done := make(chan struct{})
	go func() { s.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	cancel()
	select {
	case <-b.canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream not canceled")
	}
	<-done
}

func TestServerProducesBothAnthropicResponseFormats(t *testing.T) {
	for _, stream := range []bool{false, true} {
		s := testServer(&fakeBackend{stream: sse(delta("hello", "stop"))})
		raw := fmt.Sprintf(`{"model":"test-model","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, stream)
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw))
		r.Header.Set("Authorization", "Bearer local-key")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if stream {
			if !strings.Contains(w.Body.String(), "event: message_stop") || !strings.Contains(w.Body.String(), `"stop_reason":null`) {
				t.Fatal(w.Body.String())
			}
		} else {
			var response Response
			if json.Unmarshal(w.Body.Bytes(), &response) != nil || response.StopReason != "end_turn" || response.Content[0]["text"] != "hello" {
				t.Fatal(w.Body.String())
			}
		}
	}
}

func TestClientRefreshes401ButDoesNotReplayGenerationFailures(t *testing.T) {
	var calls, refreshes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refresh" {
			refreshes.Add(1)
			fmt.Fprint(w, `{"token":"new","expires_in":3600}`)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(503)
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "credential.json")
	_ = saveCredential(path, Credential{"old", "refresh", "user", "device", time.Now().Add(time.Hour)})
	client := NewClient(path)
	client.APIURL = upstream.URL
	client.Store.RefreshURL = upstream.URL + "/refresh"
	_, err := client.call(context.Background(), "POST", "/generate", []byte("encoded"), &Model{ID: "test-model", Config: map[string]any{}})
	var failure *UpstreamError
	if !errors.As(err, &failure) || failure.Status != 503 || failure.RetryAfter != "30" || calls.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("err=%v calls=%d refreshes=%d", err, calls.Load(), refreshes.Load())
	}
}

func TestMalformedParallelToolCallDoesNotEmitAnyTool(t *testing.T) {
	event := map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "first", "function": map[string]string{"name": "Read", "arguments": "{}"}},
			map[string]any{"index": 1, "id": "second", "function": map[string]string{"name": "Edit", "arguments": "{"}},
		}}, "finish_reason": "tool_calls",
	}}}
	emitted := false
	_, err := Translate(strings.NewReader(sse(event)), "test-model", func(_ string, value any) error {
		data, _ := json.Marshal(value)
		if strings.Contains(string(data), `"type":"tool_use"`) {
			emitted = true
		}
		return nil
	})
	if err == nil {
		t.Fatal("malformed arguments accepted")
	}
	if emitted {
		t.Fatal("a tool was exposed before all parallel tool arguments were validated")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientPreservesDeadlineForGatewayTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := saveCredential(path, Credential{"token", "refresh", "user", "device", time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client := NewClient(path)
	client.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := client.call(ctx, "GET", "/catalog", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline cause was lost: %v", err)
	}
	w := httptest.NewRecorder()
	sendError(w, err)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d, want 504", w.Code)
	}
}
