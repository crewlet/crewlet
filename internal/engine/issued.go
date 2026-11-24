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

// A seat's credential issued at use (package doc, "Seat credentials issued at
// use"; ADR-0058): GitHub's installation token (githubtoken.go), Google
// Cloud's access token (googlecloudtoken.go) and the like, named as a `${VAR}`
// in a seat's `role.mcp_env`, `role.sandbox.env` and setup steps, or
// `role.sandbox.mcp`. ONE CONTRACT, [seatIssuer]: the shared paths ask the
// registry ([Engine.seatIssuers]), never a kind. Every read goes through
// [Engine.seatResolver], which answers this seat's variable and no other, so
// an agent's identity stays with its agent by construction. Issued at the
// first lookup and cached by its issuer; when it cannot be, the value waits
// ([Engine.mintedFor], [seatToken.settling]) or fails with a reason
// ([seatToken.unissued]), and an executor run is never refused for it: the
// poll writes its token file once it can be issued ([Engine.refreshTokenFiles]).

// seatIssuer is an integration that issues each of its agents a credential at
// the moment one of the seat's values names it, rather than sealing one. The
// engine's wiring for the integration implements it (githubtoken.go), and
// [Engine.seatIssuers] lists every implementation.
type seatIssuer interface {
	// kind is the integration that issues: what a waiting value is told it
	// waits for.
	kind() integration.Kind

	// variable is what it issues the seat with handle under c, or "" where
	// it issues none: no company, the integration off, or handle no agent's.
	// Declared for every agent while the integration runs, issuable now or
	// not, since a seat naming it early waits on the integration's step.
	variable(c *Company, handle string) string

	// issuing is how seat's credential is issued for one resolution, its
	// settings read through env, or nil where the seat is issued nothing now
	// (off at the integration). Called once per resolution; the issue runs
	// at the source's first lookup.
	// A seat that is on but cannot be issued yet gets one failing with the
	// integration's waiting story, so it is told and retried
	// ([errIssueSettling]).
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
// lives the shared paths assume ([toolTokenLife], [runTokenLife]).
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

// The lives the shared paths ask an issued credential for: how long it must
// stay good after the read, as shares of [defaultLives]; the conformance
// suite holds each issuer to them.
const (
	// toolTokenLife is what a tool server on this node is started with; it is
	// restarted at its credential's renew time ([Engine.armRenewal]), so it
	// never carries one past its expiry.
	toolTokenLife = 10 * time.Minute

	// runTokenLife is what an executor's environment is given at launch,
	// fixed when its box starts: the most an hour-long credential can promise
	// while one issue still serves the next ten minutes' launches. A longer
	// run reads it through a file the poll keeps.
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
		googleCloudSeatIssuer{engine: e, tokens: &e.googleIssuer},
	}
}

// seatToken answers one variable an integration issues one seat, issued at
// its first lookup.
//
// A [config.Source] for ONE RESOLUTION, dropped with the pass; it holds the
// pass's context only because [config.Source] threads none, and an issue a
// pass begins must end with it. A resolution's tokens chain by next, one per
// variable, in the registry's order, each laid in front of the company's
// chain on its own so a shadow warning names the issuer that hid the value;
// a question about the whole resolution walks from the first.
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

// SourceName is how a shadow warning names this source, which hides a stale
// value the secret store or the environment holds under the seat's variable:
// the value the operator removes ([config.Resolver]).
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
// "": the waiting story is the integration's own record to tell. NEVER THE
// TOKEN or the vendor's response body: the reason is the issuer's.
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
// variables met the integration still settling it ([seatIssuer.refusal]: no
// reason, and passes). Told as waiting, and retried by the seat's children,
// since nothing sealed or applied will run the read again.
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
// The resolution's first token comes back too, nil where nothing issues the
// seat anything, so the caller can say why a value went without ([seatMint])
// and when what it was handed needs renewing.
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

// refreshRunTokens keeps a running box's token files current (package doc,
// "Token files in a running box"): the poll's [sandbox.RefreshFunc] on the
// waiter of the node that launched the box, and the launch's for a job
// continuing in a kept box ([sandbox.CoordinatorOptions.Refresh]), which no
// poll reached while it was paused. A file whose whole body is `${VAR}` is rewritten with the
// renewed token, REPLACED whole ([sandbox.Sandbox.WriteFile]) so no helper
// reads it half written, and only where the box's copy differs: asked of the
// box, never remembered. Each token is handed to record BEFORE any file holds
// it and written nowhere when record refuses, so every capture of the job's
// sessions is scrubbed of it; the next poll, fifteen seconds on, asks again,
// inside the five minutes an AWS session's renewal leaves. NEVER LOGGED.
func (e *Engine) refreshRunTokens(
	ctx context.Context, manager *sandbox.Manager, run sandbox.PendingRun, box sandbox.Sandbox,
	record sandbox.RecordHanded,
) {
	e.refreshTokenFiles(ctx, manager.DefaultSetup(), run, box, record)
}

// refreshTokenFiles is [Engine.refreshRunTokens] over the provider-wide setup
// steps defaults, read off the manager that reached the box. Each issued
// variable's files are refreshed from its own source: one that cannot be
// issued now leaves its files as they are, and the job's story names it.
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
			// Told ONCE FOR THE FILE: a link or a directory in its place, a
			// read-only directory or a full disk fails the same way on every
			// poll and nudge for the life of the job.
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
// the job's and its files'. Its separator differs from [tokenFileJob]'s, so
// a variable is never read as a path.
func tokenRecordJob(job, name string) string { return job + "\x01" + name }

// tokenWait is the story a poll tells of a running job: tokens its seat
// could not be issued now, read off whether the job's files hold a token and
// why the issues failed (four stories, each a different thing to say:
// [tokenWait.detail]); a token file the poll could not write (one the job
// replaced with a link or a directory, a read-only directory around it, a
// full disk; a read-only file is no obstacle, since it is replaced by one
// written beside it); or a token it could not record for the scrub
// ([Engine.recordToken]), so none of the variable's files is written.
// Comparable, so a story is told again only when an answer, the refusal's
// reason included, changes; its words are kind-agnostic, the variables
// beside them in the line.
type tokenWait struct {
	// held is whether any of the job's files for the tokens not issued holds
	// a token; for a file the poll cannot write, whether that file does.
	held bool
	// refused is why the issues that were attempted failed
	// ([seatToken.unissued]), or "" where the integrations cannot issue at
	// all yet.
	refused string
	// unwritable is why the issued token could not be written into the
	// file, or "": the box's error, which never names what was written.
	unwritable string
	// unrecorded is why the issued token could not be recorded before it was
	// written, or "": the sandbox's error, which never names the token.
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
// written at warning; the one that is not, a person's step the launch already
// said as `sandbox_env_unresolved`, is written as information.
func (w tokenWait) failure() bool {
	return w.held || w.refused != "" || w.unwritable != "" || w.unrecorded != ""
}

// detail is the story's sentence. NEVER THE TOKEN: the reason is the
// issuer's error, which carries neither it nor the vendor's response body.
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
// tokens could not be issued, written or recorded: each story ONCE per job
// (per file for a write, per variable for a record), and once per refusal
// reason, rather than on every poll and nudge for the life of the run.
//
// IN MEMORY, keyed by the job ([sandbox.PendingRun.LaunchID] under its run),
// [tokenFileJob] or [tokenRecordJob], and forgotten when settled
// ([tokenWaits.settle]) or unpolled here past [Engine.tokenWaitForget] (a
// restart, an ended run): each costs at most one more line
// where the job is still waiting.
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
// was said of it is forgotten: the longest a box goes unanswered before its
// waiter gives it up ([sandbox.ConnectGiveUp]) plus one poll interval, past
// which this node no longer polls the job.
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

// settle forgets job's wait (every token issued again, the keyed file
// written, or the keyed record landed or found the job not running), so a
// later wait is a new one and said as such.
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
