// Package money provides checked integer arithmetic for all monetary domain
// calculations. No calculation in this package silently wraps int64.
package money

import (
	"fmt"
	"math"
)

// Add returns the exact sum or an overflow error.
func Add(values ...int64) (int64, error) {
	const methodCtx = "money.Add"
	var result int64
	for _, value := range values {
		if value > 0 && result > math.MaxInt64-value {
			return 0, fmt.Errorf("%s: переполнение при сложении int64", methodCtx)
		}
		if value < 0 && result < math.MinInt64-value {
			return 0, fmt.Errorf("%s: выход ниже диапазона при сложении int64", methodCtx)
		}
		result += value
	}
	return result, nil
}

// Subtract returns value minus every subtrahend or an overflow error.
func Subtract(value int64, subtrahends ...int64) (int64, error) {
	const methodCtx = "money.Subtract"
	result := value
	for _, subtrahend := range subtrahends {
		if subtrahend > 0 && result < math.MinInt64+subtrahend {
			return 0, fmt.Errorf("%s: выход ниже диапазона при вычитании int64", methodCtx)
		}
		if subtrahend < 0 && result > math.MaxInt64+subtrahend {
			return 0, fmt.Errorf("%s: переполнение при вычитании int64", methodCtx)
		}
		result -= subtrahend
	}
	return result, nil
}

// Multiply returns the exact product or an overflow error.
func Multiply(left, right int64) (int64, error) {
	const methodCtx = "money.Multiply"
	if left == 0 || right == 0 {
		return 0, nil
	}
	if left == -1 && right == math.MinInt64 || right == -1 && left == math.MinInt64 {
		return 0, fmt.Errorf("%s: переполнение при умножении int64", methodCtx)
	}
	result := left * right
	if result/right != left {
		return 0, fmt.Errorf("%s: переполнение при умножении int64", methodCtx)
	}
	return result, nil
}

// CeilDivPositive performs ceil(numerator/denominator) for positive values.
func CeilDivPositive(numerator, denominator int64) (int64, error) {
	const methodCtx = "money.CeilDivPositive"
	if numerator <= 0 || denominator <= 0 {
		return 0, fmt.Errorf("%s: деление с округлением требует положительные операнды", methodCtx)
	}
	quotient := numerator / denominator
	if numerator%denominator != 0 {
		quotient++
	}
	return quotient, nil
}
