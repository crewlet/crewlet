package host

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
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
	ReportStatus, ReportStatusV2, SideBand64k, Atomic, Quiet, PushOptions bool
}

// ReadCommands is readCommands over r.
func ReadCommands(r io.Reader, maxBytes, maxCommands int) (Section, error) {
	s, err := readCommands(bufio.NewReader(r), maxBytes, maxCommands)
	out := Section{Raw: s.Raw, Empty: s.Empty(), Caps: Capabilities{ReportStatus: s.Caps.reportStatus,
		ReportStatusV2: s.Caps.reportStatusV2, SideBand64k: s.Caps.sideBand64k, Atomic: s.Caps.atomic,
		Quiet: s.Caps.quiet, PushOptions: s.Caps.pushOptions}}
	for _, c := range s.Commands {
		out.Commands = append(out.Commands, [3]string{c.Old, c.New, c.Ref})
	}
	return out, err
}

// The command section's refusals.
var (
	ErrSectionTooLarge = errSectionTooLarge
	ErrOptionsTooLarge = errOptionsTooLarge
	ErrTooManyCommands = errTooManyCommands
	ErrPushCertificate = errPushCertificate
)

// ChangeAnswerLines is every line a push's change is told in, as the concept
// page's table prints it: a change named by placeholders (<repo>!<n>), and a
// refusal by its code with its sentence left out, the sentences being the
// decide's and the receiver's to word.
func ChangeAnswerLines() []string {
	lines := []string{fmt.Sprintf(changeOpenedLine, "<repo>!<n>"), fmt.Sprintf(changeUpdatedLine, "<repo>!<n>"),
		changeUnlanded, changePending, changeUnknown}
	for _, r := range code.PushChangeRefusals() {
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(changeRefusedLine, r, "")))
	}
	return lines
}

// SetBoxResyncClock gives s's boxes' re-sync buckets a clock of the case's,
// so it can pass a refill without waiting it out: new buckets, every one full.
func SetBoxResyncClock(s *Server, now func() time.Time) { s.boxResyncs = code.NewResyncs(now) }

// RunRequests is how many of run's requests are in flight on s.
func RunRequests(s *Server, run string) int {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	return s.runRequests[run]
}

// IdxOIDs is idxOIDs.
func IdxOIDs(path string) (int64, io.ReadCloser, error) { return idxOIDs(path) }

// SetRequestMemory sets how much of a fetch request s holds in memory before
// it spools the rest (RequestMemoryBytes in production).
func SetRequestMemory(s *Server, n int) { s.requestMemory = n }

// SpoolSizes are a spool allowance's bounds, as SetSpools takes them.
type SpoolSizes struct {
	// Limit and Share are the bytes every spool, and each credential's, may
	// hold at once; Request is the most one request spools, and what a body
	// whose client declared no length claims.
	Limit, Share, Request int64
	// Grain is the most room a body asks for at a time; zero is the
	// production's mebibyte.
	Grain int64
	// Places and PlacesPerCredential are how many requests may wait to start.
	Places, PlacesPerCredential int
}

// SpoolSizesFor is the production allowance for a transfer lane of transfers
// slots.
func SpoolSizesFor(transfers int) SpoolSizes {
	z := spoolSizesFor(transfers)
	return SpoolSizes{Limit: z.limit, Share: z.share, Request: z.request, Grain: z.grain, Places: z.places,
		PlacesPerCredential: z.placesPerCredential}
}

// SpoolSizesCheck is why z cannot be an allowance, nil when it can.
func SpoolSizesCheck(z SpoolSizes) error { return z.sizes().check() }

func (z SpoolSizes) sizes() spoolSizes {
	grain := z.Grain
	if grain == 0 {
		grain = spoolGrain
	}
	return spoolSizes{limit: z.Limit, share: z.Share, request: z.Request, grain: grain, places: z.Places,
		placesPerCredential: z.PlacesPerCredential}
}

// SetSpools replaces s's spool allowance with one of sizes z. Set before the
// requests a case sends: the server reads the field on every request's
// goroutine.
func SetSpools(s *Server, z SpoolSizes) { s.spools = newSpools(z.sizes()) }

// SpoolsInUse is the bytes s's spool allowance holds right now, and the spools
// open — bodies arriving, and bodies that ended and are not yet given back.
func SpoolsInUse(s *Server) (int64, int) { return s.spools.inUse() }

// SpoolsArriving is how many of s's spools have their bodies arriving now.
func SpoolsArriving(s *Server) int { return s.spools.arrivingNow() }

// SpoolWaiting is how many requests wait for room in s's spool allowance
// right now.
func SpoolWaiting(s *Server) int { return s.spools.waiters() }

// SpoolPlaces is how many places s's spool waiting room has given out now.
func SpoolPlaces(s *Server) int { return s.spools.placesHeld() }

// SpoolOutcomes is every outcome a request's turn at the spool allowance ends
// in, as crewlet.code.host.spool.waits counts it.
func SpoolOutcomes() []string { return slices.Clone(spoolOutcomes) }

// SetRequestMemoryAllowance replaces s's request memory allowance.
func SetRequestMemoryAllowance(s *Server, limit int64) { s.heads = newAllowance(limit) }

// RequestMemoryInUse is the bytes s's request memory allowance holds now.
func RequestMemoryInUse(s *Server) int64 { return s.heads.inUse() }

// ProtectAfterForce is protectAfterForce over rules for a push by a principal
// of kind by, every command in judged as the receive handler judged it: how
// many commands it asked.
func ProtectAfterForce(cmds []code.Command, rules []code.PushRule, by code.PrincipalKind,
	judged []code.Command) (int, error) {
	parsed, err := code.ParseProtections(rules)
	if err != nil {
		return 0, err
	}
	seen := map[judgedKey]bool{}
	for _, c := range judged {
		seen[judgedKey{c.Ref, c.Old, c.New}] = true
	}
	return protectAfterForce(cmds, parsed, by, seen), nil
}

// TestLanding is a landing (land.go) as a case forms one — a push the relay
// never saw, its pack written into its scratch by the case.
type TestLanding struct {
	Synced     cache.Synced
	Scratch    *cache.Scratch
	Commands   []code.Command
	Atomic     bool
	Principal  code.Principal
	Via        code.PushVia
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
		Principal: l.Principal, Via: l.Via, PushID: l.PushID, ReceivedAt: l.ReceivedAt, Judged: judged})
	return TestLanded{Commands: ld.Commands, Outcome: ld.Result.outcome, Refusal: ld.Result.refusal,
		Position: ld.Result.position, OK: ld.Result.ok, PackBytes: ld.Result.packBytes, Adopted: ld.Adopted}, err
}

// IsForced is s's force check of old against new in sc, over a repository of
// objects live objects.
func IsForced(ctx context.Context, s *Server, sc *cache.Scratch, objects int64, old, new string) (bool, bool, error) {
	return s.isForced(ctx, sc, objects, old, new)
}

// Introduced is s's listing of the commits a push of news introduces in sc,
// over a repository of objects live objects.
func Introduced(ctx context.Context, s *Server, sc *cache.Scratch, objects int64, news []string,
	packBytes int64) ([]string, int, error) {

	return s.introduced(ctx, sc, objects, news, packBytes)
}

// PathOf is s's search for the path a push of news reaches oid by in sc.
func PathOf(ctx context.Context, s *Server, sc *cache.Scratch, objects int64, news []string, oid string,
	packBytes int64) string {

	return s.pathOf(ctx, sc, objects, news, oid, packBytes)
}

// The bounds of a push's walks: one force check's, and one listing's.
var (
	ForceDeadline   = forceDeadline
	ListingDeadline = listingDeadline
)

// MaxEngineCommits is the most commits committed as the engine one coding
// box's pack may hold for the repository to be asked about.
const MaxEngineCommits = maxEngineCommits

// HookWalks is what a push of cmds — each {old, new, ref} — may spend walking
// a repository of objects live objects beside its hook budget.
func HookWalks(cmds [][3]string, objects int64) time.Duration {
	parsed := make([]parsedCommand, len(cmds))
	for i, c := range cmds {
		parsed[i] = parsedCommand{Old: c[0], New: c[1], Ref: c[2]}
	}
	return hookWalks(parsed, objects)
}

// SetProposalCheck replaces s's own check of a proposal before its bundle is
// stored (code.Proposal.ValidateUnstored) — how a case stands in the
// receiver's bug no push reaches. Set before s's first push.
func SetProposalCheck(s *Server, check func(code.Proposal) error) { s.checkProposal = check }

// WriteJoiners is how many writes have joined the write of pushID running on
// s now (the in-flight join), 0 where none runs.
func WriteJoiners(s *Server, pushID string) int {
	s.writesMu.Lock()
	defer s.writesMu.Unlock()
	if f, ok := s.writes[pushID]; ok {
		return f.joined
	}
	return 0
}

// WritesInFlight is how many writes s is building or landing now: each push
// id once, however many asks joined it — what a benchmark reads the write
// lane's slots held by.
func WritesInFlight(s *Server) int {
	s.writesMu.Lock()
	defer s.writesMu.Unlock()
	return len(s.writes)
}

// BuildBudget is the git run time s gives an engine write's build
// (code.WriteBuildDeadline in production).
func BuildBudget(s *Server) time.Duration { return s.buildBudget }

// SetBuildBudget scales the git run time s gives an engine write's build.
// Set before s's first write.
func SetBuildBudget(s *Server, d time.Duration) { s.buildBudget = d }

// CommitDates is commitDates: an engine-built commit's author and committer
// dates for an operation minted at instant over a parent committed at parent.
func CommitDates(instant, parent time.Time) (author, committer time.Time) {
	return commitDates(instant, parent)
}

// GitInput is one process s handed the pool: its directory, its alternate,
// its arguments and all its standard input held.
type GitInput struct {
	Dir, Alternate string
	Args           []string
	Stdin          []byte
}

// RecordGitInputs has s record every process it hands the pool from now on —
// its standard input read whole and handed on — and answers a reader of what
// it has recorded, in order. Set before s's first request.
func RecordGitInputs(s *Server) func() []GitInput {
	var (
		mu  sync.Mutex
		ins []GitInput
	)
	run := s.runGit
	s.runGit = func(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
		in := GitInput{Dir: c.Dir, Alternate: c.Alternate, Args: slices.Clone(c.Args)}
		if c.Stdin != nil {
			body, err := io.ReadAll(c.Stdin)
			if err != nil {
				return gitproc.Result{}, err
			}
			in.Stdin = body
			c.Stdin = bytes.NewReader(body)
		}
		mu.Lock()
		ins = append(ins, in)
		mu.Unlock()
		return run(ctx, c)
	}
	return func() []GitInput {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(ins)
	}
}

// HoldGit holds every process of subcommand sub s hands the pool from now on
// until release: reached is closed when the first is held. Set before s's
// first request.
func HoldGit(s *Server, sub string) (reached <-chan struct{}, release func()) {
	at, held := make(chan struct{}), make(chan struct{})
	var once, closing sync.Once
	run := s.runGit
	s.runGit = func(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
		if len(c.Args) > 0 && c.Args[0] == sub {
			once.Do(func() { close(at) })
			select {
			case <-held:
			case <-ctx.Done():
				return gitproc.Result{}, ctx.Err()
			}
		}
		return run(ctx, c)
	}
	return at, func() { closing.Do(func() { close(held) }) }
}

// RecordGit has s record every gitproc.Cmd it hands the pool from now on, and
// answers a reader of what it has recorded, in order. Set before s's first
// request: the server reads the field on every request's goroutine.
func RecordGit(s *Server) func() []gitproc.Cmd {
	var (
		mu   sync.Mutex
		cmds []gitproc.Cmd
	)
	run := s.runGit
	s.runGit = func(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
		mu.Lock()
		cmds = append(cmds, c)
		mu.Unlock()
		return run(ctx, c)
	}
	return func() []gitproc.Cmd {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(cmds)
	}
}
