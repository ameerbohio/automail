package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// printerStateTTL matches plans/05-cloud-server.md: the cache entry
// expires 90s after the last register/state frame. A printer that holds
// its socket open but stops sending state frames (frozen, not just
// disconnected) still ages out of "known good" -- TTL expiry is the
// offline signal, not just a closed socket.
const printerStateTTL = 90 * time.Second

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

// LookupPrinterState reads the cached printer state for a mailbox, reporting
// whether the key was present. A missing key (found=false) means no live
// printer: either none has ever registered or the 90s TTL lapsed since the
// last frame -- the offline signal (see printerStateTTL). Callers that must
// distinguish "offline" from "idle" (the ops dashboard, plans/07) use this;
// GetPrinterState wraps it with the Phase 2 always-idle default for the
// dispatch path, which treats a missing key as an empty-but-usable printer.
func LookupPrinterState(ctx context.Context, rdb *redis.Client, mailboxID string) (state PrinterState, found bool, err error) {
	val, err := rdb.Get(ctx, "mailbox:"+mailboxID+":state").Result()
	if err == redis.Nil {
		return PrinterState{}, false, nil
	}
	if err != nil {
		return PrinterState{}, false, err
	}
	if err := json.Unmarshal([]byte(val), &state); err != nil {
		return PrinterState{}, false, err
	}
	return state, true, nil
}

// GetPrinterState reads the cached printer state for a mailbox. No printer
// has ever connected before Phase 3 builds /internal/printer-link, so a
// missing key is not an error -- it stubs to idle with no slot data,
// exactly as the roadmap's Phase 2 task calls for. Real dispatch logic
// (Phase 4) will need to treat "idle with no slot entry for this slot" as
// "unknown capacity" rather than always-available, but Phase 2 skips
// dispatch entirely so that distinction doesn't matter yet.
func GetPrinterState(ctx context.Context, rdb *redis.Client, mailboxID string) (PrinterState, error) {
	state, found, err := LookupPrinterState(ctx, rdb, mailboxID)
	if err != nil {
		return PrinterState{}, err
	}
	if !found {
		return PrinterState{Status: "idle", SlotOccupancy: map[string]SlotState{}}, nil
	}
	return state, nil
}

// SetPrinterState writes the cache entry at mailbox:<id>:state with a 90s
// TTL. Called once on register (seeding the cache) and again on every
// subsequent state frame (plans/05-cloud-server.md "State frames refresh
// the printer cache"). The TTL -- not the registry/socket -- is the
// system's offline signal: a frozen-but-connected printer that stops
// sending frames still ages out.
func SetPrinterState(ctx context.Context, rdb *redis.Client, mailboxID string, state PrinterState) error {
	val, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return rdb.Set(ctx, "mailbox:"+mailboxID+":state", val, printerStateTTL).Err()
}
