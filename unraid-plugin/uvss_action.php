<?php
// Mutation endpoint used by the settings page. Unraid's WebGUI gateway checks
// csrf_token before dispatching POST requests to plugin PHP files, so forms
// must include the current token but this endpoint does not duplicate that
// platform-level validation.
header('Content-Type: application/json');

if (($_SERVER['REQUEST_METHOD'] ?? '') !== 'POST') {
    http_response_code(405);
    echo json_encode(['ok' => false, 'error' => 'POST required']);
    exit;
}

require_once __DIR__ . '/uvss_control.php';

$action = (string)($_POST['action'] ?? '');
if ($action === 'set-disk-policy') {
    $id = (string)($_POST['disk_id'] ?? '');
    $policy = (string)($_POST['policy'] ?? '');
    if ($id === '' || !in_array($policy, ['auto', 'include', 'exclude'], true)) {
        http_response_code(400);
        echo json_encode(['ok' => false, 'error' => 'invalid disk policy request']);
        exit;
    }
    $result = uvss_control_set_disk_policy($id, $policy);
} elseif ($action === 'reset-disk-policies') {
    $result = uvss_control_reset_disk_policies();
} else {
    http_response_code(400);
    echo json_encode(['ok' => false, 'error' => 'unknown action']);
    exit;
}

if (($result['ok'] ?? false) !== true) {
    $status = (int)($result['http_status'] ?? 0);
    if ($status < 400 || $status > 599) {
        $status = (($result['kind'] ?? '') === 'daemon' || ($result['kind'] ?? '') === 'transport') ? 503 : 502;
    }
    http_response_code($status);
    echo json_encode(['ok' => false, 'error' => (string)($result['error'] ?? 'control request failed')]);
    exit;
}

echo json_encode(['ok' => true]);
