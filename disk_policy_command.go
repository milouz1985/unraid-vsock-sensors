// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// controlClientRuntimeTimeout bounds requests that only read daemon state. A
// healthy daemon answers well within this window; a longer wait would only
// mask a wedged daemon.
const controlClientRuntimeTimeout = 2 * time.Second

// controlClientMutationTimeout bounds requests that persist to disk (a policy
// set/reset performs a Sync and a Rename on /boot). Five seconds leaves ample
// headroom for a normally tiny policy write while still surfacing an unusually
// slow or wedged /boot instead of hiding it behind a long client wait.
const controlClientMutationTimeout = 5 * time.Second

var (
	// errDaemonNotRunning is returned when the control socket is absent or its
	// connection is refused, i.e. no daemon is serving the control plane.
	errDaemonNotRunning = errors.New("unraid-vsock-sensors daemon is not running")
	// errControlTimeout is returned when a control request exceeded its budget.
	errControlTimeout = errors.New("control request timed out")
)

type controlClient struct {
	client *http.Client
}

func newControlClient(socketPath string) *controlClient {
	if socketPath == "" {
		socketPath = defaultControlSocketPath
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := &net.Dialer{}
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	// No global client timeout: each request carries its own deadline so a
	// slow persistent mutation is not aborted by the runtime budget.
	return &controlClient{
		client: &http.Client{Transport: transport},
	}
}

func (c *controlClient) do(method, path string, body any) (int, []byte, error) {
	return c.doWithTimeout(method, path, body, controlClientRuntimeTimeout)
}

func (c *controlClient) doMutation(method, path string, body any) (int, []byte, error) {
	return c.doMutationWithTimeout(method, path, body, controlClientMutationTimeout)
}

func (c *controlClient) doMutationWithTimeout(method, path string, body any, timeout time.Duration) (int, []byte, error) {
	status, responseBody, err := c.doWithTimeout(method, path, body, timeout)
	if err != nil && errors.Is(err, errControlTimeout) {
		return 0, nil, fmt.Errorf("%w; the mutation may still have been applied", err)
	}
	return status, responseBody, err
}

func (c *controlClient) doWithTimeout(method, path string, body any, timeout time.Duration) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, "http://uvss"+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, nil, fmt.Errorf("%w after %s", errControlTimeout, timeout)
		}
		if isUnixSocketRefused(err) {
			return 0, nil, fmt.Errorf("%w: is the daemon started?", errDaemonNotRunning)
		}
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return response.StatusCode, data, err
}

// isUnixSocketRefused reports whether the error comes from a control socket
// that no daemon is listening on: either the socket file is absent (daemon not
// started) or the kernel refused the connection (stale socket).
func isUnixSocketRefused(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || opErr.Err == nil {
		return false
	}
	if errors.Is(opErr.Err, os.ErrNotExist) {
		return true
	}
	var errno unix.Errno
	return errors.As(opErr.Err, &errno) && errno == unix.ECONNREFUSED
}

type diskPolicySetRequest struct {
	ID     string     `json:"id"`
	Policy diskPolicy `json:"policy"`
}

func diskPolicyCommand(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("disks requires list, set, validate, reset or refresh")
	}
	switch args[0] {
	case "list":
		return diskPolicyListCommand(args[1:], output)
	case "set":
		return diskPolicySetCommand(args[1:], output)
	case "validate":
		return diskPolicyValidateCommand(args[1:])
	case "reset":
		return diskPolicyResetCommand(args[1:], output)
	case "refresh":
		return diskPolicyRefreshCommand(args[1:], output)
	default:
		return fmt.Errorf("unknown disks command %q", args[0])
	}
}

func diskPolicyRefreshCommand(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("disks refresh", flag.ContinueOnError)
	socketPath := controlSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("disks refresh does not accept positional arguments")
	}
	client := newControlClient(*socketPath)
	status, body, err := client.do(http.MethodPost, "/v1/refresh", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return controlAPIError(status, body)
	}
	_, err = fmt.Fprintln(output, "disk refresh requested")
	return err
}

func controlSocketFlag(fs *flag.FlagSet) *string {
	return fs.String("control-socket", defaultControlSocketPath, "daemon control Unix socket")
}

func diskPolicyListCommand(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("disks list", flag.ContinueOnError)
	socketPath := controlSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("disks list does not accept positional arguments")
	}
	client := newControlClient(*socketPath)
	status, body, err := client.do(http.MethodGet, "/v1/disks", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return controlAPIError(status, body)
	}
	// The daemon already answers with a JSON-encoded body terminated by a
	// newline (json.Encoder), so it is written through verbatim.
	_, err = output.Write(body)
	return err
}

func diskPolicySetCommand(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("disks set", flag.ContinueOnError)
	encodedID := fs.String("id-base64", "", "base64-encoded stable Unraid disk ID")
	policy := fs.String("policy", "", "auto, include or exclude")
	socketPath := controlSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("disks set does not accept positional arguments")
	}
	idBytes, err := base64.StdEncoding.Strict().DecodeString(*encodedID)
	if err != nil || !utf8.Valid(idBytes) {
		return errors.New("invalid base64 disk ID")
	}
	client := newControlClient(*socketPath)
	status, body, err := client.doMutation(http.MethodPut, "/v1/disk-policy", diskPolicySetRequest{ID: string(idBytes), Policy: diskPolicy(*policy)})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return controlAPIError(status, body)
	}
	_, err = fmt.Fprintln(output, "disk policy saved")
	return err
}

func diskPolicyValidateCommand(args []string) error {
	fs := flag.NewFlagSet("disks validate", flag.ContinueOnError)
	socketPath := controlSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("disks validate does not accept positional arguments")
	}
	client := newControlClient(*socketPath)
	// Validation is performed by the daemon against the persisted policy file
	// only; it does not depend on the collector's thermal state.
	status, body, err := client.do(http.MethodGet, "/v1/policies", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return controlAPIError(status, body)
	}
	// A successful validation is silent, matching the pre-refactor contract.
	return nil
}

func diskPolicyResetCommand(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("disks reset", flag.ContinueOnError)
	socketPath := controlSocketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("disks reset does not accept positional arguments")
	}
	client := newControlClient(*socketPath)
	status, body, err := client.doMutation(http.MethodDelete, "/v1/disk-policies", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return controlAPIError(status, body)
	}
	_, err = fmt.Fprintln(output, "disk policies reset")
	return err
}

func controlAPIError(status int, body []byte) error {
	message := fmt.Sprintf("HTTP %d", status)
	var payload struct {
		Error string `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		message = payload.Error
	}
	return fmt.Errorf("control API: %s", message)
}
