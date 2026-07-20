package main

import "testing"

// newSlotState must key occupancy under the given slot id (plans/04: slot_occupancy
// is keyed by "<slot_id>", which a real deployment sets to the DB mailbox_slots.id).
func TestNewSlotState_UsesConfiguredSlotID(t *testing.T) {
	const id = "22222222-2222-2222-2222-222222222222"
	st := newSlotState(id)

	status, slots := st.snapshot()
	if status != "idle" {
		t.Fatalf("fresh state status = %q, want idle", status)
	}
	info, ok := slots[id]
	if !ok {
		t.Fatalf("snapshot missing configured slot id %q; keys=%v", id, slots)
	}
	if info.Current != 0 || info.Max != 5 {
		t.Fatalf("slot %q = %+v, want {0 5}", id, info)
	}
	if _, stale := slots["slot-1"]; stale {
		t.Fatalf("occupancy still keyed by hardcoded slot-1")
	}
}

// recordDelivery bumps the configured slot and caps at max.
func TestRecordDelivery_IncrementsAndCaps(t *testing.T) {
	const id = "slot-x"
	st := newSlotState(id)

	for i := 1; i <= 7; i++ {
		_, slots := st.recordDelivery(id)
		want := i
		if want > 5 {
			want = 5 // capped at max
		}
		if got := slots[id].Current; got != want {
			t.Fatalf("after %d deliveries, current=%d want %d", i, got, want)
		}
	}
}

// loadConfig defaults SLOT_ID to "slot-1" and honors an override.
func TestLoadConfig_SlotID(t *testing.T) {
	setRequired := func(t *testing.T) {
		t.Helper()
		t.Setenv("MAILBOX_ID", "m")
		t.Setenv("CLOUD_SERVER_WS_URL", "wss://x/internal/printer-link")
		t.Setenv("MTLS_CA_CERT_PATH", "/ca")
		t.Setenv("MTLS_CERT_PATH", "/cert")
		t.Setenv("MTLS_KEY_PATH", "/key")
		t.Setenv("PRINTER_PRIVATE_KEY_PATH", "/doc")
	}

	t.Run("default", func(t *testing.T) {
		setRequired(t)
		t.Setenv("SLOT_ID", "")
		if got := loadConfig().SlotID; got != "slot-1" {
			t.Fatalf("default SlotID = %q, want slot-1", got)
		}
	})

	t.Run("override", func(t *testing.T) {
		setRequired(t)
		t.Setenv("SLOT_ID", "22222222-2222-2222-2222-222222222222")
		if got := loadConfig().SlotID; got != "22222222-2222-2222-2222-222222222222" {
			t.Fatalf("overridden SlotID = %q", got)
		}
	})
}
