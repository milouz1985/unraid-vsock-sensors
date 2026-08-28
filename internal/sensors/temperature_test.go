package sensors

import "testing"

func TestMaxTemperature(t *testing.T) {
	items := []Disk{{Temp: -5}, {Temp: 42}, {Temp: 17}}
	if got := MaxTemperature(items, func(disk Disk) float64 { return disk.Temp }); got != 42 {
		t.Fatalf("MaxTemperature() = %v, want 42", got)
	}
}

func TestMaxAvailableDiskTemperature(t *testing.T) {
	disks := []Disk{{Temp: 35}, {Temp: 80, Unavailable: true}, {Temp: 42}}
	if got, ok := MaxAvailableDiskTemperature(disks); !ok || got != 42 {
		t.Fatalf("MaxAvailableDiskTemperature() = %v, %v; want 42, true", got, ok)
	}
	if got, ok := MaxAvailableDiskTemperature([]Disk{{Unavailable: true}}); ok || got != 0 {
		t.Fatalf("all unavailable = %v, %v; want 0, false", got, ok)
	}
}
