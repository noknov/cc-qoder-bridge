package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalKeyPersistsAndRemainsPrivate(t *testing.T) {
	dir := t.TempDir()
	a, err := localKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := localKey(dir)
	if err != nil || a != b || len(a) != 64 {
		t.Fatal("key was not preserved")
	}
	info, err := os.Stat(filepath.Join(dir, "bridge.key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("key is not private")
	}
}
func TestShellQuoteEscapesMetacharacters(t *testing.T) {
	value := "model'; touch /tmp/unsafe; echo '"
	got := shellQuote(value)
	if !strings.Contains(got, `'"'"'`) || !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Fatal(got)
	}
}
