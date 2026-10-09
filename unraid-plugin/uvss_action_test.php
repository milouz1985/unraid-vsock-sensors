<?php
// Exercise the real mutation endpoint against an HTTP control API on a Unix socket.
$required = ['curl_init', 'stream_socket_server', 'pcntl_fork', 'pcntl_waitpid', 'pcntl_wifexited', 'pcntl_wexitstatus'];
$missing = array_values(array_filter($required, fn(string $name): bool => !function_exists($name)));
if ($missing !== []) {
    fwrite(STDERR, 'FAIL action socket integration: missing PHP functions: '
        . implode(', ', $missing) . "\n");
    exit(1);
}

$failures = 0;
$reports = [];
function check_action(string $label, bool $ok): void {
    global $failures, $reports;
    // Do not send headers before forking the next endpoint invocation.
    $reports[] = ($ok ? 'PASS ' : 'FAIL ') . $label;
    if (!$ok) $failures++;
}

// Expected routes and payloads are literal API contracts, independent of the client helpers.
$cases = [
    'set policy' => [
        'post' => ['action' => 'set-disk-policy', 'disk_id' => 'SERIAL/with space', 'policy' => 'exclude'],
        'line' => 'PUT /v1/disk-policy HTTP/1.1',
        'json' => ['id' => 'SERIAL/with space', 'policy' => 'exclude'],
        'status' => 200, 'body' => '{"status":"ok"}',
        'expected_status' => 200, 'expected_body' => ['ok' => true],
    ],
    'reset policies' => [
        'post' => ['action' => 'reset-disk-policies'],
        'line' => 'DELETE /v1/disk-policies HTTP/1.1', 'json' => null,
        'status' => 200, 'body' => '{"status":"ok"}',
        'expected_status' => 200, 'expected_body' => ['ok' => true],
    ],
    'invalid policy rejected by API' => [
        'post' => ['action' => 'set-disk-policy', 'disk_id' => 'serial1', 'policy' => 'invalid'],
        'line' => 'PUT /v1/disk-policy HTTP/1.1',
        'json' => ['id' => 'serial1', 'policy' => 'invalid'],
        'status' => 400, 'body' => '{"error":"invalid disk policy"}',
        'expected_status' => 400, 'expected_body' => ['ok' => false, 'error' => 'invalid disk policy'],
    ],
    'reset API failure' => [
        'post' => ['action' => 'reset-disk-policies'],
        'line' => 'DELETE /v1/disk-policies HTTP/1.1', 'json' => null,
        'status' => 500, 'body' => '{"error":"unable to persist disk policies"}',
        'expected_status' => 500, 'expected_body' => ['ok' => false, 'error' => 'unable to persist disk policies'],
    ],
    'missing control socket' => [
        'post' => ['action' => 'reset-disk-policies'],
        'socket_missing' => true, 'line' => null, 'json' => null,
        'expected_status' => 503, 'expected_body' => ['ok' => false],
        'error_prefix' => 'control request failed (',
    ],
    'invalid API response' => [
        'post' => ['action' => 'reset-disk-policies'],
        'line' => 'DELETE /v1/disk-policies HTTP/1.1', 'json' => null,
        'status' => 200, 'body' => 'not-json',
        'expected_status' => 502, 'expected_body' => ['ok' => false, 'error' => 'control API returned invalid JSON'],
    ],
];

$dir = sys_get_temp_dir() . '/uvss-action-test-' . getmypid();
if (!mkdir($dir, 0700)) exit(1);
$socket = $dir . '/control.sock';
$capture = $dir . '/response.json';
putenv('UVSS_CONTROL_SOCKET=' . $socket);

foreach ($cases as $label => $case) {
    $server = isset($case['socket_missing'])
        ? null : stream_socket_server('unix://' . $socket, $errno, $errstr);
    if ($server === false) {
        check_action("$label server ($errstr)", false);
        break;
    }
    $pid = pcntl_fork();
    if ($pid === -1) {
        if (is_resource($server)) fclose($server);
        check_action("$label fork", false);
        break;
    }
    if ($pid === 0) {
        if (is_resource($server)) fclose($server);
        $_SERVER['REQUEST_METHOD'] = 'POST';
        $_POST = $case['post'];
        ob_start();
        register_shutdown_function(function () use ($capture): void {
            file_put_contents($capture, json_encode([
                'status' => http_response_code() ?: 200,
                'body' => ob_get_contents(),
            ]));
            ob_end_clean();
        });
        require __DIR__ . '/uvss_action.php';
        exit(0);
    }

    // The listener is ready before the endpoint starts, with no startup sleeps.
    $request = '';
    $conn = is_resource($server) ? @stream_socket_accept($server, 6) : false;
    if ($conn !== false) {
        stream_set_timeout($conn, 6);
        while (strpos($request, "\r\n\r\n") === false && !feof($conn)) {
            $chunk = fread($conn, 8192);
            if ($chunk === false || $chunk === '') break;
            $request .= $chunk;
        }
        [$headers, $body] = array_pad(explode("\r\n\r\n", $request, 2), 2, '');
        $length = 0;
        if (preg_match('/\r\nContent-Length:\s*(\d+)/i', "\r\n" . $headers, $match)) {
            $length = (int)$match[1];
        }
        while (strlen($body) < $length && !feof($conn)) {
            $chunk = fread($conn, $length - strlen($body));
            if ($chunk === false || $chunk === '') break;
            $body .= $chunk;
        }
        $responseBody = $case['body'];
        fwrite($conn, 'HTTP/1.1 ' . $case['status'] . " Test\r\nContent-Type: application/json\r\nContent-Length: "
            . strlen($responseBody) . "\r\nConnection: close\r\n\r\n" . $responseBody);
        fclose($conn);
    } else {
        $headers = $body = '';
    }
    if (is_resource($server)) fclose($server);
    pcntl_waitpid($pid, $childStatus);
    $response = json_decode(@file_get_contents($capture) ?: '', true);
    check_action("$label endpoint exits successfully", pcntl_wifexited($childStatus) && pcntl_wexitstatus($childStatus) === 0);
    check_action("$label control request line", $case['line'] === null
        ? $headers === '' : strtok($headers, "\r\n") === $case['line']);
    check_action("$label control request body", $case['json'] === null
        ? $body === ''
        : json_decode($body, true) === $case['json']);
    check_action("$label caller status", ($response['status'] ?? 0) === $case['expected_status']);
    $callerBody = json_decode($response['body'] ?? '', true);
    if (isset($case['error_prefix'])) {
        // cURL's OS-specific text varies; the public transport category and status do not.
        $error = $callerBody['error'] ?? '';
        check_action("$label caller error", is_string($error) && strpos($error, $case['error_prefix']) === 0);
        unset($callerBody['error']);
    }
    check_action("$label caller result", $callerBody === $case['expected_body']);
    @unlink($capture);
    @unlink($socket);
}
@unlink($capture);
@unlink($socket);
rmdir($dir);
echo implode("\n", $reports) . "\n";
exit($failures === 0 ? 0 : 1);
