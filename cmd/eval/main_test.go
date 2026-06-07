package main

import (
	"math"
	"testing"
)

func TestPercentile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		vals []float64
		p    float64
		want float64
	}{
		{
			name: "empty slice returns 0",
			vals: []float64{},
			p:    50,
			want: 0,
		},
		{
			name: "single element p50",
			vals: []float64{42},
			p:    50,
			want: 42,
		},
		{
			name: "single element p95",
			vals: []float64{42},
			p:    95,
			want: 42,
		},
		{
			name: "p50 of known distribution (1..10)",
			// sorted: [1,2,3,4,5,6,7,8,9,10]; rank=ceil(50/100*10)=5 → index 4 → 5
			vals: []float64{10, 1, 3, 5, 7, 9, 2, 4, 6, 8},
			p:    50,
			want: 5,
		},
		{
			name: "p95 of known distribution (1..20)",
			// sorted: [1..20]; rank=ceil(95/100*20)=ceil(19)=19 → index 18 → 19
			vals: func() []float64 {
				s := make([]float64, 20)
				for i := range s {
					s[i] = float64(i + 1)
				}
				return s
			}(),
			p:    95,
			want: 19,
		},
		{
			name: "unsorted input — result equals sorted result",
			// five values: 10 20 30 40 50 (sorted); p50 → rank=ceil(2.5)=3 → 30
			vals: []float64{50, 10, 30, 20, 40},
			p:    50,
			want: 30,
		},
		{
			name: "p100 returns max element",
			vals: []float64{3, 1, 4, 1, 5, 9, 2, 6},
			p:    100,
			want: 9,
		},
		{
			name: "p0 returns min element (rank rounds up to 1)",
			vals: []float64{3, 1, 4},
			p:    0,
			want: 1,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := percentile(tc.vals, tc.p)
			if got != tc.want {
				t.Errorf("percentile(%v, %v) = %v; want %v", tc.vals, tc.p, got, tc.want)
			}
		})
	}
}

func TestMeanFloat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		vals []float64
		want float64
	}{
		{
			name: "empty slice returns 0",
			vals: []float64{},
			want: 0,
		},
		{
			name: "single element",
			vals: []float64{7},
			want: 7,
		},
		{
			name: "mean of 1..5 is 3",
			vals: []float64{1, 2, 3, 4, 5},
			want: 3,
		},
		{
			name: "mean of 10 100 is 55",
			vals: []float64{10, 100},
			want: 55,
		},
	}

	const eps = 1e-9
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := meanFloat(tc.vals)
			if math.Abs(got-tc.want) > eps {
				t.Errorf("meanFloat(%v) = %v; want %v", tc.vals, got, tc.want)
			}
		})
	}
}
