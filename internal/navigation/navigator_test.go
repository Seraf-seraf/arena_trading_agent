package navigation_test

import (
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/navigation"
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
