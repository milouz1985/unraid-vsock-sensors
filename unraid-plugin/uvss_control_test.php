<?php
// Test for the UVSS control socket helper. Run with: php uvss_control_test.php
// It does not require a real daemon: it checks the daemon-stopped error path
// and, when cURL and pcntl are available, exercises the nominal request/response
// path against a fake Unix HTTP server that runs concurrently with the cURL
// client, verifying the homogeneous ok/data envelope.

require __DIR__ . '/uvss_control.php';

$failures = 0;
function check(string $label, bool $ok): void {
    global $failures;
    if ($ok) {
        echo "PASS $label\n";
    } else {
        echo "FAIL $label\n";
        $failures++;
    }
}

// 1. No socket file present -> structured daemon error (ok=false, kind=daemon).
putenv('UVSS_CONTROL_SOCKET=' . sys_get_temp_dir() . '/uvss-nope.sock');
$result = uvss_control_request('GET', '/v1/disks');
$expected = function_exists('curl_init')
    ? (($result['ok'] ?? false) === false && ($result['kind'] ?? '') === 'daemon')
    : (($result['ok'] ?? false) === false && ($result['kind'] ?? '') === 'transport');
check('daemon stopped returns a structured daemon/transport error', $expected);

// 2. list_disks wraps the daemon-stopped error (ok=false), never a list.
$rows = uvss_control_list_disks();
check('list_disks reports a structured error when daemon is stopped', ($rows['ok'] ?? false) === false);

// 3. Nominal path against a fake concurrent Unix HTTP server, checking the
// ok/data envelope for every response shape.
if (function_exists('curl_init') && function_exists('stream_socket_server') && function_exists('pcntl_fork')) {
    $dir = sys_get_temp_dir() . '/uvss-ctl-test-' . getmypid();
    mkdir($dir, 0700, true);
    $socket = $dir . '/control.sock';
    putenv('UVSS_CONTROL_SOCKET=' . $socket);

    // $serverBody is the raw HTTP response the fake server sends for one
    // request. Each case below (re)creates the socket, forks a one-shot server,
    // issues the cURL request from the parent, and reaps the child.
    $cases = [
        'list with one disk' => [
            'body' => json_encode([
                ['id' => 'serial1', 'name' => 'disk1', 'device' => 'sda',
                    'transport' => 'ata', 'bus' => 'non-USB', 'policy' => 'auto',
                    'selected' => true, 'eligible' => true],
            ]),
            'status' => 200,
            'assert' => function (array $r): bool {
                return ($r['ok'] ?? false) === true
                    && ($r['http_status'] ?? 0) === 200
                    && is_array($r['data'] ?? null) && count($r['data']) === 1
                    && ($r['data'][0]['id'] ?? '') === 'serial1';
            },
        ],
        'empty list' => [
            'body' => '[]',
            'status' => 200,
            'assert' => function (array $r): bool {
                return ($r['ok'] ?? false) === true
                    && ($r['http_status'] ?? 0) === 200
                    && ($r['data'] ?? null) === [];
            },
        ],
        'status ok' => [
            'body' => json_encode(['status' => 'ok']),
            'status' => 200,
            'assert' => function (array $r): bool {
                return ($r['ok'] ?? false) === true
                    && ($r['http_status'] ?? 0) === 200
                    && ($r['data'] ?? null) === ['status' => 'ok'];
            },
        ],
        'http 422' => [
            'body' => json_encode(['error' => 'disk policies file is invalid']),
            'status' => 422,
            'assert' => function (array $r): bool {
                return ($r['ok'] ?? false) === false
                    && ($r['kind'] ?? '') === 'api'
                    && ($r['http_status'] ?? 0) === 422
                    && ($r['error'] ?? '') === 'disk policies file is invalid';
            },
        ],
        'http 500' => [
            'body' => json_encode(['error' => 'open disks.ini: no such file']),
            'status' => 500,
            'assert' => function (array $r): bool {
                return ($r['ok'] ?? false) === false
                    && ($r['kind'] ?? '') === 'api'
                    && ($r['http_status'] ?? 0) === 500
                    && ($r['error'] ?? '') === 'open disks.ini: no such file';
            },
        ],
        'invalid json 200' => [
            'body' => 'not-json{',
            'status' => 200,
            'assert' => function (array $r): bool {
                return ($r['ok'] ?? false) === false
                    && ($r['kind'] ?? '') === 'protocol'
                    && ($r['http_status'] ?? 0) === 200;
            },
        ],
        'null body 200' => [
            'body' => 'null',
            'status' => 200,
            'assert' => function (array $r): bool {
                return ($r['ok'] ?? false) === false
                    && ($r['kind'] ?? '') === 'protocol'
                    && ($r['http_status'] ?? 0) === 200;
            },
        ],
    ];

    foreach ($cases as $label => $case) {
        @unlink($socket);
        $server = stream_socket_server('unix://' . $socket, $errno, $errstr);
        if ($server === false) {
            check("$label: fake socket created", false);
            continue;
        }
        $body = $case['body'];
        $status = $case['status'];
        $pid = pcntl_fork();
        if ($pid === 0) {
            // Child: answer one request then exit.
            stream_set_blocking($server, true);
            $conn = @stream_socket_accept($server, 5);
            if ($conn !== false) {
                $request = '';
                while (($chunk = fread($conn, 8192)) !== false && strpos($request, "\r\n\r\n") === false) {
                    $request .= $chunk;
                }
                $reason = $status === 200 ? 'OK' : ($status === 422 ? 'Unprocessable Entity' : 'Internal Server Error');
                fwrite($conn, "HTTP/1.1 $status $reason\r\n" .
                    "Content-Type: application/json\r\n" .
                    "Content-Length: " . strlen($body) . "\r\n" .
                    "Connection: close\r\n\r\n" . $body);
                fclose($conn);
            }
            fclose($server);
            @unlink($socket);
            exit(0);
        } elseif ($pid > 0) {
            // Parent: issue the request while the child serves it.
            $result = uvss_control_request('GET', '/v1/disks');
            check("$label", ($case['assert'])($result));
            pcntl_waitpid($pid, $status2);
        }
        @unlink($socket);
    }
    rmdir($dir);
} else {
    echo "SKIP cURL/stream-socket/pcntl integration (extension not available)\n";
}

exit($failures === 0 ? 0 : 1);
