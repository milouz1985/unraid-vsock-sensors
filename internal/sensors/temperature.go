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
