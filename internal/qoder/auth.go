package qoder

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	qoderAPIBaseURL    = "https://api3.qoder.sh"
	qoderCenterBaseURL = "https://center.qoder.sh"
)

const openAPI = "https://openapi.qoder.sh"

type Credential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	UserID       string    `json:"user_id"`
	MachineID    string    `json:"machine_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type tokenResponse struct {
	Token        string          `json:"token"`
	RefreshToken string          `json:"refresh_token"`
	UserID       string          `json:"user_id"`
	ExpiresAt    json.RawMessage `json:"expires_at"`
	ExpiresIn    int64           `json:"expires_in"`
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// AtomicWrite keeps credentials and local keys private, including during replacement.
func AtomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func saveCredential(path string, c Credential) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return AtomicWrite(path, b)
}

func tokenExpiry(t tokenResponse) time.Time {
	var s string
	_ = json.Unmarshal(t.ExpiresAt, &s)
	if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return parsed
	}
	if s == "" {
		s = string(t.ExpiresAt)
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		if n > 10_000_000_000 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	return time.Now().Add(time.Hour) // Revalidate early when upstream omits expiry.
}

// Login never reads credentials from other applications. The browser completes PKCE authorization.
func Login(ctx context.Context, path string, showURL func(string)) error {
	return login(ctx, path, showURL, openAPI, 2*time.Second)
}

func login(ctx context.Context, path string, showURL func(string), api string, interval time.Duration) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(verifier))
	nonce, machine := randomID(), randomID()
	q := url.Values{"challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "challenge_method": {"S256"}, "nonce": {nonce}, "machine_id": {machine}}
	showURL("https://qoder.com/device/selectAccounts?" + q.Encode())
	q = url.Values{"nonce": {nonce}, "verifier": {verifier}, "challenge_method": {"S256"}}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 20 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api+"/api/v1/deviceToken/poll?"+q.Encode(), nil)
		req.Header.Set("User-Agent", "cc-qoder-bridge")
		resp, err := client.Do(req)
		if err != nil {
			return errors.New("Qoder authorization connection failed")
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == 202 || resp.StatusCode == 404 {
			continue
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("Qoder authorization returned HTTP %d", resp.StatusCode)
		}
		var t tokenResponse
		if err := json.Unmarshal(data, &t); err != nil {
			return errors.New("invalid Qoder authorization response")
		}
		if t.Token == "" || t.UserID == "" {
			return errors.New("Qoder authorization response has no token or user ID")
		}
		return saveCredential(path, Credential{t.Token, t.RefreshToken, t.UserID, machine, tokenExpiry(t)})
	}
}

type Store struct {
	Path       string
	Client     *http.Client
	RefreshURL string
	mu         sync.Mutex
}

// Token serializes refreshes. A concurrent 401 reuses credentials already refreshed by another request.
func (s *Store) Token(ctx context.Context, rejected string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return Credential{}, errors.New("no credential; run cc-qoder-bridge login")
	}
	var c Credential
	if json.Unmarshal(b, &c) != nil || c.AccessToken == "" || c.UserID == "" || c.MachineID == "" {
		return c, errors.New("invalid credential; run cc-qoder-bridge login")
	}
	if time.Now().Add(time.Minute).Before(c.ExpiresAt) && (rejected == "" || rejected != c.AccessToken) {
		return c, nil
	}
	if c.RefreshToken == "" {
		return c, errors.New("credential expired; run cc-qoder-bridge login")
	}
	body, _ := json.Marshal(map[string]string{"refreshToken": c.RefreshToken})
	endpoint := s.RefreshURL
	if endpoint == "" {
		endpoint = qoderRefreshURL()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return c, err
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cc-qoder-bridge")
	resp, err := s.Client.Do(req)
	if err != nil {
		return c, errors.New("Qoder credential refresh connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return c, fmt.Errorf("Qoder credential refresh returned HTTP %d; run login if persistent", resp.StatusCode)
	}
	var t tokenResponse
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t) != nil || t.Token == "" {
		return c, errors.New("invalid Qoder refresh response")
	}
	c.AccessToken, c.ExpiresAt = t.Token, tokenExpiry(t)
	if t.RefreshToken != "" {
		c.RefreshToken = t.RefreshToken
	}
	return c, saveCredential(s.Path, c)
}
