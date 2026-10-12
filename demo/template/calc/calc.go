// Package calc is the tiny arithmetic library the evolution demo's
// agent candidates work on: small enough that every task finishes in
// minutes, real enough that bugs and features have clear correctness.
package calc

// Sum adds up nums; an empty slice sums to 0.
func Sum(nums []int) int {
	total := 0
	for _, n := range nums {
		total += n
	}
	return total
}

// GCD returns the greatest common divisor of a and b (non-negative).
func GCD(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

// Mean returns the arithmetic mean, or 0 for an empty slice.
func Mean(nums []int) float64 {
	if len(nums) == 0 {
		return 0
	}
	return float64(Sum(nums)) / float64(len(nums))
}
