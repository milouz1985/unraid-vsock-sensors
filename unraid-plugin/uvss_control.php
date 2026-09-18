<?php
// Shared WebUI client for the daemon's local HTTP API over a Unix socket.
// The settings page is the only control-plane client: reads and mutations both
// use this helper, so the WebUI never spawns the Go binary to manage policies.

function uvss_control_socket_path(): string {
    // Environment override is primarily useful for the standalone PHP test.
    return getenv('UVSS_CONTROL_SOCKET') ?: '/run/unraid-vsock-sensors/control.sock';
}

const UVSS_CONTROL_RUNTIME_TIMEOUT = 2;
const UVSS_CONTROL_MUTATION_TIMEOUT = 5;

/**
 * @return array<string,mixed>
 */
function uvss_control_request(string $method, string $path, ?array $body = null): array {
    $mutation = $method !== 'GET';
    $timeout = $mutation ? UVSS_CONTROL_MUTATION_TIMEOUT : UVSS_CONTROL_RUNTIME_TIMEOUT;
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
    if ($body !== null && $payload === false) {
        return ['ok' => false, 'kind' => 'protocol', 'http_status' => 0,
            'error' => 'unable to encode control request'];
    }
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
        if ($errno === CURLE_OPERATION_TIMEDOUT && $mutation) {
            $error = 'control mutation timed out; it may still have been applied';
        } else {
            $error = "control request failed ($error)";
        }
        return ['ok' => false, 'kind' => 'transport',
            'http_status' => $status, 'error' => $error];
    }
    if ($status < 200 || $status >= 300) {
        $decoded = json_decode((string)$response, true);
        $message = (is_array($decoded) && isset($decoded['error']))
            ? (string)$decoded['error']
            : "control API error (HTTP $status)";
        return ['ok' => false, 'kind' => 'api', 'http_status' => $status, 'error' => $message];
    }

    $decoded = json_decode((string)$response, true);
    if (json_last_error() !== JSON_ERROR_NONE || !is_array($decoded)) {
        return ['ok' => false, 'kind' => 'protocol', 'http_status' => $status,
            'error' => 'control API returned invalid JSON'];
    }
    return ['ok' => true, 'http_status' => $status, 'data' => $decoded];
}

/** @return array<string,mixed> */
function uvss_control_list_disks(): array {
    return uvss_control_request('GET', '/v1/disks');
}

/** @return array<string,mixed> */
function uvss_control_set_disk_policy(string $id, string $policy): array {
    return uvss_control_request(
        'PUT',
        '/v1/disk-policy',
        ['id' => $id, 'policy' => $policy]
    );
}

/** @return array<string,mixed> */
function uvss_control_reset_disk_policies(): array {
    return uvss_control_request('DELETE', '/v1/disk-policies');
}
