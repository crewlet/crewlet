package engine

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// An agent's own GitHub App token, issued where the seat's values are read.
//
// A seat whose `role.integrations.github` app is created and installed acts
// on GitHub as that app, and the only credential GitHub accepts from it is an
// installation token ([github.Issuer]). Configuration names it the way it
// names every credential, `${GITHUB_APP_TOKEN_<HANDLE>}` ([github.SeatVar]),
// in the three places a seat's credentials are read:
//
//   - its tool servers' `role.mcp_env` block, read by [seatSpecs] for the
//     servers this node runs for the seat;
//   - its coding box's environment and setup files, `role.sandbox.env` and
//     the setup steps, read by [Engine.runEnvironment] for every executor
//     launch;
//   - the tool servers that go INTO that box, `role.sandbox.mcp`, read by
//     [sandboxMCP].
//
// # Answered by the seat's own resolution, and nothing else's
//
// Each of those reads goes through [Engine.seatResolver]: the company's chain
// with one [seatToken] laid in front of it, which answers this seat's
// variable and no other. Another seat naming it resolves nothing, and is told
// the variable is not set; the company's shared servers, which act as no
// seat, never see it at all. That is what keeps an app's identity with the
// agent it was created for, by construction rather than by a check somebody
// has to remember.
//
// # Issued only when read
//
// The source mints at its first lookup, so a seat whose configuration never
// names its token costs GitHub no request, and a pass over the seat's values
// that names it twice mints once. The token is held by the process-wide
// issuer, never by the source past the read that built it.
//
// # When it cannot be issued
//
// Three stories, told apart as the rest of the credential rule tells its own
// (toolcredentials.go):
//
//   - THE APP IS NOT CREATED OR NOT INSTALLED, or its key does not resolve.
//     That is a person's step, and the GitHub integration's seat-app pass
//     reports it on the card. The variable is declared as the GitHub
//     integration's to issue ([Engine.mintedFor]), so the value it is read
//     into is held back as WAITING (or as stopped until a person acts, by
//     what that pass last recorded) exactly as a provisioning pass's seal
//     is, and released when the reconcile records the installation, which
//     applies a revision and runs every read again.
//   - GITHUB REFUSED THE MINT, or could not be reached. That is a failure
//     with a reason, [credentialMissing.unissued], and it is retried on the
//     restart schedule: the process's issuer holds the refusal for the
//     schedule's wait ([github.Issuer.RetryWait], [Engine.seatRetryDelay]),
//     so every reader of one installation meets it within the wait and the
//     first after it asks GitHub again. The seat's children are passed again
//     on that schedule, and a running box's token file is written by the
//     first poll after GitHub issues one ([Engine.refreshRunTokens]); a
//     refusal that lasts, which a person ends at GitHub, costs one request a
//     wait, never one per poll, per nudge, per launch and per delivery.
//     Which of the two it was decides what the failure says ([mintPasses]):
//     GitHub not answering is a wait the next ask may end, and GitHub
//     refusing (a revoked key, a suspended installation, a permission
//     nobody approved) is a person's step on the agent's app, refused again
//     at every ask until somebody takes it.
//   - THE GITHUB INTEGRATION IS NOT ENABLED. Nothing issues anything: the
//     gesture an operator makes after a leak is switching the block off,
//     and it has to stop every token the engine hands out, so the variable
//     is one nothing sets, and is reported as such.
//
// In none of the three is an executor run refused: it starts without the
// value, said as its launch's `sandbox_env_unresolved`, because a seat's every
// turn is one and a seat refused its turn for a token would do no work at all,
// GitHub's or anyone else's. A token FILE it was launched without is the
// poll's to write once the token can be issued — the app installed, the mint
// answered, the integration switched back on — and the poll says each story
// once per job, a refusal once per reason GitHub gives and a token file it
// cannot write once for that file, rather than on every pass ([tokenWait]).

// seatToken answers one seat's own GitHub App token, issued at its first
// lookup.
//
// A [config.Source] for ONE RESOLUTION: built for a pass over one seat's
// values, consulted while those values are read, and dropped with the pass.
// It holds the pass's context for that reason alone: [config.Source] has no
// context of its own to thread, and a mint begun by a pass must end with it.
type seatToken struct {
	ctx    context.Context
	issuer *github.Issuer
	opts   github.SeatAppOptions
	app    github.SeatApp
	name   string
	life   time.Duration

	once  sync.Once
	token github.InstallationToken
	err   error
}

// Lookup implements [config.Source]: this seat's token for its own variable,
// and nothing for any other name.
func (s *seatToken) Lookup(name string) (string, bool) {
	if s == nil || name != s.name {
		return "", false
	}
	s.once.Do(func() {
		s.token, s.err = s.issuer.Token(s.ctx, s.opts, s.app, s.life)
	})
	if s.err != nil {
		return "", false
	}
	return s.token.Token, true
}

// SourceName is how a shadow warning names this source: a stale value the
// secret store or the environment holds under the seat's token variable is
// hidden by the token issued here, and it is that value the operator removes
// ([config.Resolver]).
func (s *seatToken) SourceName() string {
	return "the GitHub App token the engine issues for this seat"
}

// issued is the token this resolution handed out, and whether it handed one
// out at all: a read that never named the variable mints nothing.
func (s *seatToken) issued() (github.InstallationToken, bool) {
	if s == nil {
		return github.InstallationToken{}, false
	}
	// Read after the pass's reads, on the goroutine that ran them, so the
	// once has either run or will never run for this resolution.
	if s.err != nil || s.token.Token == "" {
		return github.InstallationToken{}, false
	}
	return s.token, true
}

// unissued is why name could not be issued by a mint that was attempted, or
// "". An app not yet created or installed is "" too: that is the waiting
// story, which the GitHub integration's own record tells.
//
// NEVER THE TOKEN, and never GitHub's response body: the error is the
// issuer's, which carries neither.
func (s *seatToken) unissued(name string) string {
	if s == nil || name != s.name || s.err == nil || errors.Is(s.err, github.ErrNotInstalled) {
		return ""
	}
	return s.err.Error()
}

// passes reports whether the mint this resolution attempted failed in a way a
// wait clears ([mintPasses]); false where none failed.
func (s *seatToken) passes() bool {
	return s != nil && s.err != nil && mintPasses(s.err)
}

// mintPasses reports whether a mint that failed with err is one a wait clears:
// GitHub not reached (a request that got no answer, its own timeout among
// them), or answering that it could not just now (408, its rate limit, which
// it answers 429 or 403 ([github.APIError.RateLimited]), a 5xx).
//
// ANYTHING ELSE IS A PERSON'S, and refused again at every ask until it is
// taken: an answer refusing the app (401, 403, 404, 422: a key GitHub does not
// take, an installation suspended or removed, a permission nobody approved),
// and a key the engine cannot sign a request with, which never reached GitHub.
// Told as a wait, a suspended app was said to be asked for again, as though
// the asking would end it; told as a person's, one blip of GitHub's sent an
// operator to fix an app nothing was wrong with.
func mintPasses(err error) bool {
	var answered *github.APIError
	if errors.As(err, &answered) {
		return answered.RateLimited || answered.Status == http.StatusRequestTimeout ||
			answered.Status >= http.StatusInternalServerError
	}
	var unanswered *url.Error
	return errors.As(err, &unanswered)
}

// seatResolver is how one seat's values are read: the company's chain, with
// the seat's own GitHub App token laid in front of it where the seat has an
// app and the company runs GitHub. life is how long a token it issues must
// stay good after the read ([github.ToolTokenLife], [github.RunTokenLife]).
//
// The [seatToken] comes back too, nil where there is none, so the caller can
// say why a value naming it went without ([seatMint]) and when what it was
// handed needs renewing.
func (e *Engine) seatResolver(
	ctx context.Context, c *Company, seat *org.Role, life time.Duration,
) (*config.Resolver, *seatToken) {
	base := e.resolver()
	// AN OFF SEAT IS ISSUED NOTHING (D1): its installation is suspended,
	// and a token minted before the suspension landed must not be handed on.
	if c == nil || seat == nil || !seat.IsAgent() || seat.GitHub == nil || !seat.GitHub.Enabled {
		return base, nil
	}
	api, ok := githubIssuing(c, base)
	if !ok {
		return base, nil
	}
	token := &seatToken{
		ctx: ctx, issuer: &e.githubIssuer,
		opts: github.SeatAppOptions{APIBase: api},
		app:  githubSeatApp(seat, base),
		name: github.SeatVar(seat.Handle(), github.FieldAppToken),
		life: life,
	}
	return base.Ahead(token), token
}

// githubIssuing reports whether this company issues its agents' GitHub App
// tokens, with the REST base it issues them against. Only while GitHub is
// enabled: switching the block off is how an operator stops every token.
func githubIssuing(c *Company, env *config.Resolver) (string, bool) {
	cfg := c.Config.Integrations.GitHub
	if cfg == nil || !cfg.Enabled {
		return "", false
	}
	resolved := *cfg
	resolved.URL = strings.TrimSpace(env.Value(cfg.URL))
	return resolved.APIBase(), true
}

// githubIssued is the variable the GitHub integration issues for handle's
// seat, or "" where it issues none: the company does not run GitHub, or the
// handle is not an agent's. Declared for EVERY agent seat while GitHub runs,
// app or not, because a seat naming its token before its app exists is
// waiting on the step the integration's card asks a person for.
func githubIssued(c *Company, handle string) string {
	if c == nil {
		return ""
	}
	if cfg := c.Config.Integrations.GitHub; cfg == nil || !cfg.Enabled {
		return ""
	}
	if c.Org == nil || c.Org.AgentSeatByHandle(handle) == nil {
		return ""
	}
	return github.SeatVar(handle, github.FieldAppToken)
}

// issuedAtUse reports an integration whose seat credential the engine issues
// when it is read rather than one a pass seals into the store, which changes
// what a value waiting on it is waiting FOR: the app's installation, not a
// seal.
func issuedAtUse(kind integration.Kind) bool { return kind == integration.KindGitHub }

// refreshRunTokens keeps a running box's GitHub App token file current: the
// completion poll's [sandbox.RefreshFunc], and the launch's for a job that
// continues in its turn's kept box.
//
// # A run outlives the token it was launched with
//
// An executor's environment is fixed when its box starts, and its token in
// that environment is issued good for [github.RunTokenLife]. A run is never
// stopped on a clock, so a long one would push with a token GitHub refuses,
// halfway through its work. What can change under a running process is a
// FILE: a setup step that writes the seat's token to one (a file whose
// whole body is `${GITHUB_APP_TOKEN_<HANDLE>}`, provisioned at launch like
// any other) is rewritten here, on the poll, with the token the issuer holds
// once it renews it, and a credential helper reading that file at the moment
// of use always reads a token that is good. The documented git-auth recipe
// reads it that way (docs/concepts/code-sandbox.md). The rewrite REPLACES the
// file whole ([sandbox.Sandbox.WriteFile]): written in place, it was empty and
// then half written for as long as the write took, and a push whose helper
// read it then authenticated with nothing.
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
// read. A file the run was launched WITHOUT — its token could not be issued
// then, so the launch wrote none ([Engine.runEnvironment]) — reads back as
// nothing, which differs from any token, so the first poll after the app can
// issue one writes it. The composition of steps is the launch's own
// ([boxSetup]), read off the company current now: a file a revision has since
// removed is no longer kept, which is what the operator who removed it asked
// for.
//
// NEVER LOGGED, never returned: the lines here name the seat, the path and
// when the token expires.
func (e *Engine) refreshRunTokens(
	ctx context.Context, manager *sandbox.Manager, run sandbox.PendingRun, box sandbox.Sandbox,
) {
	e.refreshTokenFiles(ctx, manager.DefaultSetup(), run, box)
}

// refreshTokenFiles is [Engine.refreshRunTokens] over the provider-wide setup
// steps defaults, which the poll reads off the manager that reached the box.
func (e *Engine) refreshTokenFiles(
	ctx context.Context, defaults []sandbox.SetupStep, run sandbox.PendingRun, box sandbox.Sandbox,
) {
	c := e.Company()
	if c == nil || c.Org == nil {
		return
	}
	seat := c.Org.AgentSeatByHandle(run.AgentHandle)
	if seat == nil {
		return
	}
	name := githubIssued(c, seat.Handle())
	if name == "" {
		return
	}
	paths := tokenFiles(boxSetup(defaults, seat.Sandbox), name)
	if len(paths) == 0 {
		return
	}
	_, token := e.seatResolver(ctx, c, seat, github.RefreshTokenLife)
	value, ok := token.Lookup(name)
	job := run.TurnID + "/" + run.LaunchID
	if !ok {
		attrs := []any{"seat", seat.Handle(), "turn_id", run.TurnID, "files", paths}
		story := tokenWait{held: tokenFilesHold(ctx, box, paths), refused: token.unissued(name)}
		e.tokenWaits.tell(ctx, job, story, e.tokenWaitForget(), attrs)
		return
	}
	e.tokenWaits.settle(job)
	issued, _ := token.issued()
	for _, path := range paths {
		file := tokenFileJob(job, path)
		held, err := box.ReadFile(ctx, path)
		if err == nil && subtle.ConstantTimeCompare(held, []byte(value)) == 1 {
			e.tokenWaits.settle(file)
			continue
		}
		if err := box.WriteFile(ctx, path, []byte(value)); err != nil {
			// ONCE FOR THE FILE, like every other story here: a file the
			// job replaced with a link or a directory, a directory it made
			// read-only around the file, or a box out of disk, fails the
			// same way on every poll and every nudge for the life of the job.
			story := tokenWait{held: len(held) > 0, unwritable: err.Error()}
			e.tokenWaits.tell(ctx, file, story, e.tokenWaitForget(),
				[]any{"seat", seat.Handle(), "turn_id", run.TurnID, "file", path})
			continue
		}
		e.tokenWaits.settle(file)
		log.InfoContext(ctx, "sandbox_token_refreshed", "seat", seat.Handle(),
			"turn_id", run.TurnID, "file", path, "expires_at", issued.ExpiresAt)
	}
}

// tokenFileJob is the key [tokenWaits] holds what was said of one of job's
// token files under: a file the poll cannot write is a story of its own,
// beside the job's story of a token that could not be issued, and each of a
// job's files is told apart.
func tokenFileJob(job, path string) string { return job + "\x00" + path }

// tokenWait is the story a poll tells of a running job whose seat's GitHub
// App token could not be issued now, read off two facts: whether the job's own
// files hold a token, and why the issue failed; or of one of its token files
// the poll could not write the issued token into.
//
// FOUR STORIES OF A TOKEN NOT ISSUED, because each pair of answers is a
// different thing to say:
//
//   - nothing held, nothing refused: the app is not created, not installed or
//     its key does not resolve, a person's step the GitHub integration's card
//     asks for ([github.ErrNotInstalled]), and the job was launched before it.
//     Its files hold nothing, and the first poll after the app can issue a
//     token writes them. The launch said the same wait as
//     `sandbox_env_unresolved`, so this is information, not a failure.
//   - a token held, nothing refused: the job was handed a token the app no
//     longer renews, which GitHub refuses once it expires, halfway through the
//     work, and which nothing else on this node would say.
//   - a token held, a refusal: GitHub refused the mint, or could not be
//     reached. The job keeps the token it holds until that expires.
//   - nothing held, a refusal: the same refusal, met by a job that has nothing
//     to keep. Every agent turn is an executor run and a run starts without a
//     token its seat cannot issue, so this is an ordinary path, not one a
//     refused launch rules out.
//
// AND ONE OF A FILE THE POLL CANNOT WRITE: the token is issued, and writing
// it into the file failed, so the job goes on with what the file holds — a
// token nothing renews any more, which GitHub refuses once it expires, or
// nothing at all. The job replaced the file with a link or a directory (a
// local box replaces only a regular file, and leaves either as the job made
// it), made the directory around it read-only (a file is replaced by one
// written beside it, so a file made read-only itself is no obstacle), or the
// box ran out of disk; each fails the same way on every poll, so the failure
// is told once for the file, and again only when its error changes. A write
// that succeeds settles it.
//
// THE REFUSAL'S REASON IS PART OF THE STORY, so a job is told each reason
// GitHub gives once, and told again only when GitHub's answer changes.
type tokenWait struct {
	// held is whether any of the job's token files holds a token: one its
	// launch wrote or an earlier poll did. For a file the poll cannot
	// write, whether that file does.
	held bool
	// refused is why a mint that was attempted failed ([seatToken.unissued]),
	// or "" where the seat's app cannot issue a token at all.
	refused string
	// unwritable is why the issued token could not be written into the
	// file, or "" for a story of a token not issued. The box's error, which
	// names the file and never what was being written.
	unwritable string
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
func (w tokenWait) failure() bool { return w.held || w.refused != "" || w.unwritable != "" }

// detail is the story's sentence. NEVER THE TOKEN: the reason is the
// issuer's error, which carries neither the token nor GitHub's response body.
func (w tokenWait) detail() string {
	switch {
	case w.unwritable != "" && w.held:
		return "the seat's GitHub App token was renewed and could not be written into this " +
			"file: " + w.unwritable + "; the token the file holds is no longer renewed, and " +
			"GitHub refuses it once it expires. A file the job replaced with a link or a " +
			"directory, or whose directory it made read-only, is never rewritten; the first " +
			"poll whose write succeeds puts the current token there"
	case w.unwritable != "":
		return "the seat's GitHub App token was issued and could not be written into this " +
			"file, so the run has no token there: " + w.unwritable + "; the first poll " +
			"whose write succeeds puts the current token there"
	case w.refused != "" && w.held:
		return "GitHub refused to issue the seat's GitHub App token: " + w.refused +
			"; the run keeps the token it holds until that expires, and GitHub is " +
			"asked again on the restart schedule"
	case w.refused != "":
		return "this run was launched without the seat's GitHub App token, and " +
			"GitHub refused to issue it: " + w.refused + "; GitHub is asked again " +
			"on the restart schedule, and the first poll after it issues one writes " +
			"it into these files"
	case w.held:
		return "the seat's GitHub App no longer issues a token: it is no longer " +
			"installed or no longer resolves its key, so nothing renews the token this " +
			"run holds, which GitHub refuses once it expires; the first poll after the " +
			"app is installed again writes a new one"
	default:
		return "this run was launched without the seat's GitHub App token, which " +
			"is issued once the app is created and installed and its key resolves; " +
			"the first poll after that writes it into these files"
	}
}

// tokenWaits is what this node's polls have said of the jobs they reach whose
// seat's token could not be issued, or could not be written into one of the
// job's files: each story ONCE PER JOB (once per file for a write), and a
// refusal once per reason GitHub gives, rather than on every pass, which at
// the poll's interval and the nudges besides buried the one line under a
// hundred copies of it for the life of the run.
//
// IN MEMORY, keyed by the job ([sandbox.PendingRun.LaunchID] under its run;
// [tokenFileJob] for one of its files), and forgotten once the job resolves a
// token again, or the file takes it ([tokenWaits.settle]), or goes unpolled on
// this node past [Engine.tokenWaitForget]: a restart, a duty that moved, a run
// that ended. Each costs at most one more line where the
// next poll finds the job still waiting, which is the right thing to say to a
// reader of that node's log.
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

// settle forgets job's wait: its seat issues a token again, or the file the
// key names took it, so a later wait is a new one and said as such.
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
