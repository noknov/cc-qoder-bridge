package qoder

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Backend interface {
	Models(context.Context) ([]Model, error)
	Generate(context.Context, Request) (*http.Response, error)
}

type Server struct {
	Backend Backend
	Key     string
	Timeout time.Duration
	Slots   chan struct{}
}

func errorBody(err error) map[string]any {
	typ := "api_error"
	var input *InputError
	var upstream *UpstreamError
	if errors.As(err, &input) {
		typ = "invalid_request_error"
	}
	if errors.As(err, &upstream) {
		switch upstream.Status {
		case 401, 403:
			typ = "authentication_error"
		case 429:
			typ = "rate_limit_error"
		}
	}
	return map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": err.Error()}}
}

func sendJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func sendError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var input *InputError
	var upstream *UpstreamError
	if errors.As(err, &input) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	if errors.As(err, &upstream) {
		if upstream.Status >= 400 && upstream.Status < 500 {
			status = upstream.Status
		}
		if upstream.RetryAfter != "" {
			w.Header().Set("Retry-After", upstream.RetryAfter)
		}
	}
	sendJSON(w, status, errorBody(err))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The browser is not a client: reject cross-origin requests, including localhost origins.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "browser origins are not accepted", 403)
		return
	}
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		sendJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if key == "" {
		key = r.Header.Get("X-Api-Key")
	}
	if s.Key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(s.Key)) != 1 {
		sendJSON(w, 401, map[string]any{"type": "error", "error": map[string]string{"type": "authentication_error", "message": "invalid bridge key"}})
		return
	}
	select {
	case s.Slots <- struct{}{}:
		defer func() { <-s.Slots }()
	default:
		w.Header().Set("Retry-After", "1")
		sendJSON(w, 503, errorBody(errors.New("bridge is busy")))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
	defer cancel()
	r = r.WithContext(ctx)
	switch {
	case r.URL.Path == "/v1/models" && r.Method == "GET":
		models, err := s.Backend.Models(ctx)
		if err != nil {
			sendError(w, err)
			return
		}
		data := []map[string]any{}
		for _, m := range models {
			data = append(data, map[string]any{"id": m.ID, "type": "model", "display_name": m.Name, "created_at": "2026-01-01T00:00:00Z"})
		}
		var first, last string
		if len(models) > 0 {
			first, last = models[0].ID, models[len(models)-1].ID
		}
		sendJSON(w, 200, map[string]any{"data": data, "has_more": false, "first_id": first, "last_id": last})
	case r.URL.Path == "/v1/messages" && r.Method == "POST":
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
		if err != nil {
			sendJSON(w, 413, errorBody(&InputError{"request exceeds 16 MiB or body cannot be read"}))
			return
		}
		var request Request
		if json.Unmarshal(raw, &request) != nil || request.Model == "" {
			sendError(w, &InputError{"invalid JSON or missing model"})
			return
		}
		upstream, err := s.Backend.Generate(ctx, request)
		if err != nil {
			sendError(w, err)
			return
		}
		defer upstream.Body.Close()
		// Closing the client connection cancels both header wait and stream consumption.
		stop := context.AfterFunc(ctx, func() { upstream.Body.Close() })
		defer stop()
		var emit Emit
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			rc := http.NewResponseController(w)
			emit = func(event string, v any) error {
				data, err := json.Marshal(v)
				if err != nil {
					return err
				}
				_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
					return err
				}
				return rc.Flush()
			}
		}
		out, err := Translate(upstream.Body, request.Model, emit)
		if err != nil {
			if emit != nil {
				_ = emit("error", errorBody(err))
			} else {
				sendError(w, err)
			}
			return
		}
		if !request.Stream {
			sendJSON(w, 200, out)
		}
	case r.URL.Path == "/v1/messages/count_tokens" && r.Method == "POST":
		// No exact upstream tokenizer is available. Do not return invented token counts.
		sendJSON(w, 501, errorBody(errors.New("exact token counting is not supported")))
	default:
		sendJSON(w, 404, errorBody(&InputError{"unknown route or method"}))
	}
}
