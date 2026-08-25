package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestUnixSocketIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin.sock")
	listener, err := listenUnixSocket(path)
	if errors.Is(err, syscall.EPERM) {
		t.Skip("Unix sockets are blocked by the test sandbox")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("socket permissions %04o allow group or other access", got)
	}
}

func TestUnixSocketDoesNotReplaceRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin.sock")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnixSocket(path); err == nil {
		t.Fatal("regular file should not be replaced")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "keep" {
		t.Fatalf("regular file was altered: %q, %v", content, err)
	}
}
