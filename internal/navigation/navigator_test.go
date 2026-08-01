package navigation_test

import (
	"strings"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
	"github.com/arena-trading-agent/arena-trading-agent/internal/protocol"
)

func TestPathChoosesShortestRoute(t *testing.T) {
	transitions := []navigation.Transition{
		{From: domain.StateMainMenu, To: domain.StateMarketHome, Verify: navigation.VerificationRule{State: domain.StateMarketHome}},
		{From: domain.StateMarketHome, To: domain.StateItemCard, Verify: navigation.VerificationRule{State: domain.StateItemCard}},
		{From: domain.StateMainMenu, To: domain.StateContacts, Verify: navigation.VerificationRule{State: domain.StateContacts}},
		{From: domain.StateContacts, To: domain.StateContactPage, Verify: navigation.VerificationRule{State: domain.StateContactPage}},
		{From: domain.StateContactPage, To: domain.StateItemCard, Verify: navigation.VerificationRule{State: domain.StateItemCard}},
	}
	navigator, err := navigation.New(transitions)
	if err != nil {
		t.Fatal(err)
	}
	path, err := navigator.Path(domain.StateMainMenu, domain.StateItemCard)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 2 {
		t.Fatalf("длина пути = %d, ожидалось 2", len(path))
	}
}

func TestPathNeverUsesMonetaryTransitionAsShortcut(t *testing.T) {
	transitions := []navigation.Transition{
		{
			From: domain.StateMainMenu, To: domain.StateMarketHome,
			Class:  protocol.ActionNavigation,
			Verify: navigation.VerificationRule{State: domain.StateMarketHome},
		},
		{
			From: domain.StateMarketHome, To: domain.StatePurchaseDialog,
			Class: protocol.ActionPurchase,
			Action: protocol.Action{
				Kind: "CLICK", Point: &domain.Point{X: .5, Y: .5}, Value: "LEFT",
			},
			Verify: navigation.VerificationRule{State: domain.StatePurchaseDialog},
		},
		{
			From: domain.StateMainMenu, To: domain.StateContacts,
			Class:  protocol.ActionNavigation,
			Verify: navigation.VerificationRule{State: domain.StateContacts},
		},
		{
			From: domain.StateContacts, To: domain.StateContactPage,
			Class:  protocol.ActionNavigation,
			Verify: navigation.VerificationRule{State: domain.StateContactPage},
		},
		{
			From: domain.StateContactPage, To: domain.StatePurchaseDialog,
			Class:  protocol.ActionNavigation,
			Verify: navigation.VerificationRule{State: domain.StatePurchaseDialog},
		},
	}
	navigator, err := navigation.New(transitions)
	if err != nil {
		t.Fatal(err)
	}

	path, err := navigator.Path(domain.StateMainMenu, domain.StatePurchaseDialog)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 3 {
		t.Fatalf("длина безопасного пути = %d, ожидалось 3", len(path))
	}
	for index, transition := range path {
		if transition.Class != protocol.ActionNavigation {
			t.Fatalf("шаг %d имеет денежный класс %s", index+1, transition.Class)
		}
	}

	monetary, err := navigator.PathForClass(
		domain.StateMarketHome,
		domain.StatePurchaseDialog,
		protocol.ActionPurchase,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(monetary) != 1 || monetary[0].Class != protocol.ActionPurchase {
		t.Fatalf("явный денежный путь = %#v", monetary)
	}
}

func TestNavigatorRejectsUnsupportedVerificationBBox(t *testing.T) {
	bbox := &domain.Rectangle{X: .1, Y: .2, Width: .3, Height: .4}
	_, err := navigation.New([]navigation.Transition{{
		From:  domain.StateMainMenu,
		To:    domain.StateMarketHome,
		Class: protocol.ActionNavigation,
		Verify: navigation.VerificationRule{
			State: domain.StateMarketHome, MinConfidence: .9,
			BBox: bbox,
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "verification bbox должна быть пустой") {
		t.Fatalf("неподдерживаемая область проверки не отклонена: %v", err)
	}
}

func TestCommitPathHasNavigationPrefixAndExactlyOneFinalMoneyEdge(t *testing.T) {
	navigator, err := navigation.New([]navigation.Transition{
		{
			From: domain.StateMainMenu, To: domain.StateMarketHome,
			Class:  protocol.ActionNavigation,
			Verify: navigation.VerificationRule{State: domain.StateMarketHome},
		},
		{
			From: domain.StateMarketHome, To: domain.StatePurchaseDialog,
			Class:  protocol.ActionNavigation,
			Verify: navigation.VerificationRule{State: domain.StatePurchaseDialog},
		},
		{
			From: domain.StatePurchaseDialog, To: domain.StateConfirmation,
			Class: protocol.ActionPurchase,
			Action: protocol.Action{
				Kind: "CLICK", Point: &domain.Point{X: .5, Y: .5}, Value: "LEFT",
			},
			Verify: navigation.VerificationRule{State: domain.StateConfirmation},
		},
		{
			From: domain.StateMarketHome, To: domain.StateConfirmation,
			Class: protocol.ActionListing,
			Action: protocol.Action{
				Kind: "CLICK", Point: &domain.Point{X: .5, Y: .5}, Value: "LEFT",
			},
			Verify: navigation.VerificationRule{State: domain.StateConfirmation},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path, err := navigator.PathForCommit(
		domain.StateMainMenu,
		domain.StateConfirmation,
		protocol.ActionPurchase,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 3 {
		t.Fatalf("длина пути фиксации = %d, ожидалось 3", len(path))
	}
	for index, transition := range path {
		expected := protocol.ActionNavigation
		if index == len(path)-1 {
			expected = protocol.ActionPurchase
		}
		if transition.Class != expected {
			t.Fatalf("шаг %d имеет класс %s, ожидался %s", index+1, transition.Class, expected)
		}
	}
}

func TestNavigatorRejectsSequenceForMonetaryTransition(t *testing.T) {
	_, err := navigation.New([]navigation.Transition{{
		From:  domain.StatePurchaseDialog,
		To:    domain.StateConfirmation,
		Class: protocol.ActionPurchase,
		Action: protocol.Action{
			Kind: "SEQUENCE",
			Steps: []protocol.Action{
				{Kind: "TEXT", Value: "100"},
				{Kind: "CLICK", Point: &domain.Point{X: .5, Y: .5}, Value: "LEFT"},
			},
		},
		Verify: navigation.VerificationRule{State: domain.StateConfirmation},
	}})
	if err == nil {
		t.Fatal("денежная SEQUENCE не была отклонена")
	}
}
