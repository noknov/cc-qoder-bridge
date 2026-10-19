package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type UpstreamError struct {
	Status     int
	RetryAfter string
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("Qoder returned HTTP %d", e.Status) }

type Client struct {
	HTTP   *http.Client
	Store  *Store
	APIURL string
	mu     sync.Mutex
	models []Model
	loaded time.Time
}

func NewClient(credentialPath string) *Client {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 45 * time.Second, IdleConnTimeout: 90 * time.Second,
		MaxIdleConns: 16, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 16}
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{HTTP: httpClient, Store: &Store{Path: credentialPath, Client: &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: httpClient.CheckRedirect}}}
}

func (c *Client) call(ctx context.Context, method, path string, body []byte, model *Model) (*http.Response, error) {
	credential, err := c.Store.Token(ctx, "")
	if err != nil {
		return nil, err
	}
	base := c.APIURL
	if base == "" {
		base = qoderAPIBaseURL
	}
	endpoint := strings.TrimRight(base, "/") + path
	for attempt := 0; attempt < 2; attempt++ {
		headers, err := qoderBuildAuthHeaders(body, endpoint, qoderSigningCredential{userID: credential.UserID, accessToken: credential.AccessToken, machineID: credential.MachineID})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = headers
		req.Header.Set("User-Agent", "cc-qoder-bridge")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "identity")
		if model != nil {
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("X-Model-Key", model.ID)
			source, _ := model.Config["source"].(string)
			if source == "" {
				source = "system"
			}
			req.Header.Set("X-Model-Source", source)
		} else {
			req.Header.Set("Accept", "application/json")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				return nil, context.DeadlineExceeded
			}
			return nil, errors.New("Qoder connection failed")
		}
		if resp.StatusCode == 401 && attempt == 0 {
			resp.Body.Close()
			credential, err = c.Store.Token(ctx, credential.AccessToken)
			if err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, &UpstreamError{resp.StatusCode, resp.Header.Get("Retry-After")}
		}
		return resp, nil
	}
	return nil, errors.New("Qoder authentication failed")
}

// Models caches only successful catalogs; discovery failures never invent model IDs.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.loaded) < 5*time.Minute && len(c.models) > 0 {
		return c.models, nil
	}
	resp, err := c.call(ctx, http.MethodGet, "/algo/api/v2/model/list?Encode=1", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var catalog struct {
		Chat []map[string]any `json:"chat"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&catalog); err != nil {
		return nil, errors.New("invalid Qoder model catalog")
	}
	models := []Model{}
	seen := map[string]bool{}
	for _, entry := range catalog.Chat {
		id, _ := entry["key"].(string)
		if id == "" || entry["enable"] != true || seen[id] {
			continue
		}
		seen[id] = true
		name, _ := entry["display_name"].(string)
		if name == "" {
			name = id
		}
		models = append(models, Model{ID: id, Name: name, Config: entry})
	}
	if len(models) == 0 {
		return nil, errors.New("Qoder returned no enabled models")
	}
	c.models, c.loaded = models, time.Now()
	return models, nil
}

func (c *Client) Generate(ctx context.Context, r Request) (*http.Response, error) {
	models, err := c.Models(ctx)
	if err != nil {
		return nil, err
	}
	var selected *Model
	for i := range models {
		if models[i].ID == r.Model {
			selected = &models[i]
			break
		}
	}
	if selected == nil {
		return nil, &InputError{"unknown model; select an exact ID from /v1/models"}
	}
	body, err := Convert(r, *selected)
	if err != nil {
		return nil, &InputError{err.Error()}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.call(ctx, http.MethodPost, "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1", []byte(qoderEncodeBody(raw)), selected)
}

type InputError struct{ Message string }

func (e *InputError) Error() string { return e.Message }
