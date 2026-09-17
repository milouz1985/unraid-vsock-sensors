// SPDX-License-Identifier: GPL-3.0-or-later

package main

import "os"

// defaultControlSocketPath is the local control socket owned by the daemon.
const defaultControlSocketPath = "/run/unraid-vsock-sensors/control.sock"

// controlSocketEnvironmentVariable is the shared override used by the daemon,
// CLI helpers and Unraid WebUI. An explicit --control-socket flag still takes
// precedence because flag defaults are resolved from this environment value.
const controlSocketEnvironmentVariable = "UVSS_CONTROL_SOCKET"

func defaultControlSocketPathFromEnv() string {
	if socketPath := os.Getenv(controlSocketEnvironmentVariable); socketPath != "" {
		return socketPath
	}
	return defaultControlSocketPath
}
