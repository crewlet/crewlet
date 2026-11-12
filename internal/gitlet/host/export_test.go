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

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
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
// page's table prints it: a change named and its page addressed by
// placeholders (<repo>!<n>, on <api.external_url>), and a refusal by its code
// with its sentence left out, the sentences being the decide's and the
// receiver's to word. The bare forms are a host told no base, which no engine
// node serving a push is, and the page does not print them.
func ChangeAnswerLines() []string {
	const name, page = "<repo>!<n>", "<api.external_url>/dashboard#/gitlet/repos/<repo>/changes/<n>"
	lines := []string{fmt.Sprintf(changeOpenedLine, name, page), fmt.Sprintf(changeUpdatedLine, name, page),
		changeUnlanded, changePending, changeUnknown}
	for _, r := range gitlet.PushChangeRefusals() {
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(changeRefusedLine, r, "")))
	}
	return lines
}

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

// RecordSpoolWaits has s's spool allowance note the bound of every wait a
// request begins from now on, and answers a reader of the bounds noted, in
// order. Set after SetSpools, which replaces the allowance, and before the
// requests a case sends: the allowance reads it on every request's goroutine.
func RecordSpoolWaits(s *Server) func() []time.Duration {
	var (
		mu    sync.Mutex
		waits []time.Duration
	)
	s.spools.armed = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
	}
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(waits)
	}
}

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
func ProtectAfterForce(cmds []gitlet.Command, rules []gitlet.PushRule, by gitlet.PrincipalKind,
	judged []gitlet.Command) (int, error) {
	parsed, err := gitlet.ParseProtections(rules)
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
	Commands   []gitlet.Command
	Atomic     bool
	Principal  gitlet.Principal
	Via        gitlet.PushVia
	PushID     string
	ReceivedAt time.Time
	// Judged are the commands already asked of Synced.Repo's protections,
	// as they stand; nil is none.
	Judged []gitlet.Command
}

// TestLanded is what a landing came to: its commands, the record's outcome
// as the push's audit records it, and whether its pack was adopted.
type TestLanded struct {
	Commands  []gitlet.Command
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
// stored (gitlet.Proposal.ValidateUnstored) — how a case stands in the
// receiver's bug no push reaches. Set before s's first push.
func SetProposalCheck(s *Server, check func(gitlet.Proposal) error) { s.checkProposal = check }

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

// BuildBudget is the git run time s gives the build of an engine write of kind
// (its gitlet.EngineWriteKind.BuildDeadline in production).
func BuildBudget(s *Server, kind gitlet.EngineWriteKind) time.Duration { return s.buildBudget(kind) }

// SetBuildBudget scales the git run time s gives every engine write's build,
// whatever its kind. Set before s's first write.
func SetBuildBudget(s *Server, d time.Duration) {
	s.buildBudget = func(gitlet.EngineWriteKind) time.Duration { return d }
}

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

// HoldGit holds every process s hands the pool from now on whose arguments
// start with args — a subcommand, and as many of its own arguments as tell
// one of its processes from another — until release: reached is closed when
// the first is held. Set before s's first request.
func HoldGit(s *Server, args ...string) (reached <-chan struct{}, release func()) {
	at, held := make(chan struct{}), make(chan struct{})
	var once, closing sync.Once
	run := s.runGit
	s.runGit = func(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
		if len(args) > 0 && len(c.Args) >= len(args) && slices.Equal(c.Args[:len(args)], args) {
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

// SlowGit has s report every process it hands the pool from now on whose
// arguments start with args — a subcommand, and as many of its own arguments
// as tell one of its processes from another — as having run took, whatever it
// really ran: what a write's build is charged for it against its budget of
// git. Set before s's first write.
func SlowGit(s *Server, took time.Duration, args ...string) {
	run := s.runGit
	s.runGit = func(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
		res, err := run(ctx, c)
		if len(args) > 0 && len(c.Args) >= len(args) && slices.Equal(c.Args[:len(args)], args) {
			res.Took = took
		}
		return res, err
	}
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

// GitTook has s total the run time of every process it hands the pool from now
// on whose arguments start with args, and answers a reader of that total and
// of how many there were. Set before s's first request.
func GitTook(s *Server, args ...string) func() (time.Duration, int) {
	var (
		mu    sync.Mutex
		total time.Duration
		runs  int
	)
	run := s.runGit
	s.runGit = func(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
		res, err := run(ctx, c)
		if len(c.Args) >= len(args) && slices.Equal(c.Args[:len(args)], args) {
			mu.Lock()
			total, runs = total+res.Took, runs+1
			mu.Unlock()
		}
		return res, err
	}
	return func() (time.Duration, int) {
		mu.Lock()
		defer mu.Unlock()
		return total, runs
	}
}

// SetClock has s read its clock from now in place of its Options.Now: what a
// verification's start, end and deadline are stamped by, among the rest. Set
// before s's first request: the server reads it on every request's goroutine.
func SetClock(s *Server, now func() time.Time) { s.now = now }

// SetKeepaliveTicks has every push session of s take its keepalive's ticks
// from ticks, in place of a ticker of its Timing.Keepalive: one line on the
// push's stderr a tick, each written before the session takes the next. Set
// before s's first push: the server reads the field on every session's
// goroutine.
func SetKeepaliveTicks(s *Server, ticks <-chan time.Time) {
	s.keepaliveTicks = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
}

// SetChainMerges scales what a chain's merges are given together after its
// first entry, which is always given a merge's whole budget (chain.go).
func SetChainMerges(s *Server, d time.Duration) { s.chainMerges = d }

// SetChainObjects scales the uncompressed bytes a chain's entries may write
// together, and each alone (gitlet.MaxChainObjectBytes; chain.go).
func SetChainObjects(s *Server, n int64) { s.chainObjects = n }

// SetChainPack scales what a chain's pack is given (gitlet.ChainPackDeadline;
// chain.go): BenchmarkBuildChain's past-bound arm lifts it to measure a pack
// no chain may make.
func SetChainPack(s *Server, d time.Duration) { s.chainPack = d }

// RefuseObjectOnce has s answer its next `index-pack --strict` as the relay's
// check refusing object id — the trace and the words a real refusal carries —
// without running it, and run every later one. Set before s's first request.
func RefuseObjectOnce(s *Server, id string) {
	var once sync.Once
	run := s.runGit
	s.runGit = func(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
		refuse := false
		if len(c.Args) >= 2 && c.Args[0] == "index-pack" && c.Args[1] == "--strict" {
			once.Do(func() { refuse = true })
		}
		if !refuse {
			return run(ctx, c)
		}
		return gitproc.Result{Exit: 128, Stderr: "error: object " + id + ": badDate: invalid author/committer " +
				"line - bad date\nfatal: fsck error in packed object\n",
				Trace: gitproc.Trace{Formats: []string{"fsck error in packed object"}}},
			fmt.Errorf("index-pack exited 128")
	}
}
