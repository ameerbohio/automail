package link

import (
	"testing"

	"nhooyr.io/websocket"
)

func TestRegistry_AddAndRemove(t *testing.T) {
	r := NewRegistry()
	conn := &websocket.Conn{} // never dialed; only used as a map key/value here

	r.Add("mailbox-1", conn)
	r.mu.RLock()
	_, ok := r.conns["mailbox-1"]
	r.mu.RUnlock()
	if !ok {
		t.Fatal("expected mailbox-1 to be present after Add")
	}

	r.Remove("mailbox-1", conn)
	r.mu.RLock()
	_, ok = r.conns["mailbox-1"]
	r.mu.RUnlock()
	if ok {
		t.Fatal("expected mailbox-1 to be removed")
	}
}

// TestRegistry_RemoveDoesNotClobberNewerConn guards the "only remove if
// it still points at this conn" check: a slow teardown of an old socket
// must not delete a newer reconnect that already replaced it.
func TestRegistry_RemoveDoesNotClobberNewerConn(t *testing.T) {
	r := NewRegistry()
	oldConn := &websocket.Conn{}
	newConn := &websocket.Conn{}

	r.Add("mailbox-1", oldConn)
	r.Add("mailbox-1", newConn) // simulates a reconnect replacing the entry

	r.Remove("mailbox-1", oldConn) // late teardown of the old socket

	r.mu.RLock()
	got, ok := r.conns["mailbox-1"]
	r.mu.RUnlock()
	if !ok {
		t.Fatal("expected mailbox-1 to still be registered (newer conn)")
	}
	if got != newConn {
		t.Fatalf("expected registry to retain newConn, got a different value")
	}
}
