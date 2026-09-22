package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
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
// The environment FIRST, then Tier A's first token. The order matters: a
// checked-in config carries ${VAR} references that resolve to the same place
// the environment does, and an operator who exported one deliberately means
// that one. Tier A's list is what THIS node accepts, so any entry
// authenticates; the id is stamped as the author of the write, which is why
// the environment variable exists at all.
func nodeTokenOrEmpty(boot *config.Bootstrap) string {
	if fromEnv := strings.TrimSpace(os.Getenv(apiTokenEnv)); fromEnv != "" {
		return fromEnv
	}
	if len(boot.API.Auth.Tokens) > 0 {
		return boot.API.Auth.Tokens[0].Token
	}
	return ""
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
func nodeAPIToken(boot *config.Bootstrap, surface string) (string, error) {
	if token := nodeTokenOrEmpty(boot); token != "" {
		return token, nil
	}
	return "", fmt.Errorf(
		"this node lists no api.auth.tokens, so nothing can authenticate to "+
			"its %s surface; add one, or export %s", surface, apiTokenEnv)
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
	cipher secrets.Cipher) (*authapi.Service, error) {

	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		log := logging.Get("cli")
		log.Info("api_sign_in_absent",
			"reason", "this node runs no identity domain",
			"hint", "node.roles narrows which domains a node applies; a "+
				"seats-only satellite serves no sign-in surface")
		return nil, nil
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
			return nil, nil
		}
		return nil, fmt.Errorf("api: the session signer: %w", err)
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
		return nil, fmt.Errorf("api: the sign-in throttle: %w", err)
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
	})
	if err != nil {
		return nil, fmt.Errorf("api: the sign-in surface: %w", err)
	}
	return surface, nil
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
