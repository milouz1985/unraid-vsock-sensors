<?php
// SPDX-License-Identifier: GPL-3.0-or-later
//
// This test verifies the Go -> PHP JSON contract for the diagnostics page and
// the disk policy inventory. It reads the versioned golden files
// (testdata/contract/diagnostics.json and testdata/contract/disk-policy.json)
// and checks that the expected field names and types are present. It does not
// execute UnraidVsockSensorsDiagnostics.page or UnraidVsockSensors.page; the
// functional PHP page tests (diagnostics_page_test.php, uvss_control_test.php)
// cover that separately.
//
// The Go tests (TestDiagnosticsContractFixture, TestDiskPolicyContractFixture)
// guarantee that the production code produces this contract; this test
// guarantees that the PHP consumer accepts the same contract. A rename or type
// change on the Go side is detected by the Go test; a missing field on the PHP
// side is detected here.

$fixtureDir = __DIR__ . '/../testdata/contract';

// --- diagnostics.json ---
$diagnosticsPath = $fixtureDir . '/diagnostics.json';
if (!file_exists($diagnosticsPath)) {
    fwrite(STDERR, "Missing fixture: $diagnosticsPath (run `go test -run TestDiagnosticsContractFixture .` first)\n");
    exit(1);
}
$diagnostics = json_decode(file_get_contents($diagnosticsPath), true);
if (!is_array($diagnostics)) {
    fwrite(STDERR, "Invalid JSON in $diagnosticsPath\n");
    exit(1);
}

// Top-level fields.
$requiredTopLevel = ['schema_version', 'version', 'pid', 'started_at', 'generated_at', 'uptime_seconds', 'vsock', 'emhttpd', 'disks', 'hba'];
foreach ($requiredTopLevel as $field) {
    if (!array_key_exists($field, $diagnostics)) {
        fwrite(STDERR, "diagnostics.json missing top-level field '$field'\n");
        exit(1);
    }
}
if ((int)($diagnostics['schema_version'] ?? 0) !== 2) {
    fwrite(STDERR, "diagnostics.json schema_version = " . var_export($diagnostics['schema_version'] ?? null, true) . ", want 2\n");
    exit(1);
}
if (!is_int($diagnostics['pid']) || $diagnostics['pid'] <= 0) {
    fwrite(STDERR, "diagnostics.json pid = " . var_export($diagnostics['pid'] ?? null, true) . ", want positive int\n");
    exit(1);
}
if (!is_string($diagnostics['version']) || $diagnostics['version'] === '') {
    fwrite(STDERR, "diagnostics.json version is empty\n");
    exit(1);
}

// VSOCK fields.
$vsock = $diagnostics['vsock'];
$requiredVsock = ['status', 'host_cid', 'port', 'last_connected_at', 'last_published_at', 'last_published_age_seconds'];
foreach ($requiredVsock as $field) {
    if (!array_key_exists($field, $vsock)) {
        fwrite(STDERR, "diagnostics.json vsock missing field '$field'\n");
        exit(1);
    }
}
if (!is_string($vsock['status'])) {
    fwrite(STDERR, "diagnostics.json vsock.status is not a string\n");
    exit(1);
}
if (!is_int($vsock['host_cid'])) {
    fwrite(STDERR, "diagnostics.json vsock.host_cid is not an int\n");
    exit(1);
}
if (!is_int($vsock['port'])) {
    fwrite(STDERR, "diagnostics.json vsock.port is not an int\n");
    exit(1);
}
if (!is_int($vsock['last_published_age_seconds']) && $vsock['last_published_age_seconds'] !== null) {
    fwrite(STDERR, "diagnostics.json vsock.last_published_age_seconds is not an int or null\n");
    exit(1);
}

// Emhttpd fields.
$emhttpd = $diagnostics['emhttpd'];
$requiredEmhttpd = ['status', 'temperature_source', 'last_poll_at', 'last_poll_age_seconds', 'poll_attributes', 'stale_after', 'fallback_active'];
foreach ($requiredEmhttpd as $field) {
    if (!array_key_exists($field, $emhttpd)) {
        fwrite(STDERR, "diagnostics.json emhttpd missing field '$field'\n");
        exit(1);
    }
}
if (!is_string($emhttpd['status'])) {
    fwrite(STDERR, "diagnostics.json emhttpd.status is not a string\n");
    exit(1);
}
if (!is_string($emhttpd['temperature_source'])) {
    fwrite(STDERR, "diagnostics.json emhttpd.temperature_source is not a string\n");
    exit(1);
}
if (!is_bool($emhttpd['fallback_active'])) {
    fwrite(STDERR, "diagnostics.json emhttpd.fallback_active is not a bool\n");
    exit(1);
}

// Disks fields.
$disks = $diagnostics['disks'];
if (!array_key_exists('items', $disks) || !is_array($disks['items'])) {
    fwrite(STDERR, "diagnostics.json disks.items is missing or not an array\n");
    exit(1);
}
if (count($disks['items']) === 0) {
    fwrite(STDERR, "diagnostics.json disks.items is empty\n");
    exit(1);
}
$disk = $disks['items'][0];
$requiredDisk = ['name', 'id', 'device', 'transport', 'rotational', 'temperature_c', 'status', 'source', 'last_valid_age_seconds'];
foreach ($requiredDisk as $field) {
    if (!array_key_exists($field, $disk)) {
        fwrite(STDERR, "diagnostics.json disks.items[0] missing field '$field'\n");
        exit(1);
    }
}
if (!is_string($disk['name'])) {
    fwrite(STDERR, "diagnostics.json disk.name is not a string\n");
    exit(1);
}
if (!is_string($disk['id'])) {
    fwrite(STDERR, "diagnostics.json disk.id is not a string\n");
    exit(1);
}
if (!is_bool($disk['rotational'])) {
    fwrite(STDERR, "diagnostics.json disk.rotational is not a bool\n");
    exit(1);
}
if (!is_float($disk['temperature_c']) && !is_int($disk['temperature_c'])) {
    fwrite(STDERR, "diagnostics.json disk.temperature_c is not a number\n");
    exit(1);
}

// HBA fields.
$hba = $diagnostics['hba'];
$requiredHba = ['status', 'mode', 'backend', 'interval', 'last_successful_at', 'snapshot_age_seconds', 'count'];
foreach ($requiredHba as $field) {
    if (!array_key_exists($field, $hba)) {
        fwrite(STDERR, "diagnostics.json hba missing field '$field'\n");
        exit(1);
    }
}
if (!is_int($hba['count'])) {
    fwrite(STDERR, "diagnostics.json hba.count is not an int\n");
    exit(1);
}
if (!array_key_exists('items', $hba) || !is_array($hba['items'])) {
    fwrite(STDERR, "diagnostics.json hba.items is missing or not an array\n");
    exit(1);
}
if (count($hba['items']) === 0) {
    fwrite(STDERR, "diagnostics.json hba.items is empty\n");
    exit(1);
}
$hbaItem = $hba['items'][0];
$requiredHbaItem = ['id', 'model', 'pci_address', 'ioc_temp_c', 'board_temp_c'];
foreach ($requiredHbaItem as $field) {
    if (!array_key_exists($field, $hbaItem)) {
        fwrite(STDERR, "diagnostics.json hba.items[0] missing field '$field'\n");
        exit(1);
    }
}
if (!is_string($hbaItem['id'])) {
    fwrite(STDERR, "diagnostics.json hba.items[0].id is not a string\n");
    exit(1);
}

// --- disk-policy.json ---
$policyPath = $fixtureDir . '/disk-policy.json';
if (!file_exists($policyPath)) {
    fwrite(STDERR, "Missing fixture: $policyPath (run `go test -run TestDiskPolicyContractFixture .` first)\n");
    exit(1);
}
$policy = json_decode(file_get_contents($policyPath), true);
if (!is_array($policy)) {
    fwrite(STDERR, "Invalid JSON in $policyPath\n");
    exit(1);
}
$requiredPolicy = ['id', 'name', 'device', 'transport', 'bus', 'policy', 'selected', 'eligible', 'validation_error'];
foreach ($requiredPolicy as $field) {
    if (!array_key_exists($field, $policy)) {
        fwrite(STDERR, "disk-policy.json missing field '$field'\n");
        exit(1);
    }
}
if (!is_string($policy['id'])) {
    fwrite(STDERR, "disk-policy.json id is not a string\n");
    exit(1);
}
if (!is_bool($policy['selected'])) {
    fwrite(STDERR, "disk-policy.json selected is not a bool\n");
    exit(1);
}
if (!is_bool($policy['eligible'])) {
    fwrite(STDERR, "disk-policy.json eligible is not a bool\n");
    exit(1);
}

echo "Go -> PHP contract: OK\n";
