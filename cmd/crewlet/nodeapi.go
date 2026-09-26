package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
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
// ONE PLACE FOR ALL OF THEM, so the same deployment gets the same answer from
// every command that reaches a node: which address it dials, which token it
// sends, and — when there is no token — one refusal naming the fix, rather
// than one command refusing and another sending nothing and meeting a bare
// 401 from the far end.
//
// A NIL BOOTSTRAP IS A MACHINE WITH NO TIER A: a command naming a node with
// -api from a machine that is not one. Its address is the one named, and the
// only token it can send is the one in [apiTokenEnv].

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
		if boot == nil {
			return "", fmt.Errorf("there is no Tier A config here to find a "+
				"node's address in, so there is no way to reach %s: name the "+
				"node with -api", surface)
		}
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
	if boot != nil && len(boot.API.Auth.Tokens) > 0 {
		return boot.API.Auth.Tokens[0].Token
	}
	return ""
}

// nodeAPIToken is [nodeTokenOrEmpty] for the surfaces that are always guarded.
//
// `/secrets` and `/config` authenticate every request including reads, so
// having no token is not "send none and see" — it is a 401 the operator will
// have to diagnose from the far end. Saying so here names the fix instead.
// The lenient form stays for the routes a node may legitimately serve with
// `api.auth.disabled`.
//
// WITH NO TIER A HERE the refusal names the one place a token can come from:
// a machine that is not a node has no api.auth.tokens to add one to.
func nodeAPIToken(boot *config.Bootstrap, surface string) (string, error) {
	if token := nodeTokenOrEmpty(boot); token != "" {
		return token, nil
	}
	if boot == nil {
		return "", fmt.Errorf(
			"there is no Tier A config here to read an api.auth.tokens entry "+
				"from, so nothing can authenticate to the node's %s surface: "+
				"export %s with a token that node accepts", surface, apiTokenEnv)
	}
	if boot.API.Auth.Disabled {
		return "", nil
	}
	return "", fmt.Errorf(
		"this node lists no api.auth.tokens, so nothing can authenticate to "+
			"its %s surface; add one, or export %s", surface, apiTokenEnv)
}
