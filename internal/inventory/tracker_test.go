package inventory_test

import (
	"reflect"
	"sync"
	"testing"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/inventory"
)

func TestTrackerReplaceApplyAndDeterministicSnapshot(t *testing.T) {
	t.Parallel()
	tracker, err := inventory.NewTracker(10)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Replace([]domain.InventoryItem{
		{ItemID: "z", Quantity: 2, Slots: 1},
		{ItemID: "a", Quantity: 5, Slots: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Apply(
		domain.InventoryDelta{ItemID: "new", QuantityDelta: 2, SlotsDelta: 1},
		domain.InventoryDelta{ItemID: "a", QuantityDelta: -2},
		domain.InventoryDelta{ItemID: "new", QuantityDelta: 1},
	); err != nil {
		t.Fatal(err)
	}
	snapshot := tracker.Snapshot()
	want := []domain.InventoryItem{
		{ItemID: "a", Quantity: 3, Slots: 2},
		{ItemID: "new", Quantity: 3, Slots: 1},
		{ItemID: "z", Quantity: 2, Slots: 1},
	}
	if !reflect.DeepEqual(snapshot.Items, want) {
		t.Fatalf("items = %+v, want %+v", snapshot.Items, want)
	}
	if snapshot.UsedSlots != 4 || tracker.FreeSlots() != 6 || snapshot.Revision != 2 {
		t.Fatalf("unexpected snapshot metadata: %+v", snapshot)
	}
}

func TestTrackerApplyIsAtomic(t *testing.T) {
	t.Parallel()
	tracker, _ := inventory.NewTracker(2)
	if err := tracker.Replace([]domain.InventoryItem{{ItemID: "a", Quantity: 1, Slots: 1}}); err != nil {
		t.Fatal(err)
	}
	before := tracker.Snapshot()
	err := tracker.Apply(
		domain.InventoryDelta{ItemID: "a", QuantityDelta: -1, SlotsDelta: -1},
		domain.InventoryDelta{ItemID: "b", QuantityDelta: 1, SlotsDelta: 3},
	)
	if err == nil {
		t.Fatal("expected capacity error")
	}
	if after := tracker.Snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed atomic update changed state: before=%+v after=%+v", before, after)
	}
}

func TestTrackerReplaceSnapshotUpdatesObservedCapacityAtomically(t *testing.T) {
	t.Parallel()
	tracker, err := inventory.NewTracker(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.ReplaceSnapshot(domain.InventorySnapshot{
		CapacitySlots: 5,
		UsedSlots:     2,
		Items: []domain.InventoryItem{{
			ItemID: "a", Quantity: 3, Slots: 2,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	got := tracker.Snapshot()
	if got.CapacitySlots != 5 || got.UsedSlots != 2 || tracker.FreeSlots() != 3 {
		t.Fatalf("снимок после синхронизации = %+v", got)
	}

	before := got
	err = tracker.ReplaceSnapshot(domain.InventorySnapshot{
		CapacitySlots: 7,
		UsedSlots:     1,
		Items: []domain.InventoryItem{{
			ItemID: "a", Quantity: 3, Slots: 2,
		}},
	})
	if err == nil {
		t.Fatal("ожидался отказ из-за несовпадения used_slots")
	}
	if after := tracker.Snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("отклонённый снимок изменил состояние: до=%+v после=%+v", before, after)
	}
}

func TestTrackerReservations(t *testing.T) {
	t.Parallel()
	tracker, _ := inventory.NewTracker(4)
	if err := tracker.Replace([]domain.InventoryItem{
		{ItemID: "a", Quantity: 5, Slots: 1},
		{ItemID: "b", Quantity: 2, Slots: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Reserve("trade-1", []domain.BarterIngredient{
		{ItemID: "a", Quantity: 2},
		{ItemID: "a", Quantity: 1},
		{ItemID: "b", Quantity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if tracker.Available("a") != 2 || tracker.Available("b") != 1 {
		t.Fatalf("unexpected available quantities: a=%d b=%d", tracker.Available("a"), tracker.Available("b"))
	}
	before := tracker.Snapshot()
	if err := tracker.Reserve("trade-2", []domain.BarterIngredient{{ItemID: "a", Quantity: 3}}); err == nil {
		t.Fatal("reservation exceeded available quantity")
	}
	if after := tracker.Snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatal("failed reservation changed state")
	}
	if err := tracker.Apply(domain.InventoryDelta{ItemID: "a", QuantityDelta: -3}); err == nil {
		t.Fatal("delta consumed a reserved quantity")
	}
	if err := tracker.Release("trade-1"); err != nil {
		t.Fatal(err)
	}
	if tracker.Available("a") != 5 {
		t.Fatalf("available a = %d, want 5", tracker.Available("a"))
	}
}

func TestTrackerConcurrentUpdatesAreRaceFree(t *testing.T) {
	tracker, _ := inventory.NewTracker(64)
	const workers = 32
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			itemID := string(rune('A' + index))
			if err := tracker.Apply(domain.InventoryDelta{
				ItemID:        itemID,
				QuantityDelta: 1,
				SlotsDelta:    1,
			}); err != nil {
				t.Errorf("Apply: %v", err)
			}
			_ = tracker.Snapshot()
		}()
	}
	group.Wait()
	snapshot := tracker.Snapshot()
	if len(snapshot.Items) != workers || snapshot.UsedSlots != workers {
		t.Fatalf("lost concurrent update: %+v", snapshot)
	}
}
