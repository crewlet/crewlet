package engine

import (
	"context"
	"crypto/subtle"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A seat's credential issued at use, by whichever integration issues it.
//
// Some integrations never seal an agent's credential: the engine issues it
// when one of the seat's values names it, from what the integration does hold,
// and it lives about an hour. GitHub's installation token, minted from the
// agent's own app key, is one (githubtoken.go); Google Cloud's access token,
// minted for the agent's own service account, is another (googlecloudtoken.go).
// Configuration names such a credential the way it names every other, as a
// `${VAR}`, in the three places a seat's credentials are read:
//
//   - its tool servers' `role.mcp_env` block, read by [seatSpecs] for the
//     servers this node runs for the seat;
//   - its coding box's environment and setup files, `role.sandbox.env` and
//     the setup steps, read by [Engine.runEnvironment] for every executor
//     launch;
//   - the tool servers that go INTO that box, `role.sandbox.mcp`, read by
//     [sandboxMCP].
//
// ONE CONTRACT, [seatIssuer], which every such integration implements, and the
// paths every seat shares ask the registry ([Engine.seatIssuers]), never a
// kind: the seat's resolution, the renewal of its tool servers, a running
// box's token files, and the waiting and refused stories a value naming one is
// told by. A second integration is a second implementation, not a second copy
// of those paths.
//
// # Answered by the seat's own resolution, and nothing else's
//
// Each of those reads goes through [Engine.seatResolver]: the company's chain
// with one [seatToken] per issued variable laid in front of it, each answering
// this seat's variable and no other. Another seat naming it resolves nothing,
// and is told the variable is not set; the company's shared servers, which act
// as no seat, never see it at all. That keeps an agent's identity with the
// agent it was made for, by construction rather than by a check somebody has
// to remember.
//
// # Issued only when read
//
// A source issues at its first lookup, so a seat whose configuration never
// names a credential costs its vendor no request, and a pass over the seat's
// values that names it twice issues once. What was issued is held by the
// issuer's process-wide cache, never by the source past the read that built
// it.
//
// # When it cannot be issued
//
// Three stories, told apart as the rest of the credential rule tells its own
// (toolcredentials.go):
//
//   - THE INTEGRATION CANNOT ISSUE IT YET: a person's step its card already
//     asks for (GitHub's app not created or not installed), or the
//     integration's own wait, which clears with nobody acting (a grant its
//     vendor is still applying). The variable is declared as the
//     integration's to issue ([Engine.mintedFor]), so the value it is read
//     into is held back as WAITING, exactly as a provisioning pass's seal is.
//     A person's step releases it when the reconcile records the step, which
//     applies a revision and runs every read again; the integration's own
//     wait is asked again on the restart schedule ([seatToken.settling]),
//     since no revision or seal is coming to run the reads.
//   - THE VENDOR REFUSED THE ISSUE, or could not be reached: a failure with a
//     reason ([seatToken.unissued]), retried on the restart schedule, which
//     each issuer's cache holds the refusal for, so a refusal that lasts
//     costs one request a wait rather than one per poll, nudge, launch and
//     delivery. Whether a wait clears it decides what the failure says
//     ([seatIssuer.refusal]): a vendor not answering is a wait, a vendor
//     refusing is a person's step, refused again at every ask until taken.
//   - THE INTEGRATION IS NOT ENABLED: nothing issues anything, since
//     switching the block off is the gesture an operator makes after a leak,
//     and the variable is one nothing sets.
//
// In none of the three is an executor run refused: it starts without the
// value, said as its launch's `sandbox_env_unresolved`, because a seat's every
// turn is one and a seat refused its turn for a token would do no work at all.
// A token FILE it was launched without is the poll's to write once the token
// can be issued ([Engine.refreshTokenFiles]).

// seatIssuer is an integration that issues each of its agents a credential at
// the moment one of the seat's values names it, rather than sealing one. The
// engine's wiring for the integration implements it (githubtoken.go), and
// [Engine.seatIssuers] lists every implementation.
type seatIssuer interface {
	// kind is the integration that issues: what a value waiting on the
	// credential is told it waits for.
	kind() integration.Kind

	// variable is what it issues the seat with handle under c, or "" where
	// it issues none: no company, the integration off, or handle no agent's.
	// Declared for every agent while the integration runs, issuable now or
	// not, since a seat naming it early waits on the integration's step.
	variable(c *Company, handle string) string

	// issuing is how seat's credential is issued for one resolution, its
	// settings read through env, or nil where the seat is issued nothing
	// now (it is off at the integration). Called once per resolution; the
	// issue itself runs at the source's first lookup. A seat that is on and
	// cannot be issued yet is given one that fails with the integration's
	// waiting story, so the issue is attempted, told, and retried where it
	// settles ([errIssueSettling]).
	issuing(c *Company, seat *org.Role, env *config.Resolver) issueFunc

	// refusal is why an issue that failed with err failed, and whether a
	// wait clears it. "" is a waiting story: a person's step the
	// integration's card already asks for where passes is false, and the
	// integration still settling the credential where it is true, which a
	// wait clears with nothing sealed or applied ([seatToken.settling]).
	// Never the credential.
	refusal(err error) (reason string, passes bool)

	// words are its words in the stories every issued credential is told by.
	words() issuedWords
}

// issuedLives is how long an issuer's credential lasts and how long before its
// end the holder asks again. An issuer declares its own ([lifeDeclarer]); the
// rest take [defaultLives], the hour GitHub's and Google's tokens last.
type issuedLives struct {
	lifetime, renewBefore time.Duration
}

// defaultLives is an hour-long credential renewed with ten minutes left: the
// lives the shared paths have always asked ([toolTokenLife], [runTokenLife]).
var defaultLives = issuedLives{lifetime: time.Hour, renewBefore: 10 * time.Minute}

// lifeDeclarer is an issuer whose credential lasts other than an hour.
type lifeDeclarer interface {
	lives() issuedLives
}

// livesOf is issuer's lives: its own where it declares them, else the default.
func livesOf(issuer seatIssuer) issuedLives {
	if d, ok := issuer.(lifeDeclarer); ok {
		return d.lives()
	}
	return defaultLives
}

// run is what a launch is promised: all but the renewal margin.
func (l issuedLives) run() time.Duration { return l.lifetime - l.renewBefore }

// ask is the life to issue for, where a shared path asked for life in the
// default lives' terms: a launch's ask ([runTokenLife]) is the issuer's run,
// and a tool's or a refresh's ([toolTokenLife]) at most its renewal margin.
// The default lives answer every ask unchanged.
func (l issuedLives) ask(life time.Duration) time.Duration {
	if life > defaultLives.renewBefore {
		return min(life, l.run())
	}
	return min(life, l.renewBefore)
}

// issueFunc issues one seat's credential good for at least life after the
// call, from the issuer's own process-wide cache while what it holds is.
type issueFunc func(ctx context.Context, life time.Duration) (issuedToken, error)

// errIssueSettling is an issue the integration cannot answer yet and will with
// nobody acting, such as a grant its vendor is still applying. An issuer's
// issue fails with it, wrapped in the issuer's own words, and its refusal
// answers it as settling: no reason, and a wait clears it ([seatToken.settling]).
var errIssueSettling = errors.New("engine: the integration is still settling the seat's credential")

// issuedToken is one credential an issuer handed out: its value, when it
// expires, and when a holder asks again, which is before it expires.
type issuedToken struct {
	Token     string
	ExpiresAt time.Time
	RenewAt   time.Time
}

// issuedWords complete the sentences the engine writes of an issuer's
// credential. Each is a fragment, never a whole sentence, and names no value.
type issuedWords struct {
	// source names the credential in a shadow warning, which a stale value
	// held under the same name in the secret store or the environment earns.
	source string
	// verb is what the integration does to the variable: "waiting for the
	// <kind> integration to <verb> ${VAR}".
	verb string
	// arrival ends "it starts once ...": what releases a value waiting on it.
	arrival string
	// refused ends a refusal no wait clears: who fixes what.
	refused string
	// retried ends a refusal a wait clears: that it is asked for again.
	retried string
}

// The lives the shared paths ask an issued credential for: how long it has to
// stay good after the read. Every issuer issues for about an hour and renews at
// [toolTokenLife] left, which these are shares of; the conformance suite holds
// each issuer to them.
const (
	// toolTokenLife is what a tool server on this node is started with. The
	// server is restarted with a fresh credential at the one it holds' renew
	// time ([Engine.armRenewal]), so it never carries one past its expiry.
	toolTokenLife = 10 * time.Minute

	// runTokenLife is what an executor's environment is given at launch,
	// fixed when its box starts: fifty minutes, the most an hour-long
	// credential can promise while one issue still serves the launches of
	// the next ten. A longer run reads it through a file the poll keeps.
	runTokenLife = 50 * time.Minute

	// refreshTokenLife is what a running box's token file is kept at: the
	// poll runs every fifteen seconds, so a ten-minute margin is forty polls
	// wide, and a file is rewritten within seconds of its token renewing.
	refreshTokenLife = toolTokenLife
)

// seatIssuers are the integrations this build issues seat credentials at use
// through, in the order a seat's resolution chains them; [Engine.issuers]
// replaces them in a test.
func (e *Engine) seatIssuers() []seatIssuer {
	if e.issuers != nil {
		return e.issuers
	}
	return []seatIssuer{
		githubSeatIssuer{issuer: &e.githubIssuer},
		awsSeatIssuer{issuer: &e.awsIssuer},
		googleCloudSeatIssuer{engine: e, tokens: &e.googleCloudIssuer},
	}
}

// seatToken answers one variable an integration issues one seat, issued at
// its first lookup.
//
// A [config.Source] for ONE RESOLUTION: built for a pass over one seat's
// values, consulted while those values are read, and dropped with the pass.
// It holds the pass's context for that reason alone: [config.Source] has no
// context of its own to thread, and an issue begun by a pass must end with it.
//
// A RESOLUTION'S TOKENS ARE CHAINED by next, one per variable the seat is
// issued, in the registry's order. Each is laid in front of the company's
// chain on its own, so a shadow warning names the issuer that hid the value,
// and every question about the whole resolution walks the chain from the
// first, which is what [Engine.seatResolver] hands back.
type seatToken struct {
	ctx    context.Context
	issuer seatIssuer
	// issue is nil where the seat is issued nothing now: the variable is
	// still the integration's, and nothing answers it.
	issue issueFunc
	name  string
	life  time.Duration
	next  *seatToken

	once  sync.Once
	token issuedToken
	err   error
}

// Lookup implements [config.Source]: this seat's credential for its own
// variable, and nothing for any other name.
func (s *seatToken) Lookup(name string) (string, bool) {
	if s == nil || name != s.name || s.issue == nil {
		return "", false
	}
	s.once.Do(func() {
		s.token, s.err = s.issue(s.ctx, livesOf(s.issuer).ask(s.life))
	})
	if s.err != nil {
		return "", false
	}
	return s.token.Token, true
}

// SourceName is how a shadow warning names this source: a stale value the
// secret store or the environment holds under the seat's variable is hidden by
// the credential issued here, and it is that value the operator removes
// ([config.Resolver]).
func (s *seatToken) SourceName() string { return s.issuer.words().source }

// issued is the credential this source handed out, and whether it handed one
// out at all: a read that never named the variable issues nothing.
func (s *seatToken) issued() (issuedToken, bool) {
	if s == nil {
		return issuedToken{}, false
	}
	// Read after the pass's reads, on the goroutine that ran them, so the
	// once has either run or will never run for this resolution.
	if s.err != nil || s.token.Token == "" {
		return issuedToken{}, false
	}
	return s.token, true
}

// link is the chain's source for name, nil where the resolution issues none.
func (s *seatToken) link(name string) *seatToken {
	for t := s; t != nil; t = t.next {
		if t.name == name {
			return t
		}
	}
	return nil
}

// failure is why the issue the resolution attempted for name failed and
// whether a wait clears it, or the zero [issueFailure] where it did not try,
// issued, or met the integration's waiting story.
func (s *seatToken) failure(name string) issueFailure {
	t := s.link(name)
	if t == nil || t.err == nil {
		return issueFailure{}
	}
	said, passes := t.issuer.refusal(t.err)
	if said == "" {
		return issueFailure{}
	}
	return issueFailure{said: said, passes: passes, words: t.issuer.words()}
}

// unissued is why name could not be issued by an issue that was attempted, or
// "": the waiting story is the integration's own record to tell.
//
// NEVER THE TOKEN, and never the vendor's response body: the reason is the
// issuer's, which carries neither.
func (s *seatToken) unissued(name string) string { return s.failure(name).said }

// refused reports whether an issue the resolution attempted for any of its
// variables failed with a reason, which the seat's children retry.
func (s *seatToken) refused() bool {
	for t := s; t != nil; t = t.next {
		if t.unissued(t.name) != "" {
			return true
		}
	}
	return false
}

// settling reports whether an issue the resolution attempted for any of its
// variables met the integration still settling it: a waiting story a wait
// clears ([seatIssuer.refusal] answering no reason, and passes). Told as
// waiting, and retried by the seat's children, since nothing will be sealed
// or applied to run the read again.
func (s *seatToken) settling() bool {
	for t := s; t != nil; t = t.next {
		if t.err == nil {
			continue
		}
		if said, passes := t.issuer.refusal(t.err); said == "" && passes {
			return true
		}
	}
	return false
}

// renewAt is the EARLIEST moment a holder of anything the resolution issued
// asks again, and false where it issued nothing: a child carrying two issued
// credentials is restarted before the first of them runs short.
func (s *seatToken) renewAt() (time.Time, bool) {
	var at time.Time
	found := false
	for t := s; t != nil; t = t.next {
		if issued, ok := t.issued(); ok && (!found || issued.RenewAt.Before(at)) {
			at, found = issued.RenewAt, true
		}
	}
	return at, found
}

// seatResolver is how one seat's values are read: the company's chain, with
// each variable an integration issues the seat laid in front of it
// ([seatToken]). life is how long anything it issues must stay good after the
// read ([toolTokenLife], [runTokenLife], [refreshTokenLife]).
//
// The first of the resolution's tokens comes back too, nil where nothing
// issues the seat anything, so the caller can say why a value naming one went
// without ([seatMint]) and when what it was handed needs renewing.
func (e *Engine) seatResolver(
	ctx context.Context, c *Company, seat *org.Role, life time.Duration,
) (*config.Resolver, *seatToken) {
	base := e.resolver()
	if c == nil || seat == nil || !seat.IsAgent() {
		return base, nil
	}
	issuers := e.seatIssuers()
	resolver, first := base, (*seatToken)(nil)
	// BACKWARDS, so the chain runs in the registry's order from its first.
	for i := len(issuers) - 1; i >= 0; i-- {
		issuer := issuers[i]
		name := issuer.variable(c, seat.Handle())
		if name == "" {
			continue
		}
		first = &seatToken{
			ctx: ctx, issuer: issuer, issue: issuer.issuing(c, seat, base),
			name: name, life: life, next: first,
		}
		resolver = resolver.Ahead(first)
	}
	return resolver, first
}

// refreshRunTokens keeps a running box's token files current: the completion
// poll's [sandbox.RefreshFunc], and the launch's for a job that continues in
// its turn's kept box.
//
// # A run outlives the token it was launched with
//
// An executor's environment is fixed when its box starts, and an issued
// credential in that environment is good for [runTokenLife]. A run is never
// stopped on a clock, so a long one would push with a token its vendor
// refuses, halfway through its work. What can change under a running process
// is a FILE: a setup step that writes the seat's issued variable to one (a
// file whose whole body is `${VAR}`, provisioned at launch like any other) is
// rewritten here, on the poll, with the token the issuer holds once it renews
// it, and a credential helper reading that file at the moment of use always
// reads a token that is good. The documented git-auth recipe reads GitHub's
// that way (docs/concepts/code-sandbox.md). The rewrite REPLACES the file
// whole ([sandbox.Sandbox.WriteFile]): written in place, it was empty and then
// half written for as long as the write took, and a push whose helper read it
// then authenticated with nothing.
//
// ON WHICHEVER WAITER POLLS THE BOX, and so on both of a node's
// ([Engine.sandboxWaiterOptions]): a `direct` or `container` box is polled by
// its launching node's own waiter and by no other, and an `e2b` box by the
// fleet's, on the node holding its duty (ADR-0065).
//
// AND BEFORE A JOB STARTS IN A KEPT BOX, on the node launching it
// ([sandbox.CoordinatorOptions.Refresh]). A box is paused while its turn's
// reviewer reads the job's work, or while the job waits on a person's answer,
// for up to [sandbox.DefaultPauseTTL], and no poll reaches a paused box; the
// job that continues in it runs no setup, so its file holds what the last poll
// wrote, which can have expired. A job carried on after an answer acts on it
// at once, before its own first poll could write the file again.
//
// ASKED OF THE BOX, NOT REMEMBERED. The file is read back and rewritten only
// where it differs, so the fleet's duty can move between nodes, and the
// engine can restart, with nothing to carry over, and a quiet poll costs one
// read. A file the run was launched WITHOUT (its token could not be issued
// then, so the launch wrote none, [Engine.runEnvironment]) reads back as
// nothing, which differs from any token, so the first poll after it can be
// issued writes it. The composition of steps is the launch's own
// ([boxSetup]), read off the company current now: a file a revision has since
// removed is no longer kept, which is what the operator who removed it asked
// for.
//
// # Recorded before it is written
//
// A renewed token is one the launch never saw, so no capture of the run's
// session would be scrubbed of it, and a transcript prints it: a helper's
// trace, a `cat` of the file, an AWS session's token bare, which no shape the
// redaction knows names. So each token is handed to record — the job's sealed
// set beside its row on the poll ([sandbox.Coordinator.RecordHanded]), the
// set the job will be sealed with at a launch — with the expiry its issuer
// gave it, BEFORE the first of its files is written, and a token record
// refuses is not written on this pass: the store did not answer, and the next
// poll, fifteen seconds later, asks again, well inside the five minutes the
// shortest renewal (an AWS session's) leaves; or the job is no longer running,
// and nothing more is written for it. So the job's set holds a token before
// its box does, and every capture of the job's session taken once the job has
// ended is scrubbed of it (internal/sandbox's handed.go).
//
// NEVER LOGGED, never returned: the lines here name the seat, the path and
// when the token expires.
func (e *Engine) refreshRunTokens(
	ctx context.Context, manager *sandbox.Manager, run sandbox.PendingRun, box sandbox.Sandbox,
	record sandbox.RecordHanded,
) {
	e.refreshTokenFiles(ctx, manager.DefaultSetup(), run, box, record)
}

// refreshTokenFiles is [Engine.refreshRunTokens] over the provider-wide setup
// steps defaults, which the poll reads off the manager that reached the box.
//
// ONE SET OF FILES PER ISSUED VARIABLE, each refreshed from its own source: a
// variable that cannot be issued now leaves its files as they are while the
// others' are written, and the job's story tells every variable that waits.
func (e *Engine) refreshTokenFiles(
	ctx context.Context, defaults []sandbox.SetupStep, run sandbox.PendingRun, box sandbox.Sandbox,
	record sandbox.RecordHanded,
) {
	c := e.Company()
	if c == nil || c.Org == nil {
		return
	}
	seat := c.Org.AgentSeatByHandle(run.AgentHandle)
	if seat == nil {
		return
	}
	files := e.issuedFiles(c, seat, boxSetup(defaults, seat.Sandbox))
	if len(files) == 0 {
		return
	}
	_, tokens := e.seatResolver(ctx, c, seat, refreshTokenLife)
	job := run.TurnID + "/" + run.LaunchID
	var (
		story                 tokenWait
		waiting, waitingFiles []string
		refusals              []string
	)
	for t := tokens; t != nil; t = t.next {
		paths := files[t.name]
		if len(paths) == 0 {
			continue
		}
		value, ok := t.Lookup(t.name)
		if !ok {
			waiting = append(waiting, t.name)
			waitingFiles = append(waitingFiles, paths...)
			story.held = story.held || tokenFilesHold(ctx, box, paths)
			if reason := t.unissued(t.name); reason != "" {
				refusals = append(refusals, reason)
			}
			continue
		}
		e.writeTokenFiles(ctx, box, seat.Handle(), run, job, t, value, paths, record)
	}
	if len(waiting) == 0 {
		e.tokenWaits.settle(job)
		return
	}
	story.refused = strings.Join(refusals, "; ")
	attrs := []any{"seat", seat.Handle(), "turn_id", run.TurnID, "variables", waiting, "files", waitingFiles}
	e.tokenWaits.tell(ctx, job, story, e.tokenWaitForget(), attrs)
}

// issuedFiles are the files a box keeps each variable an integration issues
// seat in, by variable: those among steps' files whose whole body names it,
// and the integrations' own files the poll rewrites whose reference is it
// ([seataccess.BoxFile.Rewritten]). Empty where the seat names none, which
// costs the poll no resolution.
func (e *Engine) issuedFiles(c *Company, seat *org.Role, steps []sandbox.SetupStep) map[string][]string {
	supplied := suppliedFiles(steps, e.boxDefaults(c.Config, seat.Handle()))
	var out map[string][]string
	for _, issuer := range e.seatIssuers() {
		name := issuer.variable(c, seat.Handle())
		if name == "" {
			continue
		}
		paths := tokenFiles(steps, name)
		for _, d := range supplied {
			if ref, whole := envref.Whole(d.File.Ref); d.File.Rewritten && whole && ref == name {
				paths = append(paths, d.File.Path)
			}
		}
		if len(paths) > 0 {
			if out == nil {
				out = map[string][]string{}
			}
			slices.Sort(paths)
			out[name] = slices.Compact(paths)
		}
	}
	return out
}

// boxDefaults is what the registered vendors put in the box of the seat with
// handle ([seataccess.BoxDefaults]); [Engine.boxSupply] replaces it in a test.
func (e *Engine) boxDefaults(c *config.Company, handle string) []seataccess.BoxDefault {
	if e.boxSupply != nil {
		return e.boxSupply(c, handle)
	}
	return seataccess.BoxDefaults(c, handle)
}

// writeTokenFiles writes value, the token t issued, into each of paths that
// does not already hold it, telling a file no write reaches once — the token
// recorded first, through record, before the first of them is written, and
// none of them written where record refuses it (see [Engine.refreshRunTokens]).
func (e *Engine) writeTokenFiles(
	ctx context.Context, box sandbox.Sandbox, seat string, run sandbox.PendingRun, job string,
	t *seatToken, value string, paths []string, record sandbox.RecordHanded,
) {
	issued, _ := t.issued()
	recorded := false
	for _, path := range paths {
		file := tokenFileJob(job, path)
		held, err := box.ReadFile(ctx, path)
		if err == nil && subtle.ConstantTimeCompare(held, []byte(value)) == 1 {
			e.tokenWaits.settle(file)
			continue
		}
		if !recorded {
			if !e.recordToken(ctx, box, seat, run, job, t, value, issued.ExpiresAt, paths, record) {
				return
			}
			recorded = true
		}
		if err := box.WriteFile(ctx, path, []byte(value)); err != nil {
			// ONCE FOR THE FILE, like every other story here: a file the
			// job replaced with a link or a directory, a directory it made
			// read-only around the file, or a box out of disk, fails the
			// same way on every poll and every nudge for the life of the job.
			story := tokenWait{held: len(held) > 0, unwritable: err.Error()}
			e.tokenWaits.tell(ctx, file, story, e.tokenWaitForget(),
				[]any{"seat", seat, "turn_id", run.TurnID, "variable", t.name, "file", path})
			continue
		}
		e.tokenWaits.settle(file)
		log.InfoContext(ctx, "sandbox_token_refreshed", "seat", seat,
			"turn_id", run.TurnID, "variable", t.name, "file", path, "expires_at", issued.ExpiresAt)
	}
}

// recordToken records value, the token t issued, which lapses at expires,
// before it is written into any of paths, and reports whether it may be
// written. A record the store did not take is told once for the job's
// variable ([tokenRecordJob]) and the files keep what they hold until a later
// poll's record lands; a job the run's record no longer holds running
// ([sandbox.ErrJobNotRunning]) is told nothing, since the poll that reached it
// is a moment old and nothing more is written into its box.
func (e *Engine) recordToken(
	ctx context.Context, box sandbox.Sandbox, seat string, run sandbox.PendingRun, job string,
	t *seatToken, value string, expires time.Time, paths []string, record sandbox.RecordHanded,
) bool {
	key := tokenRecordJob(job, t.name)
	err := record(ctx, value, expires)
	switch {
	case err == nil:
		e.tokenWaits.settle(key)
		return true
	case errors.Is(err, sandbox.ErrJobNotRunning):
		e.tokenWaits.settle(key)
		return false
	}
	story := tokenWait{held: tokenFilesHold(ctx, box, paths), unrecorded: err.Error()}
	e.tokenWaits.tell(ctx, key, story, e.tokenWaitForget(),
		[]any{"seat", seat, "turn_id", run.TurnID, "variable", t.name, "files", paths})
	return false
}

// tokenFileJob is the key [tokenWaits] holds what was said of one of job's
// token files under: a file the poll cannot write is a story of its own,
// beside the job's story of a token that could not be issued, and each of a
// job's files is told apart.
func tokenFileJob(job, path string) string { return job + "\x00" + path }

// tokenRecordJob is the key [tokenWaits] holds what was said of a token of
// job's variable name the poll could not record, a story of its own beside
// the job's and its files': no file of the variable is written until one is.
// A different separator from [tokenFileJob]'s, so a variable is never read as
// a path.
func tokenRecordJob(job, name string) string { return job + "\x01" + name }

// tokenWait is the story a poll tells of a running job whose seat's issued
// tokens could not all be issued now, read off two facts: whether the job's
// files for them hold a token, and why the issues failed; or of one of its
// token files the poll could not write the issued token into; or of a token
// of one of its variables the poll could not record for the scrub of the
// run's sessions, and so wrote into none of the variable's files.
//
// FOUR STORIES OF A TOKEN NOT ISSUED, because each pair of answers is a
// different thing to say:
//
//   - nothing held, nothing refused: the integration cannot issue it yet, a
//     person's step its card asks for, and the job was launched before it.
//     Its files hold nothing, and the first poll after the step writes them.
//     The launch said the same wait as `sandbox_env_unresolved`, so this is
//     information, not a failure.
//   - a token held, nothing refused: the job was handed a token the
//     integration no longer renews, which its vendor refuses once it expires,
//     halfway through the work, and which nothing else on this node would say.
//   - a token held, a refusal: the vendor refused the issue, or could not be
//     reached. The job keeps the token it holds until that expires.
//   - nothing held, a refusal: the same refusal, met by a job that has nothing
//     to keep. Every agent turn is an executor run and a run starts without a
//     token its seat cannot be issued, so this is an ordinary path.
//
// AND ONE OF A FILE THE POLL CANNOT WRITE: the token is issued, and writing
// it into the file failed, so the job goes on with what the file holds (a
// token nothing renews any more, or nothing at all). The job replaced the file
// with a link or a directory (a local box replaces only a regular file, and
// leaves either as the job made it), made the directory around it read-only
// (a file is replaced by one written beside it, so a file made read-only
// itself is no obstacle), or the box ran out of disk; each fails the same way
// on every poll, so the failure is told once for the file, and again only
// when its error changes. A write that succeeds settles it.
//
// AND ONE OF A TOKEN THE POLL COULD NOT RECORD for the scrub of the run's
// sessions before writing it ([Engine.recordToken]): the store did not answer,
// so none of the variable's files is written on this poll, and they keep what
// they hold until a later poll's record lands, which settles it, as a record
// that finds the job no longer running does. Told once for the variable
// ([tokenRecordJob]), and again only when the store's error, or whether the
// files hold a token, changes.
//
// KIND-AGNOSTIC WORDS, with the variables beside them in the line: a job's
// story covers every variable it waits on, whichever integrations issue them.
// THE REFUSAL'S REASON IS PART OF THE STORY, so a job is told each reason its
// vendors give once, and told again only when an answer changes.
type tokenWait struct {
	// held is whether any of the job's files for the tokens not issued holds
	// a token: one its launch wrote or an earlier poll did. For a file the
	// poll cannot write, whether that file does.
	held bool
	// refused is why the issues that were attempted failed
	// ([seatToken.unissued]), or "" where the integrations cannot issue at
	// all yet.
	refused string
	// unwritable is why the issued token could not be written into the
	// file, or "" for a story of a token not issued. The box's error, which
	// names the file and never what was being written.
	unwritable string
	// unrecorded is why the issued token could not be recorded before it was
	// written, or "" for any other story: the sandbox's error, which never
	// names the token.
	unrecorded string
}

// tokenFilesHold reports whether any of a job's token files holds anything: a
// job whose files are all empty or missing was never handed a token, which a
// file the launch could not write reads back as.
func tokenFilesHold(ctx context.Context, box sandbox.Sandbox, paths []string) bool {
	for _, path := range paths {
		if held, err := box.ReadFile(ctx, path); err == nil && len(held) > 0 {
			return true
		}
	}
	return false
}

// failure reports whether the story is one a reader of the log has to act on,
// written at warning; the one wait that is not is a person's step the launch
// already said, written as information.
func (w tokenWait) failure() bool {
	return w.held || w.refused != "" || w.unwritable != "" || w.unrecorded != ""
}

// detail is the story's sentence. NEVER THE TOKEN: the reason is the
// issuer's error, which carries neither the token nor its vendor's response
// body.
func (w tokenWait) detail() string {
	switch {
	case w.unwritable != "" && w.held:
		return "the seat's issued token was renewed and could not be written into this " +
			"file: " + w.unwritable + "; the token the file holds is no longer renewed, and " +
			"its vendor refuses it once it expires. A file the job replaced with a link or a " +
			"directory, or whose directory it made read-only, is never rewritten; the first " +
			"poll whose write succeeds puts the current token there"
	case w.unwritable != "":
		return "the seat's issued token could not be written into this file, so the run " +
			"has no token there: " + w.unwritable + "; the first poll whose write succeeds " +
			"puts the current token there"
	case w.unrecorded != "" && w.held:
		return "the seat's issued token was renewed and could not be recorded for the scrub of " +
			"the run's sessions: " + w.unrecorded + "; it is not written into the box until it " +
			"is, so these files keep the token they hold, and every poll asks again"
	case w.unrecorded != "":
		return "the seat's issued token could not be recorded for the scrub of the run's " +
			"sessions: " + w.unrecorded + "; it is not written into the box until it is, so the " +
			"run has no token in these files, and every poll asks again"
	case w.refused != "" && w.held:
		return "the seat's token could not be issued: " + w.refused + "; the run keeps " +
			"the token it holds until that expires, and it is asked for again on the " +
			"restart schedule"
	case w.refused != "":
		return "this run was launched without the seat's token, which could not be " +
			"issued: " + w.refused + "; it is asked for again on the restart schedule, " +
			"and the first poll after it is issued writes it into these files"
	case w.held:
		return "the seat's token is no longer issued: the step its integration's card " +
			"asks for is undone, so nothing renews the token this run holds, which its " +
			"vendor refuses once it expires; the first poll after it is issued again " +
			"writes a new one"
	default:
		return "this run was launched without the seat's token, which is issued once " +
			"the step its integration's card asks for is taken; the first poll after " +
			"that writes it into these files"
	}
}

// tokenWaits is what this node's polls have said of the jobs they reach whose
// seat's tokens could not be issued, could not be written into one of the
// job's files, or could not be recorded for the scrub of the run's sessions
// before they were written: each story ONCE PER JOB (once per file for a
// write, once per variable for a record), and a refusal once per reason its
// vendor gives, rather than on every pass, which at the poll's interval and
// the nudges besides buried the one line under a hundred copies of it for the
// life of the run.
//
// IN MEMORY, keyed by the job ([sandbox.PendingRun.LaunchID] under its run;
// [tokenFileJob] for one of its files; [tokenRecordJob] for the record of one
// of its variables), and forgotten once the job is issued every token again,
// the file takes it, or the variable's record lands or finds the job no longer
// running ([tokenWaits.settle]), or goes unpolled on this node past
// [Engine.tokenWaitForget]: a restart, a duty that moved, a run that ended.
// Each costs at most one more line where the next poll finds the job still
// waiting, which is the right thing to say to a reader of that node's log.
type tokenWaits struct {
	mu   sync.Mutex
	said map[string]tokenWaitSaid
	now  func() time.Time
}

// tokenWaitSaid is one job's wait as last said, and when a poll last found it.
type tokenWaitSaid struct {
	story tokenWait
	seen  time.Time
}

// tokenWaitForget is how long a job may go unpolled on this node before what
// was said of it is forgotten: the longest a running job's box goes
// unanswered before its waiter gives it up ([sandbox.ConnectGiveUp]), plus
// one of this node's poll intervals, past which a job not polled is one this
// node no longer polls.
func (e *Engine) tokenWaitForget() time.Duration {
	interval := e.sandboxPollInterval
	if interval <= 0 {
		interval = sandbox.DefaultPollInterval
	}
	return sandbox.ConnectGiveUp + interval
}

// tell logs job's wait where it is not the one last said of it, forgetting
// the jobs no poll has found for forget ([Engine.tokenWaitForget]).
func (w *tokenWaits) tell(ctx context.Context, job string, story tokenWait, forget time.Duration, attrs []any) {
	if !w.first(job, story, forget) {
		return
	}
	attrs = append(attrs, "detail", story.detail())
	if story.failure() {
		log.WarnContext(ctx, "sandbox_token_refresh_failed", attrs...)
		return
	}
	log.InfoContext(ctx, "sandbox_token_waiting", attrs...)
}

// first records story as job's wait, and reports whether it was not already
// the one said. Jobs no poll has found for forget are forgotten on the way.
func (w *tokenWaits) first(job string, story tokenWait, forget time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if w.now != nil {
		now = w.now()
	}
	for key, said := range w.said {
		if now.Sub(said.seen) > forget {
			delete(w.said, key)
		}
	}
	if w.said == nil {
		w.said = make(map[string]tokenWaitSaid)
	}
	said, known := w.said[job]
	w.said[job] = tokenWaitSaid{story: story, seen: now}
	return !known || said.story != story
}

// settle forgets job's wait: its seat is issued every token again, the file
// the key names took it, or the record the key names landed or found the job
// no longer running, so a later wait is a new one and said as such.
func (w *tokenWaits) settle(job string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.said, job)
}

// tokenFiles are the paths among steps' files whose whole body names the
// variable name: the files a box holds the seat's issued token in.
func tokenFiles(steps []sandbox.SetupStep, name string) []string {
	var out []string
	for _, step := range steps {
		for path, body := range step.Files {
			if ref, whole := envref.Whole(body); whole && ref == name {
				out = append(out, path)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
