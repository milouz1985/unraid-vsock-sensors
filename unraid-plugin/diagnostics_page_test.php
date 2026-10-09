<?php
// SPDX-License-Identifier: GPL-3.0-or-later

ob_start();
require __DIR__ . '/UnraidVsockSensorsDiagnostics.page';
ob_end_clean();

$cases = [
    [
        'name' => 'IOC only',
        'controller' => ['temp_c' => 51, 'ioc_temp_c' => 51],
        'expected' => ['51 °C', '—'],
    ],
    [
        'name' => 'Board only',
        'controller' => ['temp_c' => 47, 'board_temp_c' => 47],
        'expected' => ['—', '47 °C'],
    ],
    [
        'name' => 'IOC and Board',
        'controller' => ['temp_c' => 51, 'ioc_temp_c' => 51, 'board_temp_c' => 47],
        'expected' => ['51 °C', '47 °C'],
    ],
    [
        'name' => 'legacy temperature',
        'controller' => ['temp_c' => 49],
        'expected' => ['49 °C', '—'],
    ],
];

foreach ($cases as $case) {
    [$ioc, $board] = uvssDiagHBATemperatures($case['controller']);
    $actual = [uvssDiagTemperature($ioc), uvssDiagTemperature($board)];
    if ($actual !== $case['expected']) {
        fwrite(
            STDERR,
            $case['name'] . ': got ' . json_encode($actual) . ', want ' . json_encode($case['expected']) . PHP_EOL
        );
        exit(1);
    }
}
