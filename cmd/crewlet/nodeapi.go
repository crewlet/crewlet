package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
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
