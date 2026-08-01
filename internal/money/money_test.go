package money_test

import (
	"math"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/money"
)

func TestCheckedArithmetic(t *testing.T) {
	t.Parallel()
	sum, err := money.Add(10, -4, 7)
	if err != nil || sum != 13 {
		t.Fatalf("Add = %d, %v; want 13, nil", sum, err)
	}
	difference, err := money.Subtract(10, 4, 7)
	if err != nil || difference != -1 {
		t.Fatalf("Subtract = %d, %v; want -1, nil", difference, err)
	}
	product, err := money.Multiply(-7, 6)
	if err != nil || product != -42 {
		t.Fatalf("Multiply = %d, %v; want -42, nil", product, err)
	}
	quotient, err := money.CeilDivPositive(7, 3)
	if err != nil || quotient != 3 {
		t.Fatalf("CeilDivPositive = %d, %v; want 3, nil", quotient, err)
	}
}

func TestArithmeticRejectsOverflow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func() error
	}{
		{"add overflow", func() error { _, err := money.Add(math.MaxInt64, 1); return err }},
		{"add underflow", func() error { _, err := money.Add(math.MinInt64, -1); return err }},
		{"subtract underflow", func() error { _, err := money.Subtract(math.MinInt64, 1); return err }},
		{"subtract overflow", func() error { _, err := money.Subtract(math.MaxInt64, -1); return err }},
		{"multiply overflow", func() error { _, err := money.Multiply(math.MaxInt64, 2); return err }},
		{"min times minus one", func() error { _, err := money.Multiply(math.MinInt64, -1); return err }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.run(); err == nil {
				t.Fatal("expected overflow error")
			}
		})
	}
}
