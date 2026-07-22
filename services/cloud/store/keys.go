package store

// Redis key and channel names — the wire contract between packages.
//
// These four names are the real interface between the cloud server's
// packages, and three of them have their publisher and their subscriber in
// DIFFERENT packages:
//
//	mailbox:<id>:dispatch    dispatch.attemptDispatch publishes -> link.Hub.Accept subscribes
//	mailbox:<id>:available   link.Hub.onState publishes        -> dispatch.Dispatcher psubscribes
//	job:<id>:status          link.Hub.onStatus publishes       -> handlers.StreamJob subscribes
//	mailbox:<id>:state       store.SetPrinterState writes      -> store.LookupPrinterState reads
//
// Built inline as string concatenation (as they were), a typo on one side is
// invisible: it does not fail to compile, does not fail a unit test, and logs
// nothing -- the PUBLISH simply reaches zero subscribers and the job sits in
// 'queued' forever. That silent failure is exactly what dispatch/route.go's
// slot-mismatch diagnostic exists to help operators debug, and it deserves the
// same treatment: name the contract once so the two sides cannot drift.
//
// Keep them here rather than in each package. Tests must use them too -- a test
// that hard-codes the string while the code uses the constructor is asserting
// against a coincidence, not against the contract.
//
// Format changes are breaking: a running fleet's in-flight subscriptions and
// cached state keys are all built from these, so changing one is a deployment
// concern, not a rename.

// KeyPrinterState is the cached printer-state snapshot for a mailbox unit,
// written on every register/state frame and expiring after printerStateTTL.
// Its absence -- not a closed socket -- is the system's offline signal.
func KeyPrinterState(mailboxID string) string {
	return "mailbox:" + mailboxID + ":state"
}

// ChanDispatch carries a dispatch frame to whichever node holds this mailbox's
// printer socket. Any node may publish; only the owner node is subscribed.
func ChanDispatch(mailboxID string) string {
	return "mailbox:" + mailboxID + ":dispatch"
}

// ChanAvailable announces that a printer went idle, waking the dispatcher
// goroutine on every node to re-evaluate the blocked-jobs stream.
func ChanAvailable(mailboxID string) string {
	return "mailbox:" + mailboxID + ":available"
}

// PatternAvailable is the PSUBSCRIBE pattern matching every ChanAvailable
// channel. The dispatcher uses one pattern subscription rather than one
// SUBSCRIBE per mailbox because it holds no registry of which mailboxes exist
// -- that is precisely the authoritative state plans/03-scaling.md says nodes
// must not keep.
const PatternAvailable = "mailbox:*:available"

// ChanJobStatus carries a job's status transitions from the node holding the
// printer socket to whichever node holds the sender's SSE connection -- the
// mirror image of ChanDispatch's fan-in.
func ChanJobStatus(jobID string) string {
	return "job:" + jobID + ":status"
}
