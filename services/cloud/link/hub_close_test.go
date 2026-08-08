package link

// Hub.Close is the shutdown half of the printer link (plans/16-kubernetes.md
// §4.1). It exists because the link is a *hijacked* connection:
// http.Server.Shutdown neither waits for hijacked conns nor closes them, so
// without this the socket dies with the process and the printer only finds out
// via a read error -- which, when a pod's network namespace vanishes rather
// than sending a FIN, can take minutes of dead air before it re-dials.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/redis/go-redis/v9"
)

// TestHubClose_SendsGoingAwayToConnectedPrinters asserts the printer sees a
// deliberate StatusGoingAway close -- the signal its dial loop treats as "this
// node is leaving, reconnect now" -- rather than an abrupt transport failure.
func TestHubClose_SendsGoingAwayToConnectedPrinters(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	hub := NewHub(rdb, nil) // Queries unused by the register path

	// Same hijacked-connection discipline as hub_integration_test.go: the
	// handler goroutine can outlive the test, so it must never touch t.
	acceptErr := make(chan error, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := hub.Accept(r.Context(), w, r); err != nil {
			select {
			case acceptErr <- err:
			default:
			}
		}
	}))
	defer httpSrv.Close()
	defer func() {
		select {
		case err := <-acceptErr:
			t.Logf("hub.Accept returned: %v", err)
		default:
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws"+httpSrv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	const mailboxID = "22222222-2222-2222-2222-222222222222"
	if err := wsjson.Write(ctx, conn, Frame{
		Type:          "register",
		MailboxID:     mailboxID,
		SlotOccupancy: map[string]SlotState{"slot-1": {Current: 0, Max: 5}},
	}); err != nil {
		t.Fatalf("write register frame: %v", err)
	}

	// Wait until the hub has actually registered the socket; Close can only
	// address connections the Registry knows about.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(hub.Registry.Conns()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(hub.Registry.Conns()); got != 1 {
		t.Fatalf("registry holds %d conns after register, want 1", got)
	}

	// The printer is parked in its read loop in production, which is what
	// lets the library echo the close frame and complete the handshake. Model
	// that here -- reading only *after* Close would stall it until the
	// library's internal 5s handshake timeout, which is a test artifact, not
	// the deployed behaviour.
	readErr := make(chan error, 1)
	go func() {
		var frame Frame
		readErr <- wsjson.Read(ctx, conn, &frame)
	}()

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	start := time.Now()
	hub.Close(closeCtx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Hub.Close took %s against a responsive printer; the close handshake should be prompt", elapsed)
	}

	// The printer's read must report a clean close with GoingAway -- not
	// io.EOF, not a reset.
	select {
	case err = <-readErr:
	case <-time.After(3 * time.Second):
		t.Fatal("printer's read never returned after Hub.Close")
	}
	if err == nil {
		t.Fatal("expected the socket to be closed, read succeeded")
	}
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read after Hub.Close = %v, want a websocket.CloseError (a deliberate close, not a severed conn)", err)
	}
	if closeErr.Code != websocket.StatusGoingAway {
		t.Fatalf("close status = %v, want %v (StatusGoingAway is what tells the printer to re-dial immediately)", closeErr.Code, websocket.StatusGoingAway)
	}
}

// TestHubClose_NoConnections is the shutdown-path guard: a node that owns no
// printer socket (the common case with N replicas and one printer) must not
// block or panic on the way out.
func TestHubClose_NoConnections(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewHub(rdb, nil).Close(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Hub.Close blocked with no connections registered")
	}
}
