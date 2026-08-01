// Package economy выполняет детерминированные денежные расчёты.
package economy

import (
	"fmt"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/money"
)

// ScoreWeights задаёт веса ранжирования возможностей.
type ScoreWeights struct {
	Profit     float64
	ROI        float64
	Liquidity  float64
	Confidence float64
	Risk       float64
	Slots      float64
}

// DirectProfit рассчитывает чистую прибыль прямой перепродажи.
func DirectProfit(salePrice, saleCommission, listingFee, purchasePrice int64) (int64, error) {
	const methodCtx = "economy.DirectProfit"

	if salePrice < 0 || saleCommission < 0 || listingFee < 0 || purchasePrice < 0 {
		return 0, fmt.Errorf("%s: денежные значения не могут быть отрицательными", methodCtx)
	}
	profit, err := money.Subtract(salePrice, saleCommission, listingFee, purchasePrice)
	if err != nil {
		return 0, fmt.Errorf("%s: не удалось рассчитать прибыль: %w", methodCtx, err)
	}
	return profit, nil
}

// BarterCost рассчитывает стоимость всех ингредиентов обмена.
func BarterCost(prices map[string]int64, ingredients []domain.BarterIngredient) (int64, error) {
	const methodCtx = "economy.BarterCost"

	var total int64
	for _, ingredient := range ingredients {
		price, ok := prices[ingredient.ItemID]
		if !ok {
			return 0, fmt.Errorf("%s: неизвестна цена ингредиента %q", methodCtx, ingredient.ItemID)
		}
		if price < 0 || ingredient.Quantity <= 0 {
			return 0, fmt.Errorf("%s: некорректная стоимость ингредиента %q", methodCtx, ingredient.ItemID)
		}
		line, err := money.Multiply(price, ingredient.Quantity)
		if err != nil {
			return 0, fmt.Errorf("%s: некорректная стоимость ингредиента %q: %w", methodCtx, ingredient.ItemID, err)
		}
		total, err = money.Add(total, line)
		if err != nil {
			return 0, fmt.Errorf("%s: стоимость обмена превышает допустимое значение: %w", methodCtx, err)
		}
	}
	return total, nil
}

// Complete рассчитывает производные поля возможности без округления денег.
func Complete(opportunity domain.TradeOpportunity) (domain.TradeOpportunity, error) {
	const methodCtx = "economy.Complete"

	if opportunity.InputCost < 0 || opportunity.ExpectedRevenue < 0 || opportunity.ExpectedFees < 0 {
		return opportunity, fmt.Errorf("%s: денежные значения возможности не могут быть отрицательными", methodCtx)
	}
	profit, err := money.Subtract(opportunity.ExpectedRevenue, opportunity.ExpectedFees, opportunity.InputCost)
	if err != nil {
		return opportunity, fmt.Errorf("%s: не удалось рассчитать прибыль возможности: %w", methodCtx, err)
	}
	opportunity.ExpectedProfit = profit
	if opportunity.InputCost > 0 {
		opportunity.ROI = float64(opportunity.ExpectedProfit) / float64(opportunity.InputCost)
	} else {
		opportunity.ROI = 0
	}
	return opportunity, nil
}

// Score ранжирует уже проверенную возможность.
func Score(opportunity domain.TradeOpportunity, weights ScoreWeights, profitScale float64) float64 {
	if profitScale <= 0 {
		profitScale = 1
	}
	return weights.Profit*float64(opportunity.ExpectedProfit)/profitScale +
		weights.ROI*opportunity.ROI + weights.Liquidity*opportunity.LiquidityScore +
		weights.Confidence*opportunity.Confidence - weights.Risk*opportunity.PriceVolatility -
		weights.Slots*float64(opportunity.RequiredSlots)
}
