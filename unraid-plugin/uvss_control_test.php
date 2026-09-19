<?php
// Standalone test for the PHP WebUI control client.
require __DIR__ . '/uvss_control.php';

$failures = 0;
function check(string $label, bool $ok): void {
    global $failures;
    echo ($ok ? 'PASS ' : 'FAIL ') . $label . "\n";
    if (!$ok) $failures++;
}

putenv('UVSS_CONTROL_SOCKET=' . sys_get_temp_dir() . '/uvss-nope.sock');
$result = uvss_control_list_disks();
$expected = (($result['ok'] ?? false) === false && ($result['kind'] ?? '') === 'transport');
check('missing control socket returns a transport error', $expected);

if (function_exists('curl_init')) {
    $staleSocket = sys_get_temp_dir() . '/uvss-stale-' . getmypid() . '.sock';
    file_put_contents($staleSocket, 'not a socket');
    putenv('UVSS_CONTROL_SOCKET=' . $staleSocket);
    $result = uvss_control_list_disks();
    check('existing unusable socket path is a transport error',
        ($result['ok'] ?? true) === false && ($result['kind'] ?? '') === 'transport');
    @unlink($staleSocket);
}

$integrationFunctions = ['curl_init', 'stream_socket_server', 'pcntl_fork'];
$missingIntegrationFunctions = array_values(array_filter(
    $integrationFunctions,
    fn(string $function): bool => !function_exists($function)
));
if ($missingIntegrationFunctions === []) {
    $dir = sys_get_temp_dir() . '/uvss-ctl-test-' . getmypid();
    mkdir($dir, 0700, true);
    $socket = $dir . '/control.sock';
    putenv('UVSS_CONTROL_SOCKET=' . $socket);

    $cases = [
        'list disks' => [
            'status' => 200,
            'body' => json_encode([['id' => 'serial1', 'policy' => 'auto', 'selected' => true, 'eligible' => true]]),
            'method' => 'GET', 'path' => '/v1/disks',
            'call' => fn() => uvss_control_list_disks(),
            'assert' => fn(array $r): bool => ($r['ok'] ?? false) === true && ($r['data'][0]['id'] ?? '') === 'serial1',
        ],
        'set policy' => [
            'status' => 200, 'body' => json_encode(['status' => 'ok']),
            'method' => 'PUT', 'path' => '/v1/disk-policy',
            'call' => fn() => uvss_control_set_disk_policy('serial1', 'exclude'),
            'assert' => fn(array $r): bool => ($r['ok'] ?? false) === true,
            'json' => ['id' => 'serial1', 'policy' => 'exclude'],
        ],
        'reset policies' => [
            'status' => 200, 'body' => json_encode(['status' => 'ok']),
            'method' => 'DELETE', 'path' => '/v1/disk-policies',
            'call' => fn() => uvss_control_reset_disk_policies(),
            'assert' => fn(array $r): bool => ($r['ok'] ?? false) === true,
        ],
        'api error' => [
            'status' => 422, 'body' => json_encode(['error' => 'disk policies file is invalid']),
            'method' => 'GET', 'path' => '/v1/disks',
            'call' => fn() => uvss_control_list_disks(),
            'assert' => fn(array $r): bool => ($r['ok'] ?? true) === false
                && ($r['kind'] ?? '') === 'api' && ($r['http_status'] ?? 0) === 422,
        ],
        'invalid response' => [
            'status' => 200, 'body' => 'not-json',
            'method' => 'GET', 'path' => '/v1/disks',
            'call' => fn() => uvss_control_list_disks(),
            'assert' => fn(array $r): bool => ($r['ok'] ?? true) === false && ($r['kind'] ?? '') === 'protocol',
        ],
    ];

    foreach ($cases as $label => $case) {
        @unlink($socket);
        $capture = $dir . '/request-' . str_replace(' ', '-', $label);
        $server = stream_socket_server('unix://' . $socket, $errno, $errstr);
        if ($server === false) {
            check("$label server", false);
            continue;
        }
        $pid = pcntl_fork();
        if ($pid === 0) {
            $conn = @stream_socket_accept($server, 5);
            if ($conn !== false) {
                $request = '';
                while (strpos($request, "\r\n\r\n") === false && !feof($conn)) {
                    $chunk = fread($conn, 8192);
                    if ($chunk === false || $chunk === '') break;
                    $request .= $chunk;
                }
                [$headers, $requestBody] = array_pad(explode("\r\n\r\n", $request, 2), 2, '');
                $length = 0;
                if (preg_match('/\r\nContent-Length:\s*(\d+)/i', "\r\n" . $headers, $match)) {
                    $length = (int)$match[1];
                }
                while (strlen($requestBody) < $length && !feof($conn)) {
                    $chunk = fread($conn, $length - strlen($requestBody));
                    if ($chunk === false || $chunk === '') break;
                    $requestBody .= $chunk;
                }
                file_put_contents($capture, $headers . "\r\n\r\n" . $requestBody);
                $body = $case['body'];
                $status = $case['status'];
                fwrite($conn, "HTTP/1.1 $status Test\r\nContent-Type: application/json\r\nContent-Length: " . strlen($body) . "\r\nConnection: close\r\n\r\n" . $body);
                fclose($conn);
            }
            fclose($server);
            exit(0);
        }
        fclose($server);
        $result = ($case['call'])();
        pcntl_waitpid($pid, $childStatus);
        $rawRequest = @file_get_contents($capture) ?: '';
        $firstLine = strtok($rawRequest, "\r\n") ?: '';
        check("$label response", ($case['assert'])($result));
        check("$label request line", $firstLine === $case['method'] . ' ' . $case['path'] . ' HTTP/1.1');
        if (isset($case['json'])) {
            $parts = explode("\r\n\r\n", $rawRequest, 2);
            $decoded = json_decode($parts[1] ?? '', true);
            check("$label request body", $decoded === $case['json']);
        }
        @unlink($capture);
        @unlink($socket);
    }
    rmdir($dir);
} else {
    fwrite(STDERR, 'SKIP control socket integration: missing PHP functions: '
        . implode(', ', $missingIntegrationFunctions) . "\n");
}

exit($failures === 0 ? 0 : 1);
