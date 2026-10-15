package host

import (
	"bufio"
	"context"
	"io"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SEAMS THE TABLE TESTS AND THE SPOOL TESTS READ. Every duration a case
// scales is Options.Timing; what is here is sizes, so a case need not build
// megabytes of fixture to cross them.

// Section is a push's command section as readCommands read it.
type Section struct {
	Raw      []byte
	Commands [][3]string // old, new, ref
	Empty    bool
	Caps     Capabilities
}

// Capabilities are what a section's lines asked for, as readCommands read
// them.
type Capabilities struct {
	ReportStatus, ReportStatusV2, SideBand64k, Atomic, Quiet bool
}

// ReadCommands is readCommands over r.
func ReadCommands(r io.Reader, maxBytes, maxCommands int) (Section, error) {
	s, err := readCommands(bufio.NewReader(r), maxBytes, maxCommands)
	out := Section{Raw: s.Raw, Empty: s.Empty(), Caps: Capabilities{ReportStatus: s.Caps.reportStatus,
		ReportStatusV2: s.Caps.reportStatusV2, SideBand64k: s.Caps.sideBand64k, Atomic: s.Caps.atomic,
		Quiet: s.Caps.quiet}}
	for _, c := range s.Commands {
		out.Commands = append(out.Commands, [3]string{c.Old, c.New, c.Ref})
	}
	return out, err
}

// The command section's refusals.
var (
	ErrSectionTooLarge = errSectionTooLarge
	ErrTooManyCommands = errTooManyCommands
	ErrPushCertificate = errPushCertificate
)

// IdxOIDs is idxOIDs.
func IdxOIDs(path string) (int64, io.ReadCloser, error) { return idxOIDs(path) }

// SetRequestMemory sets how much of a fetch request s holds in memory before
// it spools the rest (RequestMemoryBytes in production).
func SetRequestMemory(s *Server, n int) { s.requestMemory = n }

// SetSpools replaces s's spool allowance: limit bytes, share bytes and
// perCredential spools a credential may hold at once.
func SetSpools(s *Server, limit, share int64, perCredential int) {
	s.spools = newSpools(limit, share, perCredential)
}

// SpoolsInUse is the bytes and the spools s's allowance holds right now.
func SpoolsInUse(s *Server) (int64, int) { return s.spools.inUse() }

// SetRequestMemoryAllowance replaces s's request memory allowance.
func SetRequestMemoryAllowance(s *Server, limit int64) { s.heads = newAllowance(limit) }

// RequestMemoryInUse is the bytes s's request memory allowance holds now.
func RequestMemoryInUse(s *Server) int64 { return s.heads.inUse() }

// ProtectAfterForce is protectAfterForce over rules, every command in judged
// as the receive handler judged it: how many commands it asked.
func ProtectAfterForce(cmds []code.Command, rules []code.Protection, judged []code.Command) (int, error) {
	parsed, err := code.ParseProtections(rules)
	if err != nil {
		return 0, err
	}
	seen := map[judgedKey]bool{}
	for _, c := range judged {
		seen[judgedKey{c.Ref, c.Old, c.New}] = true
	}
	return protectAfterForce(cmds, parsed, seen), nil
}

// TestLanding is a landing (land.go) as a case forms one — a push the relay
// never saw, its pack written into its scratch by the case.
type TestLanding struct {
	Synced     cache.Synced
	Scratch    *cache.Scratch
	Commands   []code.Command
	Atomic     bool
	Principal  code.Principal
	PushID     string
	ReceivedAt time.Time
	// Judged are the commands already asked of Synced.Repo's protections,
	// as they stand; nil is none.
	Judged []code.Command
}

// TestLanded is what a landing came to: its commands, the record's outcome
// as the push's audit records it, and whether its pack was adopted.
type TestLanded struct {
	Commands  []code.Command
	Outcome   types.AuditOutcome
	Refusal   AuditRefusal
	Position  statelog.Position
	OK        int
	PackBytes int64
	Adopted   bool
}

// Land is s's land over l, with nobody listening to its progress.
func Land(ctx context.Context, s *Server, l TestLanding) (TestLanded, error) {
	var judged map[judgedKey]bool
	if l.Judged != nil {
		judged = map[judgedKey]bool{}
		for _, c := range l.Judged {
			judged[judgedKey{c.Ref, c.Old, c.New}] = true
		}
	}
	ld, err := s.land(ctx, landing{Synced: l.Synced, Scratch: l.Scratch, Commands: l.Commands, Atomic: l.Atomic,
		Principal: l.Principal, PushID: l.PushID, ReceivedAt: l.ReceivedAt, Judged: judged})
	return TestLanded{Commands: ld.Commands, Outcome: ld.Result.outcome, Refusal: ld.Result.refusal,
		Position: ld.Result.position, OK: ld.Result.ok, PackBytes: ld.Result.packBytes, Adopted: ld.Adopted}, err
}

// SetProposalCheck replaces s's own check of a proposal before its bundle is
// stored (code.Proposal.ValidateUnstored) — how a case stands in the
// receiver's bug no push reaches. Set before s's first push.
func SetProposalCheck(s *Server, check func(code.Proposal) error) { s.checkProposal = check }
