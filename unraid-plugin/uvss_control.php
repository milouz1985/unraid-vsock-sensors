<?php
// Central helper for talking to the UVSS daemon over its local control Unix
// socket for read-only operations (listing the disk inventory, validating the
// policy file). The transport is cURL over a Unix socket
// (CURLOPT_UNIX_SOCKET_PATH): no shell and no external curl.
//
// Disk policy mutations (set/reset) are not issued here: the WebUI routes them
// through update.php -> service.sh -> the UVSS CLI, which is now only a client
// of the daemon's control socket and no longer writes the policy file itself.
//
// uvss_control_request() always returns a homogeneous envelope:
//   success: ['ok' => true, 'http_status' => int, 'data' => mixed]
//   failure: ['ok' => false, 'kind' => 'daemon'|'transport'|'api'|'protocol',
//             'http_status' => int, 'error' => string]
// A GET /v1/disks answering [] is a success with data=[] (no disks known). A
// 200 response whose body is not a JSON object or array (invalid JSON, or a
// bare "null") is a protocol error, never silently coerced to an empty list.

function uvss_control_socket_path(): string {
    return getenv('UVSS_CONTROL_SOCKET') ?: '/run/unraid-vsock-sensors/control.sock';
}

// Timeouts (seconds). Reads and other runtime calls use a short budget; a
// healthy daemon answers well within it. Mutations that persist to /boot are
// handled elsewhere (the CLI) with a longer budget.
const UVSS_CONTROL_RUNTIME_TIMEOUT = 2;

/**
 * Issue an HTTP request to the UVSS control socket and return a homogeneous
 * envelope (see the file header for the exact shape).
 *
 * @return array<string,mixed>
 */
function uvss_control_request(string $method, string $path, ?array $body = null, int $timeout = UVSS_CONTROL_RUNTIME_TIMEOUT): array
{
    if (!function_exists('curl_init')) {
        return ['ok' => false, 'kind' => 'transport', 'http_status' => 0,
            'error' => 'cURL extension is not available'];
    }
    $socket = uvss_control_socket_path();
    if (!file_exists($socket)) {
        return ['ok' => false, 'kind' => 'daemon', 'http_status' => 0,
            'error' => 'unraid-vsock-sensors daemon is not running'];
    }

    $payload = $body === null ? null : json_encode($body);
    $headers = ['Accept: application/json'];
    if ($payload !== null) {
        $headers[] = 'Content-Type: application/json';
    }

    $curl = curl_init();
    curl_setopt_array($curl, [
        CURLOPT_URL => 'http://uvss' . $path,
        CURLOPT_UNIX_SOCKET_PATH => $socket,
        CURLOPT_CUSTOMREQUEST => $method,
        CURLOPT_HTTPHEADER => $headers,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_TIMEOUT => $timeout,
        CURLOPT_CONNECTTIMEOUT => $timeout,
        CURLOPT_FOLLOWLOCATION => false,
    ]);
    if ($payload !== null) {
        curl_setopt($curl, CURLOPT_POSTFIELDS, $payload);
    }

    $response = curl_exec($curl);
    $errno = curl_errno($curl);
    $error = curl_error($curl);
    $status = (int)curl_getinfo($curl, CURLINFO_RESPONSE_CODE);
    curl_close($curl);

    if ($response === false || $errno !== 0) {
        $kind = ($errno === CURLE_OPERATION_TIMEDOUT) ? 'transport' : 'daemon';
        return ['ok' => false, 'kind' => $kind, 'http_status' => $status,
            'error' => "control request failed ($error)"];
    }
    if ($status < 200 || $status >= 300) {
        $decoded = json_decode((string)$response, true);
        $message = (is_array($decoded) && isset($decoded['error']))
            ? (string)$decoded['error']
            : "control API error (HTTP $status)";
        return ['ok' => false, 'kind' => 'api', 'http_status' => $status, 'error' => $message];
    }
    // A 200 response must be a JSON object or array. json_decode returns null
    // for both "null" and invalid JSON; a bare "null" body is therefore also a
    // protocol violation, never a valid answer.
    $decoded = json_decode((string)$response, true);
    if (json_last_error() !== JSON_ERROR_NONE || !is_array($decoded)) {
        return ['ok' => false, 'kind' => 'protocol', 'http_status' => $status,
            'error' => 'control API returned invalid JSON'];
    }
    return ['ok' => true, 'http_status' => $status, 'data' => $decoded];
}

/**
 * Fetch the disk policy inventory. A valid empty inventory is a success with
 * data=[] and is never treated as a transport error.
 *
 * @return array<string,mixed>
 */
function uvss_control_list_disks(): array
{
    return uvss_control_request('GET', '/v1/disks');
}
