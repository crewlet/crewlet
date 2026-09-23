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
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
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

// apiTokenEnv is where every node client reads a bearer token that is not in
// Tier A.
//
// An ENV VAR rather than a flag, because a token on a command line is in the
// shell history, in `ps`, and in any CI log that echoes the command — the same
// reason `secrets set` reads its value from stdin.
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
			"(want something like http://127.0.0.1:8080)", base)
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
// # Two postures produce a nil, and neither is a fault
//
//   - THIS NODE RUNS NO IAM DOMAIN. It is the first domain in the register
//     that narrows: a seats-only satellite does not apply it, because no turn
//     reads identity and shedding a company's seats because a human cannot
//     sign in would be an outage caused by the wrong subsystem. Such a node
//     serves seats and no sign-in, which is what its `node.roles` asked for.
//   - THE KEYRING CANNOT SIGN FOR THE FLEET. A cookie minted under a
//     per-process key is one every other ingress node rejects, so a person
//     would be signed in on whichever node their request happened to reach.
//     Refusing to mint is the only honest answer, and `crewlet validate`
//     refuses that keyring by name — on a laptop, long before a bind.
//
// In both, the routes are ABSENT rather than answering an error. A 404 says
// this deployment does not sign in that way; a 503 would say it does and is
// broken, and send an operator looking for an outage.
func signInSurface(boot *config.Bootstrap, e *engine.Engine,
	cipher secrets.Cipher) (*authapi.Service, *auth.Sessions, error) {

	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		log := logging.Get("cli")
		log.Info("api_sign_in_absent",
			"reason", "this node runs no identity domain",
			"hint", "node.roles narrows which domains a node applies; a "+
				"seats-only satellite serves no sign-in surface")
		return nil, nil, nil
	}
	signer, err := session.New(session.Options{
		Material:    boot.Secrets.TokenMaterial(),
		RotateAfter: boot.API.Auth.Session.RotateAfter(),
	})
	if err != nil {
		if errors.Is(err, session.ErrNoKeyring) {
			logging.Get("cli").Warn("api_sign_in_absent",
				"reason", "this node's keyring cannot sign for the fleet",
				"error", err,
				"hint", "set secrets.keys and secrets.active_key; a cookie "+
					"signed under a per-process key is one every other node "+
					"rejects")
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("api: the session signer: %w", err)
	}
	throttle, err := credential.NewThrottle(credential.ThrottleDeps{
		// THE FLEET'S OWN WINDOW, so a caller guessing against three
		// ingress nodes is one attacker rather than three. Nil is a real
		// deployment — a single node with no coordination backend — and
		// it throttles on the local curve alone.
		Attempts: e.Backends().Fleet,
		Logger:   logging.Get("api.auth"),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the sign-in throttle: %w", err)
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
		Opener:   e.PersonSealer(),
		Sessions: reader,
		Cipher:   cipher,
		// THE SAME PURE FUNCTION THE GUARD USES over the same Tier A,
		// which is one PARSER rather than one instance — see
		// [auth.Clients].
		Clients:  auth.NewClients(boot),
		Provider: signInProvider(boot),
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
		// THE CHART VIEW, and the ZERO VALUE on a node with no chart
		// domain — never nil, which internal/iam/session reads as the
		// seatless arm. See [engine.SeatViewOf].
		Chart:    engine.SeatViewOf(e),
		External: boot.API.ExternalBase(),
		OnReuse:  sessionReuse(e),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the session arm: %w", err)
	}
	return surface, sessions, nil
}

// sessionReuse ends every session of a person whose cookie was replayed past
// the rotation overlap.
//
// THE EPOCH AND NOT THE ONE SESSION, because a cookie that was replayed is a
// cookie somebody else has, and the one thing nobody can establish from the
// replay is which of the two holders is the person. Bumping the revocation
// epoch ends them both and costs that person one sign-in; ending only the
// lineage would leave whoever captured it holding whatever they rotate to
// next.
//
// THE WRITE IS THE NODE'S OWN, not the person's: they did not ask for it, and
// an authentication trail that recorded them as the author of their own
// lockout would be wrong about the one row an investigation reads.
func sessionReuse(e *engine.Engine) func(context.Context, string) {
	return func(ctx context.Context, person string) {
		writer := e.IAMWriter()
		if writer == nil {
			return
		}
		// WITHOUT CANCEL, because the request this was noticed on is
		// about to be refused and its context cancelled — and a
		// revocation that inherits a dead context does nothing at all,
		// which is this engine's rule for every cleanup.
		ctx = context.WithoutCancel(ctx)
		opID := "session-reuse:" + person + ":" + uuid.NewString()
		if _, err := writer.Revoke(ctx, person, opID,
			"a session cookie was replayed past the rotation overlap"); err != nil {

			logging.Get("api.auth").ErrorContext(ctx,
				"iam_session_reuse_not_revoked", "person", person,
				"error", err,
				"detail", "the replayed cookie was refused, but this "+
					"person's other sessions are still live; retry with "+
					"crewlet iam revoke")
		}
	}
}

// signInProvider is the identity provider, or nil where none is configured.
func signInProvider(boot *config.Bootstrap) *oidc.Provider {
	block := boot.API.Auth.OIDC
	if block == nil || block.Issuer == "" {
		return nil
	}
	return oidc.NewProvider(oidc.Config{
		Issuer:       block.Issuer,
		ClientID:     block.ClientID,
		ClientSecret: block.ClientSecret,
		RedirectURI:  boot.API.ExternalBase() + auth.PathAuthOIDCCallback,
		RequireACR:   block.RequireACR,
	}, nil, nil)
}

// seatHeld reports whether a seat is one somebody in the identity directory is
// bound to, or nil on a node that cannot tell.
//
// # The nil is the third value, and it is the whole of this function
//
// A node that runs no identity domain has a legitimately EMPTY copy of that
// estate — it never applies the records — so asking it produces false for
// every seat in the company, which reads as "nobody works here". That is the
// shape of the bug the continuous report already had for a different reason:
// it read a seat's declared contact block, so a company managing its people
// elsewhere saw every human seat reported.
//
// So a node with no reader supplies NO ANSWER, and the report skips that arm
// rather than answering it. See [chartapi.Held].
func seatHeld(e *engine.Engine) chartapi.Held {
	reader := e.IAM()
	if reader == nil {
		return nil
	}
	return func(handle string) bool {
		// THE BACKGROUND CONTEXT, because this is asked while rendering a
		// report on a tick with no request to inherit: a per-seat read
		// bound to a cancelled request would make a page half-answer.
		return reader.SeatHeld(context.Background(), handle)
	}
}

// directorySurface builds /iam, or reports that this node serves none.
//
// NIL IS A REAL POSTURE, exactly as [signInSurface]'s is and for the same
// reason: a node that runs no identity domain holds a legitimately empty copy
// of that estate, and a surface over it would serve an empty directory as
// though the company had nobody in it. The routes are ABSENT rather than
// answering an error — which takes returning an untyped nil; see
// [surfaceMounter] for what a typed one did.
func directorySurface(boot *config.Bootstrap, e *engine.Engine, nodeID string,
	auth *authapi.Service) (surfaceMounter, error) {

	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		logging.Get("cli").Info("api_directory_absent",
			"reason", "this node runs no identity domain",
			"hint", "node.roles narrows which domains a node applies; a "+
				"seats-only satellite serves no directory")
		return nil, nil
	}
	surface, err := iamapi.New(iamapi.Options{
		Directory: reader,
		// ONE WRITER PER CALLER. The node's own writer acts as the
		// DEPLOYMENT, which is right for a bootstrap and wrong for
		// everything here: a directory whose author field is the node
		// is not an audit trail.
		Authority: func(actor string, kind iam.Kind, grants []iam.Grant) iamapi.Writer {
			return writer.As(actor, kind, grants)
		},
		Opener:       e.PersonSealer(),
		Bootstrap:    bootstrapReissue(nodeID, auth),
		ExternalBase: boot.API.ExternalBase(),
		// THIS NODE'S OWN CEILING, which the report compares a person's
		// declared grants against: it is applied at decision time and
		// never written, so a fleet mid-rollout legally disagrees and
		// nothing else would say so.
		Ceiling: boot.API.Auth.MaxGrants,
		Seats:   seatExists(e),
	})
	if err != nil {
		return nil, fmt.Errorf("api: the identity directory: %w", err)
	}
	return surface, nil
}

// bootstrapReissue is the one-time code's re-issue, or nil where this node
// serves no sign-in surface.
//
// THE SAME SERVICE THAT MINTS ONE AT BOOT, rather than a second
// implementation: the file's path, its mode, the hash that is published and
// the withdrawals that precede it are one sequence, and a copy of it here
// would be a second answer to "how many codes are live".
func bootstrapReissue(nodeID string, auth *authapi.Service) iamapi.Bootstrap {
	if auth == nil {
		return nil
	}
	return bootstrapMinter{auth: auth, node: nodeID}
}

type bootstrapMinter struct {
	auth *authapi.Service
	node string
}

func (b bootstrapMinter) MintCode(ctx context.Context) (string, error) {
	return b.auth.ReissueBootstrapCode(ctx, b.node)
}

// seatExists reports whether a seat is one this node's org chart holds, or
// nil on a node that cannot tell.
//
// THE MIRROR OF [seatHeld], one estate the other way round, and the nil is
// the same third value: a node running no chart domain has an empty copy of
// it, so asking would report EVERY bound person as dangling.
func seatExists(e *engine.Engine) iamapi.Seats {
	reader := e.Chart()
	if reader == nil {
		return nil
	}
	return func(handle string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), seatProbeBudget)
		defer cancel()
		_, err := reader.Seat(ctx, handle,
			statelog.Freshness{Level: statelog.ReadStale})
		// AN UNREADABLE CHART READS AS PRESENT, which is the direction
		// that does not raise a false alarm: reporting somebody's
		// binding as dangling because a read failed would send an
		// administrator to unbind a person whose seat is perfectly
		// there.
		return err == nil || !errors.Is(err, chart.ErrNotFound)
	}
}

// seatProbeBudget bounds one seat lookup inside the report.
//
// TWO SECONDS, and it is a per-ROW budget on a walk that may cover the whole
// directory — so the number is what one local SQL read on a busy node costs
// at its worst rather than what a network call would. A probe that cannot
// answer inside it reads as present, which is the arm above.
const seatProbeBudget = 2 * time.Second

// openBootstrap writes this node's one-time founder code when the company has
// nobody in it.
//
// # Why it runs at boot and not on demand
//
// A company with no person has no way to create one: every /iam route needs a
// credential, and the Tier A token an operator holds is the deployment's
// rather than anybody's. The code is what closes that, and it has to exist
// BEFORE somebody opens the dashboard — a welcome screen that told them to run
// a command to mint a code would be a welcome screen for an operator with a
// shell rather than for the founder.
//
// # And why a node that already has people mints nothing
//
// The file is a superuser claim sitting on a host. An established fleet of a
// hundred nodes must not leave one on every machine, so the mint is gated on
// the estate being genuinely empty — and on `api.auth.bootstrap` being open,
// which is how a deployment whose first person is created by `POST /iam/people`
// under a Tier A token says so.
func openBootstrap(ctx context.Context, boot *config.Bootstrap,
	e *engine.Engine, nodeID string, auth *authapi.Service) error {

	if auth == nil || boot.API.Auth.Bootstrap == config.BootstrapAccessClosed {
		return nil
	}
	reader := e.IAM()
	if reader == nil {
		return nil
	}
	held, err := reader.AnyPerson(ctx)
	if err != nil {
		// A NODE THAT CANNOT READ ITS OWN ESTATE MINTS NOTHING and does
		// not refuse to boot. The two failure directions are not
		// symmetric: a code nobody needed is a live superuser claim on
		// a host, and a code that was not written is one command away.
		logging.Get("cli").Warn("api_bootstrap_not_offered",
			"error", err,
			"detail", "this node could not read its identity estate, so it "+
				"did not mint a founder code; `crewlet iam bootstrap-code` "+
				"mints one once it can")
		return nil
	}
	if held {
		return nil
	}
	path, err := auth.WriteBootstrapCode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("api: mint this node's founder code: %w", err)
	}
	// THE PATH AND NEVER THE VALUE. A log is shipped, aggregated and
	// searched, and a superuser claim in one outlives every rotation.
	logging.Get("cli").Warn("iam_bootstrap_code_ready", "path", path,
		"detail", "this company has nobody in it; the one-time founder code "+
			"is in that file, mode 0600, on this host")
	return nil
}
