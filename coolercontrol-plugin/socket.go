package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

func listenUnixSocket(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refuse to replace non-socket path %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale Unix socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect Unix socket path: %w", err)
	}

	// This is a local Unix socket, not a TCP listener. Restrict access at
	// creation time so there is no permissive window before chmod. Root
	// (CoolerControl) can still connect to the plugin-owned socket.
	previousUmask := syscall.Umask(0o077)
	listener, err := net.Listen("unix", path)
	syscall.Umask(previousUmask)
	if err != nil {
		return nil, fmt.Errorf("listen on Unix socket: %w", err)
	}
	return listener, nil
}
