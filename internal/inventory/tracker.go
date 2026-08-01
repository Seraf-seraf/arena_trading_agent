// Package inventory maintains a verified, concurrency-safe inventory view.
package inventory

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/arena-trading-agent/arena-trading-agent/internal/domain"
	"github.com/arena-trading-agent/arena-trading-agent/internal/money"
)

type trackedItem struct {
	quantity int64
	reserved int64
	slots    int
}

// Tracker applies observations and reservations atomically. It never infers
// slot changes because stack rules are game data, not a safe domain guess.
type Tracker struct {
	mu           sync.RWMutex
	capacity     int
	used         int
	revision     uint64
	items        map[string]trackedItem
	reservations map[string]map[string]int64
}

// NewTracker creates an empty tracker with a fixed slot capacity.
func NewTracker(capacitySlots int) (*Tracker, error) {
	const methodCtx = "inventory.NewTracker"

	if capacitySlots < 0 {
		return nil, fmt.Errorf("%s: вместимость инвентаря не может быть отрицательной", methodCtx)
	}
	return &Tracker{
		capacity:     capacitySlots,
		items:        make(map[string]trackedItem),
		reservations: make(map[string]map[string]int64),
	}, nil
}

// Replace atomically installs a complete verified observation and clears all
// old reservations. Observations cannot carry anonymous reservations.
func (t *Tracker) Replace(items []domain.InventoryItem) error {
	const methodCtx = "inventory.Tracker.Replace"

	if t == nil {
		return fmt.Errorf("%s: трекер инвентаря не задан", methodCtx)
	}
	t.mu.RLock()
	capacity := t.capacity
	t.mu.RUnlock()
	if err := t.replace(capacity, -1, items); err != nil {
		return fmt.Errorf("%s: не удалось заменить содержимое инвентаря: %w", methodCtx, err)
	}
	return nil
}

// ReplaceSnapshot atomically installs a complete verified inventory snapshot,
// including the observed capacity. It is used only at a synchronization
// boundary before a new saga; active saga checkpoints restore the same data.
func (t *Tracker) ReplaceSnapshot(snapshot domain.InventorySnapshot) error {
	const methodCtx = "inventory.Tracker.ReplaceSnapshot"

	if t == nil {
		return fmt.Errorf("%s: трекер инвентаря не задан", methodCtx)
	}
	if snapshot.CapacitySlots < 0 || snapshot.UsedSlots < 0 ||
		snapshot.UsedSlots > snapshot.CapacitySlots {
		return fmt.Errorf(
			"%s: снимок содержит некорректную вместимость %d или занятое число слотов %d",
			methodCtx,
			snapshot.CapacitySlots,
			snapshot.UsedSlots,
		)
	}
	if err := t.replace(snapshot.CapacitySlots, snapshot.UsedSlots, snapshot.Items); err != nil {
		return fmt.Errorf("%s: не удалось установить снимок инвентаря: %w", methodCtx, err)
	}
	return nil
}

func (t *Tracker) replace(capacity, expectedUsed int, items []domain.InventoryItem) error {
	const methodCtx = "inventory.Tracker.replace"

	next := make(map[string]trackedItem, len(items))
	var used int
	for _, item := range items {
		if item.ItemID == "" {
			return fmt.Errorf("%s: идентификатор предмета инвентаря пуст", methodCtx)
		}
		if _, duplicate := next[item.ItemID]; duplicate {
			return fmt.Errorf("%s: предмет инвентаря %q продублирован", methodCtx, item.ItemID)
		}
		if item.Quantity <= 0 || item.Slots <= 0 {
			return fmt.Errorf("%s: предмет %q имеет некорректное количество или число слотов", methodCtx, item.ItemID)
		}
		if item.ReservedQuantity != 0 {
			return fmt.Errorf("%s: наблюдение инвентаря не может восстановить анонимные резервы", methodCtx)
		}
		var err error
		used, err = addInt(used, item.Slots)
		if err != nil {
			return fmt.Errorf("%s: переполнение числа слотов инвентаря: %w", methodCtx, err)
		}
		next[item.ItemID] = trackedItem{quantity: item.Quantity, slots: item.Slots}
	}
	if expectedUsed >= 0 && used != expectedUsed {
		return fmt.Errorf(
			"%s: сумма слотов предметов %d не совпадает с used_slots=%d",
			methodCtx,
			used,
			expectedUsed,
		)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if used > capacity {
		return fmt.Errorf("%s: инвентарь занимает %d слотов при вместимости %d", methodCtx, used, capacity)
	}
	if t.revision == math.MaxUint64 {
		return fmt.Errorf("%s: переполнение ревизии инвентаря", methodCtx)
	}
	t.capacity = capacity
	t.items = next
	t.used = used
	t.reservations = make(map[string]map[string]int64)
	t.revision++
	return nil
}

// Apply applies every signed delta or none. Deltas for the same item are
// aggregated in ItemID order so the outcome and errors do not depend on map
// iteration.
func (t *Tracker) Apply(deltas ...domain.InventoryDelta) error {
	const methodCtx = "inventory.Tracker.Apply"

	if t == nil {
		return fmt.Errorf("%s: трекер инвентаря не задан", methodCtx)
	}
	type aggregate struct {
		quantity int64
		slots    int
	}
	ordered := append([]domain.InventoryDelta(nil), deltas...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ItemID != ordered[j].ItemID {
			return ordered[i].ItemID < ordered[j].ItemID
		}
		if ordered[i].QuantityDelta != ordered[j].QuantityDelta {
			return ordered[i].QuantityDelta < ordered[j].QuantityDelta
		}
		return ordered[i].SlotsDelta < ordered[j].SlotsDelta
	})
	changes := make(map[string]aggregate, len(deltas))
	for _, delta := range ordered {
		if delta.ItemID == "" {
			return fmt.Errorf("%s: идентификатор предмета в изменении инвентаря пуст", methodCtx)
		}
		value := changes[delta.ItemID]
		var err error
		value.quantity, err = money.Add(value.quantity, delta.QuantityDelta)
		if err != nil {
			return fmt.Errorf("%s: переполнение изменения количества предмета %q: %w", methodCtx, delta.ItemID, err)
		}
		value.slots, err = addInt(value.slots, delta.SlotsDelta)
		if err != nil {
			return fmt.Errorf("%s: переполнение изменения слотов предмета %q: %w", methodCtx, delta.ItemID, err)
		}
		changes[delta.ItemID] = value
	}
	ids := make([]string, 0, len(changes))
	for itemID := range changes {
		ids = append(ids, itemID)
	}
	sort.Strings(ids)

	t.mu.Lock()
	defer t.mu.Unlock()
	next := make(map[string]trackedItem, len(t.items)+len(changes))
	for itemID, item := range t.items {
		next[itemID] = item
	}
	used := t.used
	for _, itemID := range ids {
		change := changes[itemID]
		item := next[itemID]
		var err error
		item.quantity, err = money.Add(item.quantity, change.quantity)
		if err != nil {
			return fmt.Errorf("%s: переполнение количества предмета %q: %w", methodCtx, itemID, err)
		}
		item.slots, err = addInt(item.slots, change.slots)
		if err != nil {
			return fmt.Errorf("%s: переполнение числа слотов предмета %q: %w", methodCtx, itemID, err)
		}
		used, err = addInt(used, change.slots)
		if err != nil {
			return fmt.Errorf("%s: переполнение числа занятых слотов инвентаря: %w", methodCtx, err)
		}
		if item.quantity < 0 || item.slots < 0 {
			return fmt.Errorf("%s: изменение инвентаря делает значения предмета %q отрицательными", methodCtx, itemID)
		}
		if item.reserved > item.quantity {
			return fmt.Errorf("%s: изменение инвентаря расходует зарезервированный предмет %q", methodCtx, itemID)
		}
		if (item.quantity == 0) != (item.slots == 0) {
			return fmt.Errorf("%s: количество и слоты предмета %q должны одновременно стать нулевыми", methodCtx, itemID)
		}
		if item.quantity == 0 {
			delete(next, itemID)
		} else {
			next[itemID] = item
		}
	}
	if used < 0 || used > t.capacity {
		return fmt.Errorf("%s: занято %d слотов, что выходит за вместимость %d", methodCtx, used, t.capacity)
	}
	if t.revision == math.MaxUint64 {
		return fmt.Errorf("%s: переполнение ревизии инвентаря", methodCtx)
	}
	t.items = next
	t.used = used
	t.revision++
	return nil
}

// Reserve marks recipe ingredients unavailable to other plans.
func (t *Tracker) Reserve(reservationID string, ingredients []domain.BarterIngredient) error {
	const methodCtx = "inventory.Tracker.Reserve"

	if t == nil {
		return fmt.Errorf("%s: трекер инвентаря не задан", methodCtx)
	}
	if reservationID == "" {
		return fmt.Errorf("%s: идентификатор резерва пуст", methodCtx)
	}
	required, ids, err := aggregateRequirements(ingredients)
	if err != nil {
		return fmt.Errorf("%s: не удалось агрегировать требования резерва: %w", methodCtx, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.reservations[reservationID]; exists {
		return fmt.Errorf("%s: резерв %q уже существует", methodCtx, reservationID)
	}
	for _, itemID := range ids {
		item := t.items[itemID]
		available, err := money.Subtract(item.quantity, item.reserved)
		if err != nil {
			return fmt.Errorf("%s: не удалось вычислить доступное количество предмета %q: %w", methodCtx, itemID, err)
		}
		if available < required[itemID] {
			return fmt.Errorf(
				"%s: недостаточно предмета %q: требуется %d, доступно %d",
				methodCtx,
				itemID,
				required[itemID],
				available,
			)
		}
	}
	if t.revision == math.MaxUint64 {
		return fmt.Errorf("%s: переполнение ревизии инвентаря", methodCtx)
	}
	for _, itemID := range ids {
		item := t.items[itemID]
		item.reserved += required[itemID]
		t.items[itemID] = item
	}
	t.reservations[reservationID] = required
	t.revision++
	return nil
}

// Release removes an existing reservation.
func (t *Tracker) Release(reservationID string) error {
	const methodCtx = "inventory.Tracker.Release"

	if t == nil {
		return fmt.Errorf("%s: трекер инвентаря не задан", methodCtx)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	reservation, exists := t.reservations[reservationID]
	if !exists {
		return fmt.Errorf("%s: резерв %q не существует", methodCtx, reservationID)
	}
	if t.revision == math.MaxUint64 {
		return fmt.Errorf("%s: переполнение ревизии инвентаря", methodCtx)
	}
	for itemID, quantity := range reservation {
		item := t.items[itemID]
		item.reserved -= quantity
		t.items[itemID] = item
	}
	delete(t.reservations, reservationID)
	t.revision++
	return nil
}

// Available returns unreserved quantity for one item.
func (t *Tracker) Available(itemID string) int64 {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	item := t.items[itemID]
	return item.quantity - item.reserved
}

// FreeSlots returns currently unoccupied slots.
func (t *Tracker) FreeSlots() int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.capacity - t.used
}

// Snapshot returns a defensive, ItemID-sorted copy.
func (t *Tracker) Snapshot() domain.InventorySnapshot {
	if t == nil {
		return domain.InventorySnapshot{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	ids := make([]string, 0, len(t.items))
	for itemID := range t.items {
		ids = append(ids, itemID)
	}
	sort.Strings(ids)
	items := make([]domain.InventoryItem, 0, len(ids))
	for _, itemID := range ids {
		item := t.items[itemID]
		items = append(items, domain.InventoryItem{
			ItemID:           itemID,
			Quantity:         item.quantity,
			ReservedQuantity: item.reserved,
			Slots:            item.slots,
		})
	}
	return domain.InventorySnapshot{
		CapacitySlots: t.capacity,
		UsedSlots:     t.used,
		Revision:      t.revision,
		Items:         items,
	}
}

func aggregateRequirements(
	ingredients []domain.BarterIngredient,
) (map[string]int64, []string, error) {
	const methodCtx = "inventory.aggregateRequirements"

	if len(ingredients) == 0 {
		return nil, nil, fmt.Errorf("%s: резерв не содержит ингредиентов", methodCtx)
	}
	required := make(map[string]int64, len(ingredients))
	for _, ingredient := range ingredients {
		if ingredient.ItemID == "" || ingredient.Quantity <= 0 {
			return nil, nil, fmt.Errorf("%s: резерв содержит некорректный ингредиент", methodCtx)
		}
		var err error
		required[ingredient.ItemID], err = money.Add(
			required[ingredient.ItemID],
			ingredient.Quantity,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: переполнение количества предмета %q в резерве: %w", methodCtx, ingredient.ItemID, err)
		}
	}
	ids := make([]string, 0, len(required))
	for itemID := range required {
		ids = append(ids, itemID)
	}
	sort.Strings(ids)
	return required, ids, nil
}

func addInt(left, right int) (int, error) {
	const methodCtx = "inventory.addInt"

	if right > 0 && left > math.MaxInt-right {
		return 0, fmt.Errorf("%s: переполнение int при сложении", methodCtx)
	}
	if right < 0 && left < math.MinInt-right {
		return 0, fmt.Errorf("%s: выход ниже диапазона int при сложении", methodCtx)
	}
	return left + right, nil
}
