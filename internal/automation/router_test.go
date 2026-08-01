package automation

import (
	"strings"
	"testing"
	"time"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestSpecializePathRejectsMonetaryTransition(t *testing.T) {
	_, err := specializePath(navigation.Path{{
		From:  domain.StatePurchaseDialog,
		To:    domain.StateConfirmation,
		Class: protocol.ActionPurchase,
		Action: protocol.Action{
			Kind: "CLICK", Point: &domain.Point{X: .5, Y: .5},
		},
		Verify: navigation.VerificationRule{State: domain.StateConfirmation},
	}}, nil)
	if err == nil || !strings.Contains(err.Error(), "недопустимый класс") {
		t.Fatalf("ошибка денежного пути = %v", err)
	}
}

func TestSpecializePathPreservesVerificationSettings(t *testing.T) {
	bbox := &domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .4}
	path := navigation.Path{{
		From:  domain.StateMarketHome,
		To:    domain.StateMarketSearch,
		Class: protocol.ActionNavigation,
		Action: protocol.Action{
			Kind: "TEXT", Value: "${item.name}",
		},
		Verify: navigation.VerificationRule{
			State: domain.StateMarketSearch, MinConfidence: .9,
			Timeout: 2 * time.Second, BBox: bbox,
		},
	}}

	result, err := specializePath(path, map[string]string{"item.name": "Болт"})
	if err != nil {
		t.Fatal(err)
	}
	if result[0].Action.Value != "Болт" ||
		result[0].Verify.Timeout != 2*time.Second ||
		result[0].Verify.BBox == nil ||
		*result[0].Verify.BBox != *bbox {
		t.Fatalf("специализированный путь = %#v", result)
	}
	if result[0].Verify.BBox == bbox {
		t.Fatal("specializePath retained verification bbox pointer")
	}
}

func TestSpecializeCommitPathAllowsOnlyFinalExpectedMoneyClass(t *testing.T) {
	path := navigation.Path{
		{
			From:  domain.StateMarketHome,
			To:    domain.StatePurchaseDialog,
			Class: protocol.ActionNavigation,
			Action: protocol.Action{
				Kind: "CLICK", Point: &domain.Point{X: .4, Y: .4},
			},
			Verify: navigation.VerificationRule{State: domain.StatePurchaseDialog},
		},
		{
			From:  domain.StatePurchaseDialog,
			To:    domain.StateConfirmation,
			Class: protocol.ActionPurchase,
			Action: protocol.Action{
				Kind: "CLICK", Point: &domain.Point{X: .5, Y: .5},
			},
			Verify: navigation.VerificationRule{State: domain.StateConfirmation},
		},
	}
	result, err := specializePathForClass(path, nil, protocol.ActionPurchase)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[1].Class != protocol.ActionPurchase {
		t.Fatalf("денежный путь = %#v", result)
	}
	if _, err := specializePathForClass(path, nil, protocol.ActionListing); err == nil {
		t.Fatal("ожидался отказ для неверного финального денежного класса")
	}
}
