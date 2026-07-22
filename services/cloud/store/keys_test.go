package store

import "testing"

// These literals are the wire contract (see keys.go). They are written out
// here rather than derived from the constructors, so this test compares the
// constructors against an independent copy of the format -- deriving them
// would make the test tautological and it would pass through any rename.
//
// A failure here is not a broken test: it means the Redis key format changed,
// which is a deployment concern (a running fleet's live subscriptions and
// cached state keys are all built from these), not a rename. Update the
// literals only alongside a migration plan.
func TestKeyFormats_AreTheDocumentedWireContract(t *testing.T) {
	const (
		mailboxID = "8f14e45f-ceea-467a-9d3e-1a4b8d2c7e90"
		jobID     = "6f1c2a4e-9b33-4d21-8f77-0c5a1e2b7d90"
	)

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"KeyPrinterState", KeyPrinterState(mailboxID), "mailbox:8f14e45f-ceea-467a-9d3e-1a4b8d2c7e90:state"},
		{"ChanDispatch", ChanDispatch(mailboxID), "mailbox:8f14e45f-ceea-467a-9d3e-1a4b8d2c7e90:dispatch"},
		{"ChanAvailable", ChanAvailable(mailboxID), "mailbox:8f14e45f-ceea-467a-9d3e-1a4b8d2c7e90:available"},
		{"ChanJobStatus", ChanJobStatus(jobID), "job:6f1c2a4e-9b33-4d21-8f77-0c5a1e2b7d90:status"},
		{"PatternAvailable", PatternAvailable, "mailbox:*:available"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// The dispatcher PSUBSCRIBEs to PatternAvailable to reach channels that
// ChanAvailable produces. If those two ever drift apart, the dispatcher stops
// waking on idle printers -- silently, since a pattern that matches nothing is
// not an error. Pin the relationship, not just the two strings.
func TestPatternAvailable_MatchesChanAvailable(t *testing.T) {
	const prefix, suffix = "mailbox:", ":available"
	ch := ChanAvailable("any-mailbox-id")

	if got := PatternAvailable[:len(prefix)]; got != prefix {
		t.Fatalf("PatternAvailable prefix = %q, want %q", got, prefix)
	}
	if got := PatternAvailable[len(PatternAvailable)-len(suffix):]; got != suffix {
		t.Fatalf("PatternAvailable suffix = %q, want %q", got, suffix)
	}
	if ch[:len(prefix)] != prefix || ch[len(ch)-len(suffix):] != suffix {
		t.Fatalf("ChanAvailable(%q) = %q does not match pattern %q", "any-mailbox-id", ch, PatternAvailable)
	}
}
