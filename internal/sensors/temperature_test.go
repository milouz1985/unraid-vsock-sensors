package sensors

import "testing"

func TestMaxTemperature(t *testing.T) {
	items := []Disk{{Temp: -5}, {Temp: 42}, {Temp: 17}}
	if got := MaxTemperature(items, func(disk Disk) float64 { return disk.Temp }); got != 42 {
		t.Fatalf("MaxTemperature() = %v, want 42", got)
	}
}
