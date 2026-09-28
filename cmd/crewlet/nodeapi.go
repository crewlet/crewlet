package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
)

// Finding the running node, and authenticating to it.
//
// Several of this binary's estates live inside the engine's own process rather
// than in a file a second process can open: the company's secrets, on the
// coordination KV, the fleet's activation pointer, and the live counters
// `budgets` reads. On the default topology none of them listens on a socket,
// so the running node's API is the only way in — and every command that wants
// one has to answer the same two questions first: which address, and which
// token.
//
// ONE PLACE FOR ALL OF THEM. This rule was written twice — once in
// [nodeClient] and once inside the secrets client — and the two had already
// drifted on what to do when Tier A lists no token at all, so the same
// deployment got a clear refusal from one command and a bare 401 from the
// other. A third copy was one `crewlet config -api` away.

// apiTokenEnv is where every node client reads the bearer token it sends, and
// the ONLY place: not Tier A's `api.auth.tokens` (see [nodeTokenOrEmpty]) and
// not a flag.
//
// An ENV VAR rather than a flag, because a token on a command line is in the
// shell history, in `ps`, and in any CI log that echoes the command — the same
// reason `secrets set` reads its value from stdin. The node commands carried a
// `-token` flag anyway, documented, beside this comment; it is gone, and with
// it the second source a 401 had to send an operator to check.
const apiTokenEnv = "CREWLET_API_TOKEN"

// nodeBaseURL is the API address of the node a Tier A file describes, or the
// override if one was given.
//
// The BIND ADDRESS is what Tier A carries, and a bind address is not always a
// reachable one: 0.0.0.0 and :: mean "every interface", which as a destination
// means nothing at all. They resolve to loopback here, because a command
// reading a node's own config is running on that node — and the override is
// there for the case where it is not. surface names what the caller wanted to
// reach, so a node serving no HTTP says which thing is out of reach.
func nodeBaseURL(boot *config.Bootstrap, override, surface string) (string, error) {
	base := strings.TrimSpace(override)
	if base == "" {
		if boot.API.Port == 0 {
			return "", fmt.Errorf(
				"this node's api.port is 0, so it serves no HTTP surface and "+
					"there is no way to reach %s: set api.port, or name "+
					"another node's address", surface)
		}
		host := strings.TrimSpace(boot.API.Host)
		switch host {
		case "", "0.0.0.0", "::", "[::]":
			host = "127.0.0.1"
		}
		base = "http://" + net.JoinHostPort(host, strconv.Itoa(boot.API.Port))
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("%q is not a URL the API can be reached at "+
			"(want something like http://127.0.0.1:8000)", base)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// nodeTokenOrEmpty is the bearer token to send, or "" when there is none.
//
// # The environment, and nothing else
//
// It used to fall back to Tier A's FIRST token, which was wrong in a way the
// identity estate makes plain: a write's author is now a person or a machine
// with a row, a trail and grants of its own, and picking whichever credential
// happened to be listed first meant an operator's `crewlet secrets set` landed
// under a name they had not chosen and might not hold. `api.auth.tokens` is
// what this node ACCEPTS; it is not a wallet the CLI helps itself from.
//
// It also read a value out of the config file it had just parsed — a resolved
// `${VAR}`, in the clear, in a process that had no other reason to hold one —
// which is the shape a credential leaks from.
//
// So the CLI carries its own credential or says so. `CREWLET_API_TOKEN` is
// where it comes from, never a flag: a token on a command line lands in shell
// history and in `ps`.
func nodeTokenOrEmpty() string {
	return strings.TrimSpace(os.Getenv(apiTokenEnv))
}

// nodeAPIToken is [nodeTokenOrEmpty] for the surfaces that are always guarded.
//
// `/secrets` and `/config` authenticate every request including reads, so
// having no token is not "send none and see" — it is a 401 the operator will
// have to diagnose from the far end. Saying so here names the fix instead.
//
// THERE IS NO LONGER AN ESCAPE ARM. `api.auth.disabled` used to make an empty
// token legitimate here, and it is gone: every guarded route now needs a
// credential on every posture, so an empty token is always the 401 this
// message describes.
func nodeAPIToken(surface string) (string, error) {
	if token := nodeTokenOrEmpty(); token != "" {
		return token, nil
	}
	return "", fmt.Errorf(
		"nothing can authenticate to this node's %s surface: export %s with "+
			"one of the values in api.auth.tokens, or a machine token minted "+
			"by `crewlet iam token`", surface, apiTokenEnv)
}

// signInSurface builds /auth, or reports that this node serves none.
//
// # One posture produces a nil, and it is not a fault
//
// THIS NODE STARTED WITH NO COMPANY. Every node runs the identity domain,
// whatever its roles, but the state log it rides on is part of the native
// runtime, and a node that booted before any revision was active opens none —
// it serves its HTTP surface unconfigured until the first revision arrives.
// Such a node has no directory to sign anybody in against, and the routes are
// ABSENT rather than answering an error, as every other surface the native
// runtime feeds is on that node.
//
// A keyring that cannot sign for the fleet USED TO BE a second such posture,
// and it is not any more: Tier A refuses a file without a usable keyring and
// the engine refuses to start without one, so a session signer this function
// cannot build is a fault it returns rather than a node it quietly narrows.
func signInSurface(boot *config.Bootstrap, e *engine.Engine) (
	*authapi.Service, *auth.Sessions, error) {

	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		log := logging.Get("cli")
		log.Info("api_sign_in_absent",
			"reason", "this node started with no active company, so it runs "+
				"no native runtime and holds no identity directory",
			"hint", "activate a company revision and restart the node")
		return nil, nil, nil
	}
	signer, err := session.New(session.Options{
		Material: boot.Secrets.TokenMaterial(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the session signer: %w", err)
	}
	// THIS NODE'S OWN CURVE, and nobody else's: a guessing run a load
	// balancer rotates across the fleet meets each node's separately, which
	// is the residual internal/iam/credential's throttle states and bounds.
	throttle := credential.NewThrottle(credential.ThrottleDeps{})
	// THE SEALER, checked here for the directory's reason: a typed nil in
	// the surface's interface would pass its own refusal.
	sealer := e.PersonSealer()
	if sealer == nil {
		return nil, nil, errors.New("api: the sign-in surface: this node " +
			"holds no keyring to seal a second factor with")
	}
	surface, err := authapi.New(authapi.Options{
		Bootstrap: boot,
		Directory: reader,
		// THE NODE'S OWN WRITER, which acts as the deployment. What the
		// routes do with it is create people and open sessions, both of
		// which are the deployment's to do on somebody's behalf — a
		// person cannot author their own enrolment, because they do not
		// exist until it lands.
		Writer:   writer,
		Signer:   signer,
		Hasher:   credential.NewHasher(credential.Default(), credential.VerifyCap()),
		Throttle: throttle,
		Blinder:  e.PersonBlinder(),
		Sealer:   sealer,
		Sessions: reader,
		// THE SAME PURE FUNCTION THE GUARD USES over the same Tier A,
		// which is one PARSER rather than one instance — see
		// [auth.Clients].
		Clients: auth.NewClients(boot),
		// THE NODE'S ONE AUDIT TRAIL, which the guard and the directory
		// hand what they saw to as well: a failed sign-in and a refused
		// bearer fold into one row per client per minute only because
		// both reach the same tally.
		Audit: e.AuthEvents(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the sign-in surface: %w", err)
	}
	// AND THE OTHER HALF, built from the SAME signer. A cookie minted
	// under one key and validated against another is a sign-in that
	// appears to work and then does not stick — and it would do so only
	// on the requests that landed on a node whose signer was built
	// separately, which is the shape nobody reproduces.
	sessions, err := auth.NewSessions(auth.SessionsDeps{
		Signer:    signer,
		Directory: reader,
		// THE SAME READER'S APPLIER, which a write presenting a session
		// this node has not applied yet waits on — the sign-in above
		// answers before it does.
		Applier: reader,
		// THE CHART VIEW, and the ZERO VALUE on a node with no chart
		// domain — never nil, which internal/iam/session reads as the
		// seatless arm. See [engine.SeatViewOf].
		Chart:    engine.SeatViewOf(e),
		External: boot.API.ExternalBase(),
		// The same trail, whose once-per-lineage claim is what keeps a
		// cookie presented past its deadline announcing that ending once.
		Audit: e.AuthEvents(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the session arm: %w", err)
	}
	return surface, sessions, nil
}

// seatHeld reports whether a seat is one somebody in the identity directory is
// bound to, or nil on a node with no directory to ask.
//
// # The nil says nobody can be asked, which is not "nobody holds it"
//
// A node that started with no active company runs no native runtime and holds
// no identity rows at all, and read as a directory that would answer "not
// held" for every seat in the company — which reads as "nobody works here".
// That is the shape of the bug the continuous report already had for a
// different reason: it read a seat's declared contact block, so a company
// managing its people elsewhere saw every human seat reported.
//
// So such a node supplies NO ANSWER, and the report skips that arm rather than
// answering it. A directory that is there and cannot be read answers an error,
// which leaves the arm undecided the same way. See [chartapi.Held].
func seatHeld(e *engine.Engine) chartapi.Held {
	reader := e.IAM()
	if reader == nil {
		return nil
	}
	// THE CALLER'S CONTEXT, which the seam carries: every evaluation is
	// made for a request — /chart/check, /health, the seat listing — so a
	// read for one that has gone has nobody to answer.
	return reader.HeldSeats
}

// directorySurface builds /iam, or reports that this node serves none.
//
// NIL IS A REAL POSTURE, exactly as [signInSurface]'s is and for the same
// reason: a node that started with no active company holds no identity rows,
// and a surface over it would serve an empty directory as though the company
// had nobody in it. The routes are ABSENT rather than answering an error —
// which takes returning an untyped nil; see [surfaceMounter] for what a typed
// one did.
func directorySurface(boot *config.Bootstrap, e *engine.Engine) (surfaceMounter, error) {

	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		logging.Get("cli").Info("api_directory_absent",
			"reason", "this node started with no active company, so it runs "+
				"no native runtime and holds no identity directory",
			"hint", "activate a company revision and restart the node")
		return nil, nil
	}
	// THE KEYRING'S OPENER, checked here rather than handed in as a typed
	// nil: an interface holding a nil pointer is not nil, so the surface's
	// own refusal would never see it and the first name it opened would
	// panic.
	sealer := e.PersonSealer()
	if sealer == nil {
		return nil, errors.New("api: the identity directory: this node holds " +
			"no keyring to open anybody's name with")
	}
	surface, err := iamapi.New(iamapi.Options{
		Directory: reader,
		// ONE WRITER PER CALLER. The node's own writer acts as the
		// DEPLOYMENT, which is right for a bootstrap and wrong for
		// everything here: a directory whose author field is the node
		// is not an audit trail. The party is the caller's principal,
		// whole — its name, the credential beside it, its grants and its
		// id, which a person's own mint is decided on.
		Authority: func(principal iam.Principal) iamapi.Writer {
			return writer.As(principal)
		},
		Opener:       sealer,
		ExternalBase: boot.API.ExternalBase(),
		// THIS NODE'S OWN CEILING, which the report compares a person's
		// declared grants against: it is applied at decision time and
		// never written, so a fleet mid-rollout legally disagrees and
		// nothing else would say so.
		Ceiling:  boot.API.Auth.MaxGrants,
		Bindings: danglingBindings(e),
		// What an administrator did — a token minted or revoked, a
		// session ended, a person removed — on the node's audit feed.
		Audit: e.AuthEvents(),
	})
	if err != nil {
		return nil, fmt.Errorf("api: the identity directory: %w", err)
	}
	return surface, nil
}

// danglingBindings is the dangling-binding arm of the directory report, or nil
// where this node cannot run it.
//
// THE ENGINE'S RULE, not one written here: [engine.Engine.DanglingBinding] is
// the request path's own seat table applied to a person's row, and the
// `iam_binding_dangling` alarm asks the same function — so the report and the
// alarm cannot disagree about which binding dangles. The seam it replaced
// asked only whether the chart held a row by that handle, which is how a
// person bound to an AGENT seat went unreported while every request they made
// was refused.
//
// NIL ON A NODE WITH NO CHART READER — one that started with no active
// company — which is the same third value [seatHeld] answers with: there are
// no rows to ask, so the arm is skipped rather than asked.
func danglingBindings(e *engine.Engine) iamapi.Bindings {
	if e.Chart() == nil {
		return nil
	}
	return func(ctx context.Context, row iamdomain.PersonRow) (bool, string, error) {
		residue, dangling, err := e.DanglingBinding(ctx, row)
		return dangling, residue.Detail, err
	}
}

// identityOf is what /health says about whether anybody is enrolled, or nil
// where this node holds no identity rows — one that started with no active
// company — which the health body answers by leaving the field out rather than
// calling the company unclaimed.
//
// NIL AND NEVER A FUNCTION OVER A NIL READER, which would panic on the first
// probe.
func identityOf(e *engine.Engine) api.Identity {
	reader := e.IAM()
	if reader == nil {
		return nil
	}
	return reader.AnyPerson
}

// announceUnclaimed says, once at boot, what an operator does next with a
// company nobody is enrolled in yet: invite its first person under a Tier A
// token, exactly as every later person is invited.
//
// A LOG LINE AND NOTHING ELSE. There is no founder route and no code to
// write: the Tier A token every serving node already requires is the
// credential a company has before it has anybody, so the first invitation is
// an ordinary one. An estate this node cannot read says nothing here — /health
// answers `unknown` for it — rather than telling an operator to invite
// somebody into a company that may have started.
func announceUnclaimed(ctx context.Context, e *engine.Engine) {
	reader := e.IAM()
	if reader == nil {
		return
	}
	enrolled, err := reader.AnyPerson(ctx)
	if err != nil || enrolled {
		return
	}
	logging.Get("cli").Warn("iam_unclaimed",
		"detail", "this company has nobody in it; invite its first person "+
			"with `crewlet iam invite <address> -grants <grants>` and "+
			apiTokenEnv+" set to one of api.auth.tokens, then send them the "+
			"link it prints")
}
