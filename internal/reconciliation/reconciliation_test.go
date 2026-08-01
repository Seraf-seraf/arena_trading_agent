package reconciliation_test

import (
	"math"
	"reflect"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/reconciliation"
)

func TestReconcileExactMatch(t *testing.T) {
	t.Parallel()
	opportunity := domain.TradeOpportunity{
		ID:              "opportunity",
		InputCost:       100,
		ExpectedRevenue: 180,
		ExpectedFees:    20,
		ResultItemID:    "result",
		ResultQuantity:  2,
	}
	report, err := reconciliation.Reconcile(opportunity, domain.TradeResult{
		ExecutionID:    "execution",
		InputCost:      100,
		Revenue:        180,
		Fees:           20,
		ResultItemID:   "result",
		ResultQuantity: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Matched || report.Expected.Profit != 60 || report.Actual.Profit != 60 ||
		len(report.Mismatches) != 0 {
		t.Fatalf("unexpected exact report: %+v", report)
	}
}

func TestReconcileReportsStableVariancesAndMismatches(t *testing.T) {
	t.Parallel()
	report, err := reconciliation.Reconcile(domain.TradeOpportunity{
		ID:              "opportunity",
		InputCost:       100,
		ExpectedRevenue: 200,
		ExpectedFees:    20,
		ResultItemID:    "expected",
		ResultQuantity:  2,
	}, domain.TradeResult{
		InputCost:      110,
		Revenue:        190,
		Fees:           25,
		ResultItemID:   "actual",
		ResultQuantity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Matched ||
		report.InputCostVariance != 10 ||
		report.RevenueVariance != -10 ||
		report.FeesVariance != 5 ||
		report.ProfitVariance != -25 {
		t.Fatalf("unexpected variances: %+v", report)
	}
	var fields []string
	for _, mismatch := range report.Mismatches {
		fields = append(fields, mismatch.Field)
	}
	want := []string{"input_cost", "revenue", "fees", "profit", "result_item", "result_quantity"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("mismatch order = %v, want %v", fields, want)
	}
}

func TestReconcileRejectsOverflow(t *testing.T) {
	t.Parallel()
	_, err := reconciliation.Reconcile(domain.TradeOpportunity{}, domain.TradeResult{
		InputCost: math.MaxInt64,
		Fees:      math.MaxInt64,
	})
	if err == nil {
		t.Fatal("reconciliation accepted profit underflow")
	}
}
