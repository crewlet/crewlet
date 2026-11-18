package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/gitlet/mirror"
)

// A GITLET NODE'S IMPORTS AND MIRRORS (internal/gitlet/mirror): a worker of its
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
	objects gitlet.Putter, home *gitlethome.Loop) (*mirror.Mirrors, error) {

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

// mirrorDial is the one outbound dial an import or a mirror makes (M6-D18):
// the source's URL held to the public-address rule unless Tier A
// gitlet.import_allow_private lifts it (gitproc.ResolveRemote), and its
// credential, one whole ${VAR} reference (M6-D17), resolved at the dial from
// the secret store then the environment (lookup), never earlier, never kept.
//
// ONLY A CREDENTIAL code.remote_credentials DECLARES FOR THE URL'S HOST IS
// RESOLVED, under the table this node's epoch holds at the dial (declared:
// Engine.codeRemoteCredentials; config.CodeRemoteCredentials.Refusal judges),
// any other refused before a lookup: a declaration withdrawn or narrowed stops
// the value at the next attempt, whoever wrote the record, so no secret goes
// to a host an administrator did not name for it.
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

// remoteAuthorization is the header a credential's value is sent in: HTTP
// Basic, the one scheme every forge's git endpoint accepts. A value holding a
// colon is `user:password` as written; any other is a token, sent as the
// password beside the user `x-access-token` (GitHub's documented user for a
// token over https, which GitLab, Gitea and Forgejo accept too), so a forge
// wanting a particular user (Bitbucket's `x-token-auth`) gets it in the
// variable itself.
func remoteAuthorization(value string) string {
	if !strings.Contains(value, ":") {
		value = "x-access-token:" + value
	}
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(value))
}
