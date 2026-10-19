package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cc-qoder-bridge/internal/qoder"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Println("Usage: cc-qoder-bridge <login|models|serve|claude-env> [options]")
		return nil
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	home, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	// Preserve existing credentials and local keys across the project rename.
	dir := flags.String("state", filepath.Join(home, "qoder-bridge"), "private state directory")
	port := flags.Int("port", 8787, "loopback listening port")
	model := flags.String("model", "", "exact Qoder model ID (claude-env only)")
	noBrowser := flags.Bool("no-browser", false, "print login URL without opening a browser")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *port < 1 || *port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	credentialPath := filepath.Join(*dir, "credential.json")
	client := qoder.NewClient(credentialPath)
	switch command {
	case "login":
		if err := qoder.Login(ctx, credentialPath, func(u string) {
			fmt.Println("Authorize your Qoder account in the browser:\n" + u)
			if !*noBrowser {
				var cmd *exec.Cmd
				switch runtime.GOOS {
				case "darwin":
					cmd = exec.Command("open", u)
				case "linux":
					cmd = exec.Command("xdg-open", u)
				case "windows":
					cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
				}
				if cmd != nil {
					_ = cmd.Run()
				}
			}
		}); err != nil {
			return err
		}
		fmt.Println("Credential saved. Run cc-qoder-bridge models next.")
	case "models":
		ctx, stop := context.WithTimeout(ctx, 60*time.Second)
		defer stop()
		models, err := client.Models(ctx)
		if err != nil {
			return err
		}
		fmt.Println("ID\tNAME")
		for _, m := range models {
			fmt.Printf("%s\t%s\n", m.ID, m.Name)
		}
	case "claude-env":
		if *model == "" {
			return errors.New("--model is required; choose an exact ID from models")
		}
		key, err := localKey(*dir)
		if err != nil {
			return err
		}
		// Output contains a local bridge key only, never a Qoder credential.
		fmt.Println("unset ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN")
		for _, kv := range [][2]string{{"ANTHROPIC_BASE_URL", "http://127.0.0.1:" + strconv.Itoa(*port)}, {"ANTHROPIC_AUTH_TOKEN", key}, {"ANTHROPIC_MODEL", *model}, {"ANTHROPIC_DEFAULT_OPUS_MODEL", *model}, {"ANTHROPIC_DEFAULT_SONNET_MODEL", *model}, {"ANTHROPIC_DEFAULT_HAIKU_MODEL", *model}, {"CLAUDE_CODE_SUBAGENT_MODEL", *model}, {"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"}} {
			fmt.Printf("export %s=%s\n", kv[0], shellQuote(kv[1]))
		}
	case "serve":
		key, err := localKey(*dir)
		if err != nil {
			return err
		}
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
		if err != nil {
			return err
		}
		handler := &qoder.Server{Backend: client, Key: key, Timeout: 5 * time.Minute, Slots: make(chan struct{}, 8)}
		server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
			BaseContext: func(net.Listener) context.Context { return ctx }}
		go func() {
			<-ctx.Done()
			shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if server.Shutdown(shutdown) != nil {
				_ = server.Close()
			}
		}()
		fmt.Println("Listening at http://" + listener.Addr().String() + " (loopback only)")
		fmt.Println("Readiness: authenticated GET /v1/models. Liveness: GET /healthz.")
		err = server.Serve(listener)
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	default:
		return errors.New("unknown command: " + command)
	}
	return nil
}

func localKey(dir string) (string, error) {
	path := filepath.Join(dir, "bridge.key")
	data, err := os.ReadFile(path)
	if err == nil {
		key := strings.TrimSpace(string(data))
		if len(key) != 64 {
			return "", errors.New("invalid bridge.key; expected a 32-byte hex key")
		}
		if _, err := hex.DecodeString(key); err != nil {
			return "", err
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	key := hex.EncodeToString(raw[:])
	// Publish a complete key atomically, without replacing another process's key.
	f, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return "", err
	}
	if _, err = f.WriteString(key + "\n"); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Link(f.Name(), path); errors.Is(err, os.ErrExist) {
		return localKey(dir)
	}
	return key, err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
