package link

import (
	"sync"

	"github.com/coder/websocket"
)

// Registry is the per-node map of mailbox_id -> live socket. It is
// in-process soft state, not the source of truth: if this node restarts,
// printers reconnect (possibly to a different node) and re-register. The
// only thing that depends on a registry entry surviving is routing a
// dispatch frame to *this* node's socket while the connection is open.
//
// One Registry per cloud-server process. Looking a mailbox up here only
// tells you "is its printer connected to me" -- the authoritative
// connected/disconnected signal for the rest of the system is the Redis
// mailbox:<id>:state key (TTL-expires when nobody refreshes it), not this
// map (plans/05-cloud-server.md "GET /internal/printer-link").
type Registry struct {
	mu    sync.RWMutex
	conns map[string]*websocket.Conn
}

func NewRegistry() *Registry {
	return &Registry{conns: make(map[string]*websocket.Conn)}
}

func (r *Registry) Add(mailboxID string, conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[mailboxID] = conn
}

// Conns returns a snapshot of the live sockets. Used by Hub.Close on
// shutdown; the slice is a copy, so callers can take as long as they like
// closing connections without holding the lock (or blocking a reconnect that
// wants to Add).
func (r *Registry) Conns() []*websocket.Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	conns := make([]*websocket.Conn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	return conns
}

// Remove deletes the entry only if it still points at conn. Guards against
// a slow teardown of an old connection clobbering a newer reconnect that
// already replaced it in the map.
func (r *Registry) Remove(mailboxID string, conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[mailboxID] == conn {
		delete(r.conns, mailboxID)
	}
}
