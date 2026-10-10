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
// The environment FIRST, then Tier A's first ADMIN key. The order matters: a
// checked-in config carries ${VAR} references that resolve to the same place
// the environment does, and an operator who exported one deliberately means
// that one. The id is stamped as the author of the write, which is why the
// environment variable exists at all.
//
// AN ADMIN KEY, never the first key of either role (ADR-0031). Every surface
// this binary reaches through a node is run by whoever runs the node — the
// configuration, the secrets, backups, retention, the fleet, the budgets —
// and those are an admin's: a member key listed first would be refused 403 by
// each of them, and a write it were allowed would be authored as a teammate
// who never made it. The one gesture made AS a person (`crewlet seats`) takes
// a key linked to a human seat, which an admin key can be.
func nodeTokenOrEmpty(boot *config.Bootstrap) string {
	if fromEnv := strings.TrimSpace(os.Getenv(apiTokenEnv)); fromEnv != "" {
		return fromEnv
	}
	for _, key := range boot.API.Auth.Tokens {
		if key.Role == config.RoleAdmin {
			return key.Token
		}
	}
	return ""
}

// nodeAPIToken is [nodeTokenOrEmpty] for the surfaces that are always guarded.
//
// `/secrets` and `/config` are an admin's, reads included, so having no admin
// key is not "send none and see" — it is a 401 the operator will have to
// diagnose from the far end. Saying so here names the fix instead. The lenient
// form stays for the routes a node may legitimately serve with
// `api.auth.disabled`.
func nodeAPIToken(boot *config.Bootstrap, surface string) (string, error) {
	if token := nodeTokenOrEmpty(boot); token != "" {
		return token, nil
	}
	if boot.API.Auth.Disabled {
		return "", nil
	}
	return "", fmt.Errorf(
		"there is no admin key in api.auth.tokens, so nothing this command "+
			"could send reaches the node's %s surface: give the key of whoever "+
			"runs this deployment role: %s, or export %s",
		surface, config.RoleAdmin, apiTokenEnv)
}

// adminKeySource says where a command's key came from, for a refusal that has
// to name which key to change.
const adminKeySource = "this command sends " + apiTokenEnv + " when it is set, " +
	"else the first api.auth.tokens entry with role: admin"
