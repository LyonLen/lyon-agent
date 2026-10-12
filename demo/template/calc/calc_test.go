package calc

import "testing"

func TestSum(t *testing.T) {
	cases := []struct {
		in   []int
		want int
	}{
		{nil, 0},
		{[]int{}, 0},
		{[]int{1}, 1},
		{[]int{1, 2, 3, 4}, 10},
		{[]int{-5, 5}, 0},
	}
	for _, c := range cases {
		if got := Sum(c.in); got != c.want {
			t.Errorf("Sum(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestGCD(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{12, 18, 6},
		{7, 13, 1},
		{0, 5, 5},
		{5, 0, 5},
		{-12, 18, 6},
	}
	for _, c := range cases {
		if got := GCD(c.a, c.b); got != c.want {
			t.Errorf("GCD(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestMean(t *testing.T) {
	if got := Mean(nil); got != 0 {
		t.Errorf("Mean(nil) = %v, want 0", got)
	}
	if got := Mean([]int{1, 2, 3, 4}); got != 2.5 {
		t.Errorf("Mean(1..4) = %v, want 2.5", got)
	}
}
