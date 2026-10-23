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
//     the setup steps, read by [Engine.runEnvironment] for both launch paths;
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
//     with a reason, [credentialMissing.unissued], and it is retried: the
//     seat's children on the restart schedule, a coding run at the seat's
//     next attempt. Which of the two it was decides what a coding run's
//     seat is told ([mintPasses]): GitHub not answering is a wait, and
//     GitHub refusing (a revoked key, a suspended installation, a
//     permission nobody approved) is a person's step on the agent's app,
//     refused again at every attempt until somebody takes it.
//   - THE GITHUB INTEGRATION IS NOT ENABLED. Nothing issues anything: the
//     gesture an operator makes after a leak is switching the block off,
//     and it has to stop every token the engine hands out, so the variable
//     is one nothing sets, and is reported as such.

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
// ANYTHING ELSE IS A PERSON'S, and refused again at every attempt until it is
// taken: an answer refusing the app (401, 403, 404, 422: a key GitHub does not
// take, an installation suspended or removed, a permission nobody approved),
// and a key the engine cannot sign a request with, which never reached GitHub.
// Told as a wait, a seat whose app was suspended tried again for as long as its
// turn lasted; told as a person's, a seat whose mint met one blip of GitHub's
// escalated a configuration fault that did not exist.
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
	if c == nil || seat == nil || !seat.IsAgent() || seat.GitHub == nil {
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
// completion poll's [sandbox.RefreshFunc].
//
// # A run outlives the token it was launched with
//
// A coding run's environment is fixed when its box starts, and its token in
// that environment is issued good for [github.RunTokenLife]. A run is never
// stopped on a clock, so a long one would push with a token GitHub refuses,
// halfway through its work. What can change under a running process is a
// FILE: a setup step that writes the seat's token to one (a file whose
// whole body is `${GITHUB_APP_TOKEN_<HANDLE>}`, provisioned at launch like
// any other) is rewritten here, on the poll, with the token the issuer holds
// once it renews it, and a credential helper reading that file at the moment
// of use always reads a token that is good. The documented git-auth recipe
// reads it that way (docs/concepts/code-sandbox.md).
//
// ASKED OF THE BOX, NOT REMEMBERED. The file is read back and rewritten only
// where it differs, so the poll's duty can move between nodes, and the
// engine can restart, with nothing to carry over, and a quiet poll costs one
// read. The composition of steps is the launch's own ([boxSetup]), read off
// the company current now: a file a revision has since removed is no longer
// kept, which is what the operator who removed it asked for.
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
	if !ok {
		attrs := []any{"seat", seat.Handle(), "turn_id", run.TurnID, "files", paths}
		if why := token.unissued(name); why != "" {
			log.WarnContext(ctx, "sandbox_token_refresh_failed", append(attrs,
				"detail", "the run keeps the token it holds and the next poll asks again: "+why)...)
			return
		}
		log.WarnContext(ctx, "sandbox_token_refresh_failed", append(attrs,
			"detail", "the seat's GitHub App is no longer installed or no longer "+
				"resolves its key, so nothing renews this run's token")...)
		return
	}
	issued, _ := token.issued()
	for _, path := range paths {
		held, err := box.ReadFile(ctx, path)
		if err == nil && subtle.ConstantTimeCompare(held, []byte(value)) == 1 {
			continue
		}
		if err := box.WriteFile(ctx, path, []byte(value)); err != nil {
			log.WarnContext(ctx, "sandbox_token_refresh_failed", "seat", seat.Handle(),
				"turn_id", run.TurnID, "file", path, "error", err.Error(),
				"detail", "the next poll writes it again")
			continue
		}
		log.InfoContext(ctx, "sandbox_token_refreshed", "seat", seat.Handle(),
			"turn_id", run.TurnID, "file", path, "expires_at", issued.ExpiresAt)
	}
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
