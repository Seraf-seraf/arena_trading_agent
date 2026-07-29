// Package economy выполняет детерминированные денежные расчёты.
package economy

import (
	"fmt"
	"math"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
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
	if salePrice < 0 || saleCommission < 0 || listingFee < 0 || purchasePrice < 0 {
		return 0, fmt.Errorf("денежные значения не могут быть отрицательными")
	}
	return salePrice - saleCommission - listingFee - purchasePrice, nil
}

// BarterCost рассчитывает стоимость всех ингредиентов обмена.
func BarterCost(prices map[string]int64, ingredients []domain.BarterIngredient) (int64, error) {
	var total int64
	for _, ingredient := range ingredients {
		price, ok := prices[ingredient.ItemID]
		if !ok {
			return 0, fmt.Errorf("неизвестна цена ингредиента %q", ingredient.ItemID)
		}
		if price < 0 || ingredient.Quantity <= 0 || price > math.MaxInt64/ingredient.Quantity {
			return 0, fmt.Errorf("некорректная стоимость ингредиента %q", ingredient.ItemID)
		}
		line := price * ingredient.Quantity
		if total > math.MaxInt64-line {
			return 0, fmt.Errorf("стоимость обмена превышает допустимое значение")
		}
		total += line
	}
	return total, nil
}

// Complete рассчитывает производные поля возможности без округления денег.
func Complete(opportunity domain.TradeOpportunity) (domain.TradeOpportunity, error) {
	if opportunity.InputCost < 0 || opportunity.ExpectedRevenue < 0 || opportunity.ExpectedFees < 0 {
		return opportunity, fmt.Errorf("денежные значения возможности не могут быть отрицательными")
	}
	opportunity.ExpectedProfit = opportunity.ExpectedRevenue - opportunity.ExpectedFees - opportunity.InputCost
	if opportunity.InputCost > 0 {
		opportunity.ROI = float64(opportunity.ExpectedProfit) / float64(opportunity.InputCost)
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
