package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	codehome "github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/code/mirror"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
)

// A GITLET NODE'S IMPORTS AND MIRRORS (internal/code/mirror): a worker of its
// home's loop on every Gitlet node, filling the repositories it is home to from
// the other hosts their rows name, and pushing their green tips to the hosts
// their push mirrors name.

// buildMirrors builds this node's imports and mirrors and registers them with
// its home loop, beside Gitlet, which they write through: over the router's
// code seam, this node's cache and git pool, the fleet's object store and the
// one outbound dial (mirrorDial), its limits the current epoch's, a push
// mirror's seat judged against the published chart (Engine.codeRoster) and
// its pushes announced on the node's event stream (Engine.codeEvents). The
// attempts it starts run on goroutines of its own, ended by its Stop in
// Gitlet's one bounded stop, after the home's loop that starts them.
func (e *Engine) buildMirrors(nodeID string, boot *config.Bootstrap, c *cache.Cache, pool *gitproc.Pool,
	objects code.Putter, home *codehome.Loop) (*mirror.Mirrors, error) {

	o := mirror.Options{
		Estate: e.router.Code(), Cache: c, Git: pool, Objects: objects, NodeID: nodeID, Limits: e.codeLimits,
		Dial: mirrorDial(boot.Gitlet.ImportAllowPrivate, e.codeRemoteCredentials, e.LookupSecret), Roster: e.codeRoster,
		People: turnRequesters{engine: e}, Events: e.codeEvents(), Metrics: e.metrics,
	}
	if e.mirrorSeams != nil {
		o = e.mirrorSeams(o)
	}
	m, err := mirror.New(o)
	if err != nil {
		return nil, err
	}
	if err := home.Register(m); err != nil {
		return nil, err
	}
	return m, nil
}

// mirrorDial is the one outbound dial an import or a mirror makes
// (M6-D18): the source's URL resolved and held to the public-address rule
// unless Tier A gitlet.import_allow_private lifts it (gitproc.ResolveRemote), and
// its credential — one whole ${VAR} reference, as the record carries it
// (M6-D17) — resolved at the dial from the secret store and then the
// environment (lookup: Engine.LookupSecret), never earlier and never kept.
//
// ONLY A CREDENTIAL code.remote_credentials DECLARES FOR THE URL'S HOST IS
// RESOLVED (declared: the table this node's epoch holds at the dial,
// Engine.codeRemoteCredentials; config.CodeRemoteCredentials.Refusal, the one
// judgement), and any other refused before anything is looked up. The decide
// held the request that recorded it to the table then; this holds every dial
// to the table now — a declaration withdrawn, or narrowed to other hosts,
// stops the value leaving at the next attempt — and to the URL each record
// carries, whoever wrote it, so no value of this node's secret store or its
// environment goes to a host an administrator did not name for it.
//
// THE CREDENTIAL'S VALUE IS SENT AS HTTP BASIC (remoteAuthorization): the one
// scheme every forge's git endpoint accepts. A value holding a colon is
// `user:password` as written; any other is a token, sent as the password
// beside the user `x-access-token` — the user GitHub documents for a token
// over https, and one GitLab, Gitea and Forgejo accept beside a token — so a
// forge that wants a particular user (Bitbucket's `x-token-auth`) is given it
// in the variable itself.
func mirrorDial(allowPrivate bool, declared func() config.CodeRemoteCredentials,
	lookup func(name string) (string, bool)) mirror.Dial {
	return func(ctx context.Context, url, credential string) (gitproc.Remote, error) {
		r, err := gitproc.ResolveRemote(ctx, url, allowPrivate)
		if err != nil || credential == "" {
			return r, err
		}
		name, whole := envref.Whole(credential)
		if !whole {
			return gitproc.Remote{}, fmt.Errorf("engine: the credential of %s is no whole ${VAR} reference", url)
		}
		if why := declared().Refusal(url, credential); why != "" {
			return gitproc.Remote{}, fmt.Errorf("engine: %s is not dialed with its credential: %s", url, why)
		}
		value, ok := lookup(name)
		if !ok || value == "" {
			return gitproc.Remote{}, fmt.Errorf("engine: %s resolves to nothing on this node: set it with "+
				"`crewlet secrets set %s` or in the node's environment", credential, name)
		}
		return r.WithHeader(remoteAuthorization(value))
	}
}

// remoteAuthorization is the header a credential's value is sent in
// ([mirrorDial] says why Basic, and why that user).
func remoteAuthorization(value string) string {
	if !strings.Contains(value, ":") {
		value = "x-access-token:" + value
	}
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(value))
}
