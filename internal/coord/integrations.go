package coord

// The integrations bucket's two key classes (ADR-0175, "The integrations
// bucket declares its classes"). A surface's STATUS is the small record every
// hot reader lists or reads by its key: the duty's tick, a turn's atlas, the
// beat's mint judgement and every write's re-read under the surface's lease.
// Its FINDINGS, which grow with the company's seats, are a record of their
// own, named on the status by size and SHA-256 (internal/integration's
// FindingsRecord), so only a reader that needs them pays their crossing. A
// pass writes the findings first and then the status naming them; a reader
// whose digest does not match reads the findings as unread.
const (
	// IntegrationStatusClass keys one surface's status, `status.<kind>`.
	IntegrationStatusClass = "status"
	// IntegrationFindingsClass keys one surface's findings,
	// `findings.<kind>`.
	IntegrationFindingsClass = "findings"
)

// MaxIntegrationStatusBytes bounds one surface's status as stored, read at
// the read term: [SmallValueBytes], a 23,328-byte reply, 0.67 s at the
// slowest link pace. Its writer cuts the fault and the report's sentence to
// fit, and keeps exact what it compares (the address and the registration's
// name, each bounded in the configuration).
const MaxIntegrationStatusBytes = SmallValueBytes

// MaxIntegrationFindingsBytes bounds one surface's findings as stored. No
// reader decides a write on them, so a read is given the reply's arrival:
// 2.6 s at the floor, and a pass over all 24 surfaces at their bound about
// 46 s. Four times this (256 KiB) would hold a list four times longer and
// make that pass three minutes, too long for the dashboard's one read of
// every row. A typical list is a few KiB; past this its writer keeps the
// reported finding, cuts long subject lists and counts what it drops.
const MaxIntegrationFindingsBytes = 64 << 10

// IntegrationStatusKey is one surface's status key.
func IntegrationStatusKey(kind string) string { return DocumentKey(IntegrationStatusClass, kind) }

// IntegrationFindingsKey is one surface's findings key.
func IntegrationFindingsKey(kind string) string {
	return DocumentKey(IntegrationFindingsClass, kind)
}

// IntegrationKind recovers the surface a key of class names, reporting false
// for a key of any other shape, which names no surface.
func IntegrationKind(key, class string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != class || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}
