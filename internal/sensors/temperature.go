package sensors

import (
	"cmp"
	"slices"
)

// MaxTemperature returns the highest temperature among items. Callers must
// pass a non-empty slice because slices.MaxFunc panics otherwise.
func MaxTemperature[T any](items []T, temperature func(T) float64) float64 {
	hottest := slices.MaxFunc(items, func(a, b T) int {
		return cmp.Compare(temperature(a), temperature(b))
	})
	return temperature(hottest)
}

// MaxAvailableDiskTemperature returns the highest usable disk temperature.
// The boolean is false when every disk is unavailable.
func MaxAvailableDiskTemperature(disks []Disk) (float64, bool) {
	available := make([]Disk, 0, len(disks))
	for _, disk := range disks {
		if !disk.Unavailable {
			available = append(available, disk)
		}
	}
	if len(available) == 0 {
		return 0, false
	}
	return MaxTemperature(available, func(disk Disk) float64 {
		return disk.Temp
	}), true
}
