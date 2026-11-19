package analytics

import (
	"math"
	"testing"
)

func TestEstimatedCreatorRevenue(t *testing.T) {
	for _, tt := range []struct {
		name        string
		gross, want int64
	}{
		{"zero", 0, 0},
		{"whole dollars", 10000, 7000},
		{"fractional cents round up", 101, 71},
		{"fractional cents round down", 102, 71},
		{"positive tie", 5, 4},
		{"positive tie with whole portion", 105, 74},
		{"refund", -10000, -7000},
		{"refund rounds toward zero", -102, -71},
		{"refund rounds away from zero", -101, -71},
		{"negative tie", -5, -4},
		{"negative tie with whole portion", -105, -74},
		{"maximum int64", math.MaxInt64, 6456360425798343065},
		{"minimum int64", math.MinInt64, -6456360425798343066},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimatedCreatorRevenue(tt.gross); got != tt.want {
				t.Fatalf("EstimatedCreatorRevenue(%d) = %d; want %d", tt.gross, got, tt.want)
			}
		})
	}
}
