package dispatch

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newFakeRedis starts a miniredis with its clock *pinned* and returns a client
// for it. Every test in this package should use it rather than calling
// miniredis.RunT directly.
//
// Why pin the clock. miniredis reads the wall clock as time.Now().UTC(), which
// drops Go's monotonic reading, and its XPENDING implementation then filters
// each PEL entry on `now.Sub(entry.lastDelivery) >= idle` -- with idle == 0
// when the command carries no IDLE argument, which is how go-redis sends
// XPendingExt here. An entry whose delivery time is *ahead* of that wall clock
// yields a negative duration and is silently dropped from the reply. Real Redis
// applies no idle filter at all unless IDLE is given, so this is a divergence
// in the fake, not behaviour worth reproducing.
//
// Wall clocks do run backwards. This project's dev environment (WSL2) steps
// CLOCK_REALTIME back ~1.8s every ~30s as it resyncs with the host, and any
// XPENDING that lands in such a step reports an empty PEL for an entry that is
// very much still pending. That is not only an assertion hazard: production
// deleteConsumerIfDrained probes the PEL the same way, and against the fake it
// would then XGROUP DELCONSUMER a consumer that still owes entries -- the exact
// job-destroying case consumer_lifecycle_test.go exists to forbid.
// TestDispatcher_BlockedRetryStaysPendingWithoutDuplicating failed once per
// backward step (~1 run in 75 locally), and is the likeliest explanation for
// the intermittent red `Coverage floor` CI job.
//
// Pinning makes that arithmetic exactly zero instead of occasionally negative.
// Nothing here needs the fake's clock to advance: claimMinIdle (60s) and
// reapMinIdle (5m) are far beyond any unit test's runtime, so the idle-driven
// XAUTOCLAIM and reaper paths are pinned against real Redis in the
// -tags=integration suite instead. A test that ever does need time to pass must
// move the fake forward explicitly with mr.FastForward.
func newFakeRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(time.Now())
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return mr, rdb
}
