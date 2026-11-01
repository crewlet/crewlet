package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
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

// nodeAPIFlagName is the flag every command that reaches a running node names
// it by, registered only through [nodeAPIFlag].
const nodeAPIFlagName = "api"

// nodeAPIFlag registers -api on fs: the running node a command reaches, which
// [nodeBaseURL] takes as its override. usage says what the command does there
// and what the default is, which is all that differs between them.
//
// ONE SPELLING, REGISTERED IN ONE PLACE. Seven flag sets registered their own,
// and the CLI ended up with two names for one address: `-api` on `iam`,
// `config import`, `secrets`, `llm` and the provisioners (two of them), `-url`
// on every command built on [nodeClientFor]. So an operator retrying an `iam` write through
// another node and a `retention evict` through another node typed different
// flags for the same thing. `-api` is the name kept: more commands took it,
// and it names WHAT is addressed — the node's API, which Tier A's `api` block
// is the default for — rather than the shape of the value, where `-url` would
// sit beside a provisioner's `-public-url` naming something else entirely.
func nodeAPIFlag(fs *flag.FlagSet, usage string) *string {
	return fs.String(nodeAPIFlagName, "", usage)
}

// opIDFlagName is the flag every command whose write can come back unknown
// takes the retry's operation id by, registered only through [opIDFlag].
const opIDFlagName = "op-id"

// opIDFlag registers -op-id on fs: the operation id an unknown answer named,
// sent back with the retry — as the Idempotency-Key header, or as the op_id a
// retention gesture's route reads from its query — so the retry is that
// operation rather than a second one. usage says what a retry of this command
// finishes.
//
// ONE SPELLING, for [nodeAPIFlag]'s reason: `iam` took the same id as
// `-idempotency-key` while `seats`, `worklet purge` and the retention gestures
// took `-op-id`, so one operator retrying two unknown writes typed two flags
// for one thing. `-op-id` is the name kept, because more commands took it and
// it names what goes in it — the op id every one of those answers prints —
// where the other named the HTTP header it travels in.
func opIDFlag(fs *flag.FlagSet, usage string) *string {
	return fs.String(opIDFlagName, "", usage)
}

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
//
// # Whose environment
//
// The INVOCATION's, which [runIn] is handed — the process's own for every run
// of the binary ([run]), so the rule above holds exactly as written. It is a
// parameter rather than a read of the process, because a read of the process
// is shared by everything running in it: a suite that runs its commands side
// by side could only hand each its credential by setting the process's
// variable, which Go refuses to a parallel test, and setting it from each case
// would hand every concurrent command whichever case wrote last.
func nodeTokenOrEmpty(environ config.Source) string {
	token, _ := environ.Lookup(apiTokenEnv)
	return strings.TrimSpace(token)
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
func nodeAPIToken(environ config.Source, surface string) (string, error) {
	if token := nodeTokenOrEmpty(environ); token != "" {
		return token, nil
	}
	return "", fmt.Errorf(
		"no token to reach this node's %s API. Set %s to an API token or an "+
			"access token", surface, apiTokenEnv)
}

// announceUnclaimed says, once at boot, what an operator does next with a
// company nobody is enrolled in yet — see [unclaimedDetail] for what it says.
//
// A LOG LINE AND NOTHING ELSE. There is no founder route and no code to
// write: the Tier A token every serving node already requires is the
// credential a company has before it has anybody, so the first invitation is
// an ordinary one.
func announceUnclaimed(ctx context.Context, anybody func(context.Context) (bool, error),
	seats func() ([]session.Seat, bool), claims seatClaims) {

	if detail, unclaimed := unclaimedDetail(ctx, anybody, seats, claims,
		time.Now()); unclaimed {
		logging.Get("cli").Warn("iam_unclaimed", "detail", detail)
	}
}

// seatClaims is who and what holds the human seats at an instant — every
// binding and every open invitation, the identity reader's own
// [iamdomain.Reader.SeatClaims].
type seatClaims func(context.Context, time.Time) (iamdomain.SeatClaims, error)

// unclaimedDetail is the sentence [announceUnclaimed] logs, and whether the
// company is unclaimed at all.
//
// A PERSON IS ALWAYS INVITED ONTO A HUMAN SEAT (ADR-0032), so what comes next
// depends on the company this node runs: with none, or with no human seat in
// it, there is nowhere to invite anybody, and the step before the invitation
// is the one to name — a company, or a seat in it.
//
// WHERE THERE ARE SEATS, ONLY THE VACANT ONES ARE NAMED. "Nobody is in the
// directory" counts people, and a seat can be held without one: by a SERVICE
// ACCOUNT (a Tier A token's `token:<id>` row bound to it), or by an OPEN
// INVITATION — the founder invited, the node restarted before the link was
// redeemed. An invitation onto either is refused, so the line names what is
// vacant, says an open invitation is the step already taken (its link is the
// next one), and where nothing is vacant says what holds each seat and what
// frees one. The seats are read in the snapshot `GET /iam/seats` reads them
// in; where that read fails the line names no seat at all and points at
// `crewlet iam seats -unheld`, which asks again, rather than listing seats it
// cannot vouch are free.
//
// SILENT ON AN ESTATE THIS NODE CANNOT READ, or has not caught up with, rather
// than telling an operator to invite somebody into a company that may have
// started: at boot a node joining a fleet has usually applied none of its
// identity log yet, and /health answers `unknown` for it.
func unclaimedDetail(ctx context.Context, anybody func(context.Context) (bool, error),
	seats func() ([]session.Seat, bool), claims seatClaims, now time.Time) (string, bool) {

	enrolled, err := anybody(ctx)
	if err != nil || enrolled {
		return "", false
	}
	const nobody = "nobody has been added to this company yet"
	const invite = "`crewlet iam invite <address> -seat <handle> -grants " +
		"admin` (with " + apiTokenEnv + " set to an `api.auth.tokens` " +
		"value), then send them the link it prints"
	human, running := seats()
	switch {
	case !running:
		return nobody + ", and this node has no company yet. Import one " +
			"with a `kind: human` seat for the first person (`crewlet config " +
			"import FILE`, or Org chart › Edit org). Then invite them with " +
			invite, true
	case len(human) == 0:
		return nobody + ", and the company has no human seats. Add a " +
			"`kind: human` seat for the first person, then invite them " +
			"with " + invite, true
	}
	var bySeat map[string]iamdomain.SeatClaim
	read, err := claims(ctx, now)
	if err == nil {
		bySeat, err = iamdomain.ClaimsBySeat(read)
	}
	if err != nil {
		return nobody + ". Invite the first person to a vacant human seat " +
			"(`crewlet iam seats -unheld`) from Company › Org chart, or with " +
			invite, true
	}
	var vacant, invited, bound []string
	for _, seat := range human {
		claim := bySeat[seat.Handle]
		switch {
		case claim.Holder != nil:
			// A SERVICE ACCOUNT, whenever nobody is enrolled: what the
			// directory found absent is every person at every stage.
			who := claim.Holder.Login
			if who == "" {
				who = claim.Holder.Person
			}
			if claim.Holder.Kind == iam.KindMachine {
				who = "service account " + who
			}
			bound = append(bound, seat.Handle+" ("+who+", id "+
				claim.Holder.Person+")")
		case claim.Invitation != nil:
			invited = append(invited, seat.Handle+" (invitation "+
				claim.Invitation.Invitation+")")
		default:
			vacant = append(vacant, seat.Handle)
		}
	}
	var detail string
	switch {
	case len(invited) > 0:
		detail = nobody + ", and an invitation is open for " +
			strings.Join(invited, ", ") + ". If its link is lost, cancel it " +
			"with `crewlet iam cancel-invite ID` and send a new one with " +
			invite
		if len(vacant) > 0 {
			detail += ". Vacant human seats: " + strings.Join(vacant, ", ")
		}
	case len(vacant) > 0:
		detail = nobody + ". Invite the first person to a vacant human " +
			"seat (" + strings.Join(vacant, ", ") + ") from Company › Org " +
			"chart › the seat › Invite, or with " + invite
	default:
		detail = nobody + ", and every human seat is taken (" +
			strings.Join(bound, ", ") + "). Free one with `crewlet iam " +
			"unbind ID`, or add a `kind: human` seat, then invite the first " +
			"person with " + invite
	}
	return detail, true
}
