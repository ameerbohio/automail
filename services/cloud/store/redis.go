package store

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
)

// SlotState mirrors the slot_occupancy shape the printer reports in its
// "register"/"state" frames (plans/09-api-contracts.md). Phase 3 starts
// populating this for real; Phase 2 only reads it.
type SlotState struct {
	Current int `json:"current"`
	Max     int `json:"max"`
}

// PrinterState is the cached snapshot at Redis key mailbox:<id>:state.
type PrinterState struct {
	Status        string               `json:"status"` // idle | printing
	SlotOccupancy map[string]SlotState `json:"slot_occupancy"`
}

// GetPrinterState reads the cached printer state for a mailbox. No printer
// has ever connected before Phase 3 builds /internal/printer-link, so a
// missing key is not an error -- it stubs to idle with no slot data,
// exactly as the roadmap's Phase 2 task calls for. Real dispatch logic
// (Phase 4) will need to treat "idle with no slot entry for this slot" as
// "unknown capacity" rather than always-available, but Phase 2 skips
// dispatch entirely so that distinction doesn't matter yet.
func GetPrinterState(ctx context.Context, rdb *redis.Client, mailboxID string) (PrinterState, error) {
	val, err := rdb.Get(ctx, "mailbox:"+mailboxID+":state").Result()
	if err == redis.Nil {
		return PrinterState{Status: "idle", SlotOccupancy: map[string]SlotState{}}, nil
	}
	if err != nil {
		return PrinterState{}, err
	}
	var state PrinterState
	if err := json.Unmarshal([]byte(val), &state); err != nil {
		return PrinterState{}, err
	}
	return state, nil
}
