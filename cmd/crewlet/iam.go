package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet iam` — the company's identity directory from a terminal.
//
// # Why it goes through a running node and never at the database
//
// The directory is a state-log domain: a change is a RECORD, arbitrated at
// the broker on the subject of the thing it changes, and the broker is
// embedded in the engine's own process with no listener. A second process
// cannot publish one. So this speaks to `/iam` on a running node, exactly as
// `crewlet secrets` speaks to `/secrets` and for the same reason.
//
// # And why every write answers three ways
//
// `applied` means this node has the change and the next read here sees it;
// `pending` means the record is durable and this node has not applied it yet,
// and carries the position to read at; `unknown` means nothing can be
// established from here and the only safe retry is the SAME operation id.
// Every command prints which, because a caller that read all three as success
// would tell somebody a revocation landed when nothing can say it did.

const iamUsage = `crewlet iam: manage the company's people, credentials and sessions

Usage:
  crewlet iam people [-q TERM] [-stage S] [-limit N] [-after ID]
                                                       List people
  crewlet iam show ID                                  Show one person
  crewlet iam seats [-unheld]                          List human seats, who holds each,
                                                       and any open invitation
  crewlet iam invite EMAIL -seat SEAT [-grants G,...]
                                                       Invite someone to a vacant human
                                                       seat. The link is shown once
  crewlet iam invitations [-all] [-limit N] [-after ID]
                                                       List open invitations. -all adds
                                                       expired and redeemed ones
  crewlet iam cancel-invite ID                         Cancel an invitation. Its link
                                                       stops working and the seat is
                                                       free again
  crewlet iam reset-password ID|LOGIN                  Issue a password reset link. It's
                                                       shown once and works for a day
  crewlet iam create -email E -seat SEAT [-login L] [-name N] [-grants G,...]
                                                       Create a person on a vacant human
                                                       seat. Their password link is
                                                       shown once
  crewlet iam create -kind machine -login L -grants G,... [-seat SEAT] [-name N]
                                                       Create a service account
  crewlet iam bind ID SEAT                             Move a person to another vacant
                                                       human seat, or bind a service
                                                       account to one
  crewlet iam unbind ID                                Unbind a service account from its
                                                       seat. People always keep one
  crewlet iam grant ID -grants G,...                   Change someone's permissions
  crewlet iam suspend ID                               Suspend someone. Ends their
                                                       sessions, tokens and reset link,
                                                       and keeps their account
  crewlet iam activate ID                              Reactivate a suspended account
  crewlet iam remove ID                                Remove someone and erase their data
  crewlet iam revoke ID                                End every session and token they hold
  crewlet iam sessions ID                              Their sessions, newest first
  crewlet iam credentials [-person ID]                 List someone's credentials
  crewlet iam token -login L [-label L] [-days N] [-grants G,...]
                                                       Create your personal access token, shown once
  crewlet iam token -person ID [-label L] [-days N] [-grants G,...]
                                                       Create a service account's access token, shown once
  crewlet iam revoke-credential CREDENTIAL_ID [-person ID]
                                                       Revoke one credential
  crewlet iam reset-mfa ID                             Reset two-step verification and end sessions
  crewlet iam invalidate-all                           End every session and access token, for everyone
  crewlet iam check                                    Find problems with the company's access
  crewlet iam audit [-person ID] [-event OP] [-since POSITION] [-at TIME]
                    [-before POSITION]                 Show the identity audit trail

Flags:
  -config PATH   The node config file naming the node to reach (default %[1]q)
  -api URL       The running node's API; default is its own api.host:port
  -json          Print the raw answer rather than a table
  -reason TEXT   Saved with the change for the audit trail
  -op-id OP      Retry a write whose outcome was unknown, as the same operation
  -grants G,...  On invite, create, grant and token:
                 %[2]s, comma-separated, or none
                 (overview only). admin includes every app. Defaults:
                 %[3]s for people; for a token, the owner's
                 permissions you may give. Required for a service account
  -seat SEAT     On invite and create: the vacant human seat they'll hold
                 ("iam seats -unheld" lists them). Required for people,
                 optional for service accounts
  -unheld        On seats: only vacant seats (nobody holds them and no
                 invitation is open)

Every write prints its outcome: "applied" (this node has it) or "pending"
(saved, not here yet). If the outcome is unknown, is one this build can't read
(then nothing of the answer is printed), or the node never answered, run the
same command again with -op-id <op id> from the error: every write that takes
-op-id is sent under one made before it asks. Don't start fresh, or it runs
twice. If the node can't confirm it, retry through another node with -api.
A token or a reset link takes no -op-id, since its route reads no key: if its
outcome is unknown or unreadable, its value isn't printed, and running it
again makes a new one.

A person always holds one human seat. They're invited or created onto a
vacant one, "iam bind" moves them, and "iam remove" frees it. "iam unbind" is
only for service accounts, which can hold one seat or none.

"iam create" prints a password link, shown once and good for a week. The
person uses it to set their first password. Retry with -op-id <op id> to see
the same link, or use "iam reset-password" if it stopped working.

Set CREWLET_API_TOKEN to authenticate. Every iam command needs it, reads
included.

A link from "iam reset-password" lets the person set a new password once. It
signs them out everywhere, ends their tokens, and doesn't sign them in. Their
authenticator app still applies. Suspend, revoke, reset-mfa and invalidate-all
all cancel an outstanding link, so issue it after those. People change their own
password in the dashboard (Account), not with this command.

A token from "iam token" acts as its person or service account, with at most
their current permissions. It lasts 90 days unless -days says otherwise (a year
at most). You can use it as CREWLET_API_TOKEN for other commands, but it can't
create another token or change how its owner signs in.

Only you can create your personal access token. "iam token -login" signs you
in, creates the token, and signs out. It asks for your password and code (or
reads them from piped lines), never from flags. Admins use -person for service
accounts only. Changes made with a token are recorded as its owner's.
`

// runIAM dispatches `crewlet iam`.
//
// STDIN IS AN ARGUMENT for the one subcommand that reads it — `token -login`,
// whose password is never a flag — so a suite can pipe one in.
func runIAM(environ config.Source, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprint(stdout, iamHelp())
		return flag.ErrHelp
	}
	// THE SUBJECT COMES OFF BEFORE THE FLAGS, for `crewlet secrets`'
	// reason: `iam show ID -json` is the natural spelling and Go's flag
	// package stops at the first non-flag argument, so parsing first
	// would leave every flag after the id unparsed and silently
	// defaulted.
	subject, rest := splitSubject(rest)
	second, rest := splitSubject(rest)

	fs := flag.NewFlagSet("iam "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	bootstrapPath := fs.String("config", defaultBootstrapPath,
		"node config file: the node this command reaches")
	apiURL := nodeAPIFlag(fs,
		"the running node's API; default is the api.host:port in -config")
	asJSON := fs.Bool("json", false, "print the raw answer")
	reason := fs.String("reason", "", "recorded on the change")
	grants := fs.String("grants", "",
		iamGrantWords()+", comma-separated, or `none`. admin includes "+
			"every app. If left out: every app on invite and a "+
			"person's create, the owner's permissions on token. Required on a "+
			"service account's create")
	stage := fs.String("stage", "", "only people at this stage")
	term := fs.String("q", "", "search by login or seat")
	limit := fs.Int("limit", 0, "how many rows at most")
	login := fs.String("login", "",
		"the login for a new account, or to sign in with when creating your own token")
	email := fs.String("email", "", "the email address of the person to create")
	kind := fs.String("kind", "", "person or machine (create only)")
	name := fs.String("name", "", "the person's own name")
	person := fs.String("person", "",
		"the person whose credentials or sessions to list, or the service account a token is for")
	label := fs.String("label", "", "a name for the new token")
	days := fs.Int("days", 0, "how many days the new token lasts")
	event := fs.String("event", "", "only entries for this kind of change")
	since := fs.Uint64("since", 0, "the oldest log POSITION to include")
	// THE TWO CURSORS A LISTING'S LAST LINE NAMES, which it named while
	// neither was a flag here: following "more: crewlet iam people -after
	// <id>" answered "flag provided but not defined".
	after := fs.String("after", "", "start the next page after this id")
	before := fs.Uint64("before", 0, "end the next page before this POSITION")
	at := fs.String("at", "", "an RFC 3339 instant, resolved to a position once")
	key := opIDFlag(fs,
		"retry a write whose outcome was unknown: the op id its answer named")
	seat := fs.String("seat", "",
		"the vacant human seat to put them on (required on invite "+
			"and for a person's create)")
	all := fs.Bool("all", false,
		"list the expired and redeemed invitations too (invitations only)")
	unheld := fs.Bool("unheld", false,
		"only vacant seats (seats only)")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	// EnvOnly, because Tier A is the root of trust: it holds the keys to
	// the secret store, so it can never read a value out of it.
	// THE SUBJECT IS CHECKED BEFORE THE CONFIG IS READ. An operator who
	// typed `crewlet iam show` needs to be told they left off a person id
	// — not that their config file is missing, which is a different
	// problem they do not have.
	if err := iamSubjects.check(sub, subject, second); err != nil {
		return err
	}
	if strings.TrimSpace(*key) != "" && !iamKeyed[sub] {
		return iamUnkeyed(sub)
	}
	// A KEYED WRITE'S OPERATION IS MINTED HERE, BEFORE IT ASKS, as every
	// keyed write this binary makes mints its own ([operationFor]) — or it
	// is the op id an earlier answer named, held to the node's rule before
	// anything is read or sent. A write whose answer never came may well
	// have landed, and the key it was sent under is the only handle on it:
	// left to the node to mint, it was lost with the answer, so the only
	// retry an operator could run was a fresh operation — a second
	// invitation, or a create the first attempt's person refuses.
	//
	// ONE VALUE NAMES THE WRITE for both halves that finish it — the client,
	// for an answer that never came ([iamClient.unanswered]), and the
	// printer, for a 2xx whose outcome leaves it open ([iamPrinter.settled])
	// — so the retries the two prescribe are one.
	write := keyedWrite{gesture: sub}
	if iamKeyed[sub] {
		var err error
		if write.operation, err = operationFor(sub, *key); err != nil {
			return err
		}
	}
	// -seat IS invite's AND create's — the seat somebody new takes — and
	// refused anywhere else rather than ignored: `iam grant ID -seat lead`
	// read as accepted would say a seat was taken that nothing sent. Somebody
	// who already exists is moved with `bind`.
	if strings.TrimSpace(*seat) != "" && sub != "invite" && sub != "create" {
		return fmt.Errorf("-seat only works with invite and create. To move "+
			"someone, use `crewlet iam bind ID SEAT` instead of `iam %s -seat`", sub)
	}
	// -all IS invitations', refused elsewhere for -seat's reason: `iam
	// people -all` read as accepted would say every stage was listed.
	if *all && sub != "invitations" {
		return fmt.Errorf("-all only works with invitations. `iam %s` doesn't "+
			"take it", sub)
	}
	// -unheld IS seats', for the same reason: `iam people -unheld` read as
	// accepted would say nobody listed holds a seat.
	if *unheld && sub != "seats" {
		return fmt.Errorf("-unheld only works with seats. `iam %s` doesn't "+
			"take it", sub)
	}
	// WHAT SOMEBODY IS GIVEN, read before a config file is read or a
	// request is sent: a name this build does not know is refused naming
	// the set, and -grants anywhere it means nothing is refused rather than
	// ignored, for -seat's reason. See [iamGrants].
	granted, err := iamGrants(*grants)
	if err != nil {
		return err
	}
	if granted.given && !iamGranting[sub] {
		return fmt.Errorf("-grants only works with invite, create, grant and "+
			"token. `iam %s` doesn't take it", sub)
	}
	// WHAT A NEW PERSON OR SERVICE ACCOUNT HAS TO BE GIVEN, told here
	// rather than by the node, before a config file is read or a request
	// is sent — the node refuses each the same way, and an operator who
	// left a flag off needs the sentence naming it rather than a round
	// trip to a 400. See [iamNewcomer].
	if err = iamNewcomer(sub, *kind, *email, *login, *seat, granted.given); err != nil {
		return err
	}
	// AND WHAT A PERSON IS GIVEN WHEN NOTHING SAYS: every app, and never
	// admin. The node requires the field on a create and an invitation —
	// `[]` is a real answer there, the overview alone — so the default is
	// this command's, stated in its help, rather than an absence the node
	// would have to guess at.
	if !granted.given && iamNewPerson(sub, *kind) {
		granted = iamGrantList{grants: slices.Clone(iam.Apps), given: true}
	}
	// A TOKEN HAS AN OWNER, and which kind decides who may mint it: a
	// person mints their own, signed in (-login), and a service account's
	// is minted by an admin (-person). Naming both, or
	// neither, is told so here rather than by the node.
	if sub == "token" {
		owner := strings.TrimSpace(orSubject(*person, subject))
		switch self := strings.TrimSpace(*login); {
		case self != "" && owner != "":
			return errors.New("use -login or -person, not both. -login " +
				"creates your own token, -person creates one for a service " +
				"account")
		case self == "" && owner == "":
			return errors.New("say who the token is for: -login <your login>, " +
				"or -person <service account id>")
		}
	}
	boot, err := config.LoadBootstrap(*bootstrapPath, config.EnvOnly())
	if err != nil {
		return fmt.Errorf("read %s: %w", *bootstrapPath, err)
	}
	ctx := context.Background()
	out := &iamPrinter{w: stdout, raw: *asJSON, write: write,
		what: strings.TrimSpace(sub + " " + subject)}
	if sub == "token" && strings.TrimSpace(*login) != "" {
		// YOUR OWN TOKEN, which only your own session may mint: this
		// signs in for the one request and never reads
		// CREWLET_API_TOKEN. See iamself.go.
		return out.token(mintOwnToken(ctx, boot, *apiURL,
			strings.TrimSpace(*login), iamTokenBody(granted, *label, *days),
			stdin, stderr))
	}
	client, err := newIAMClient(environ, boot, *apiURL)
	if err != nil {
		return err
	}
	client.write, client.keyed = write, iamKeyed[sub]

	switch sub {
	case "people":
		query := url.Values{}
		setIf(query, "q", *term)
		setIf(query, "stage", *stage)
		setIf(query, "after", *after)
		if *limit > 0 {
			query.Set("limit", strconv.Itoa(*limit))
		}
		answer, err := client.get(ctx, "/iam/people", query)
		return out.people(query, answer, err)
	case "show":
		return out.one(client.get(ctx, "/iam/people/"+subject, nil))
	case "invite":
		body := granted.body()
		body["email"] = subject
		body["seat"] = strings.TrimSpace(*seat)
		body["reason"] = *reason
		return out.invite(client.post(ctx, "/iam/invitations", body))
	case "seats":
		query := url.Values{}
		if *unheld {
			query.Set("unheld", "true")
		}
		answer, err := client.get(ctx, "/iam/seats", query)
		// AN EMPTY VACANCY LIST HAS TWO CAUSES with opposite remedies — a
		// company declaring no human seat, and one whose every seat is
		// held or invited — and `?unheld=true` answers `[]` for both, so
		// the whole listing is asked once to tell them apart. Never under
		// -json, which prints the node's answer as it gave it.
		declared := seatsUncounted
		var counting error
		if err == nil && *unheld && !out.raw && len(seatRows(answer)) == 0 {
			every, everyErr := client.get(ctx, "/iam/seats", nil)
			if everyErr != nil {
				counting = everyErr
			} else {
				declared = len(seatRows(every))
			}
		}
		return out.seats(query, answer, declared, counting, err)
	case "invitations":
		query := url.Values{}
		setIf(query, "after", *after)
		if *all {
			query.Set("all", "true")
		}
		if *limit > 0 {
			query.Set("limit", strconv.Itoa(*limit))
		}
		answer, err := client.get(ctx, "/iam/invitations", query)
		return out.invitations(query, answer, err)
	case "cancel-invite":
		return out.written(client.delete(ctx, "/iam/invitations/"+subject,
			withReason(*reason)))
	case "reset-password":
		person, err := client.personID(ctx, subject)
		if err != nil {
			return err
		}
		return out.reset(client.post(ctx,
			"/iam/people/"+person+"/password-reset", nil))
	case "create":
		// A PERSON'S LOGIN MAY BE LEFT OUT: the node proposes one from
		// the address, the proposal an invitation's screen offers its
		// redeemer, and the answer names the login it took.
		body := granted.body()
		body["login"], body["email"] = *login, *email
		body["name"], body["kind"] = *name, *kind
		body["seat"] = strings.TrimSpace(*seat)
		body["reason"] = *reason
		return out.created(client.post(ctx, "/iam/people", body))
	case "bind":
		// TRIMMED, as an invitation's and a create's seat is: a handle
		// typed with a stray space was a seat the company does not have.
		return out.written(client.patch(ctx, "/iam/people/"+subject,
			map[string]any{"seat": strings.TrimSpace(second), "reason": *reason}))
	case "unbind":
		// A SERVICE ACCOUNT'S, and the node is what says so: the rule is
		// the directory's, decided in the record's own snapshot, so a
		// person is refused `seat_required` there and this command
		// renders the remedy ([iamRefusal]) rather than restating the
		// rule over a read of its own.
		return out.written(client.patch(ctx, "/iam/people/"+subject,
			map[string]any{"seat": "", "reason": *reason}))
	case "grant":
		body := granted.body()
		if len(body) == 0 {
			return fmt.Errorf("say what to give with -grants: %s, "+
				"comma-separated, or none", iamGrantWords())
		}
		body["reason"] = *reason
		return out.written(client.patch(ctx, "/iam/people/"+subject, body))
	case "suspend", "activate":
		to := "active"
		if sub == "suspend" {
			to = "suspended"
		}
		return out.written(client.patch(ctx, "/iam/people/"+subject,
			map[string]any{"stage": to, "reason": *reason}))
	case "remove":
		return out.written(client.delete(ctx, "/iam/people/"+subject,
			withReason(*reason)))
	case "revoke":
		return out.written(client.delete(ctx,
			"/iam/people/"+subject+"/sessions", withReason(*reason)))
	case "sessions":
		return out.sessions(client.get(ctx,
			"/iam/people/"+subject+"/sessions", nil))
	case "credentials":
		return out.credentials(client.get(ctx, "/iam/credentials",
			withPerson(orSubject(*person, subject))))
	case "token":
		// A SERVICE ACCOUNT'S, the one kind an administrator mints for.
		// THE OWNER IS A QUERY PARAMETER, never a body field: it is the
		// value the authority table decides on, and a body naming
		// somebody else is a second answer to "whose" the route no
		// longer reads.
		return out.token(client.call(ctx, http.MethodPost, "/iam/credentials",
			withPerson(orSubject(*person, subject)),
			iamTokenBody(granted, *label, *days)))
	case "revoke-credential":
		return out.written(client.delete(ctx, "/iam/credentials/"+subject,
			withPerson(*person)))
	case "reset-mfa":
		return out.written(client.post(ctx,
			"/iam/people/"+subject+"/mfa/reset", nil))
	case "invalidate-all":
		return out.written(client.post(ctx, "/iam/invalidate-all", nil))
	case "check":
		return out.check(client.get(ctx, "/iam/check", nil))
	case "audit":
		query := url.Values{}
		setIf(query, "person", orSubject(*person, subject))
		setIf(query, "event", *event)
		setIf(query, "at", *at)
		if *since > 0 {
			query.Set("since", strconv.FormatUint(*since, 10))
		}
		if *before > 0 {
			query.Set("before", strconv.FormatUint(*before, 10))
		}
		if *limit > 0 {
			query.Set("limit", strconv.Itoa(*limit))
		}
		answer, err := client.get(ctx, "/iam/audit", query)
		return out.audit(query, answer, err)
	}
	fmt.Fprint(stdout, iamHelp())
	return fmt.Errorf("unknown subcommand %q", sub)
}

// iamSubjects is what each subcommand has to be told, in one table.
//
// A TABLE RATHER THAN A CHECK PER ARM, because it runs BEFORE the config is
// read: an operator who typed `crewlet iam show` needs to be told they left
// off a person id, and a check inside the dispatch would have reported a
// missing config file first — a different problem they do not have.
//
// A subcommand with no row needs no subject, which is the safe direction: the
// far end refuses a request that names nothing, and a table that guessed
// would refuse commands that are perfectly complete.
var iamSubjects = subjectTable{
	"show":              {"a person id"},
	"invite":            {"an address"},
	"bind":              {"a person id", "a seat handle"},
	"unbind":            {"a person id"},
	"grant":             {"a person id"},
	"suspend":           {"a person id"},
	"activate":          {"a person id"},
	"remove":            {"a person id"},
	"revoke":            {"a person id"},
	"sessions":          {"a person id"},
	"reset-mfa":         {"a person id"},
	"revoke-credential": {"a credential id"},
	"cancel-invite":     {"an invitation id"},
	"reset-password":    {"a person id or login"},
}

// iamNewcomer is what `invite` and `create` must be told before anything is
// sent: the seat a person will hold, the address that finds them, and the
// login a service account is found by.
//
// A PERSON HOLDS A HUMAN SEAT for as long as they are here (ADR-0032), so an
// invitation and a person's create each name one; a SERVICE ACCOUNT's is
// optional, bound only to act as it. A person's login may be left out — the
// node proposes it from their address — and a service account's may not,
// since it has no address to propose from.
//
// A SERVICE ACCOUNT IS ALSO TOLD WHAT IT CARRIES, every time: a person left
// without -grants is given every app ([iamNewPerson]), the colleague's
// default, but nobody is behind a service account to have meant one — what a
// pipeline may do is a decision, and `-grants none` is how to decide nothing.
//
// THE KIND IS THE FLAG'S, never guessed from the login's shape: the node reads
// a blank kind as a person, and so does this. A kind this command does not
// name is the node's to refuse, in its own words.
func iamNewcomer(sub, kind, email, login, seat string, granted bool) error {
	seatless := strings.TrimSpace(seat) == ""
	switch sub {
	case "invite":
		if seatless {
			return errors.New("name the seat with -seat <handle>. It must be a " +
				"vacant human seat (`crewlet iam seats -unheld` lists them)")
		}
	case "create":
		switch iam.Kind(strings.TrimSpace(kind)) {
		case iam.KindMachine:
			if strings.TrimSpace(login) == "" {
				return errors.New("name the login with -login, e.g. ci:release " +
					"(or token:<id> for an API token)")
			}
			if !granted {
				return fmt.Errorf("name what the service account gets with "+
					"-grants: %s (comma-separated), or none", iamGrantWords())
			}
		case "", iam.KindPerson:
			if strings.TrimSpace(email) == "" {
				return errors.New("name the email with -email jane@example.com. " +
					"Their login is suggested from it unless you pass -login")
			}
			if seatless {
				return errors.New("name a vacant human seat with -seat <handle> " +
					"(`crewlet iam seats -unheld` lists them). Only service " +
					"accounts can skip it")
			}
		}
	}
	return nil
}

// iamKeyed is every subcommand whose route reads an Idempotency-Key: each a
// write, and each one whose `unknown` answer prescribes the SAME operation
// again — which a command that could not send the key could never perform, so
// the retry an operator could actually run was a fresh operation.
//
// TWO WRITES ARE MISSING, deliberately, and [iamUnkeyed] says why to whoever
// asks: a mint and a reset link each answer a value only their first attempt
// could show. A CREATE IS NOT ONE OF THEM although a person's answers a link
// too: its first password link is DERIVED from the operation under the
// company's key, so its retry hands back the link its first attempt issued —
// which a reset link's random secret could never be.
var iamKeyed = map[string]bool{
	"invite": true, "create": true, "bind": true, "unbind": true,
	"grant": true, "suspend": true, "activate": true, "remove": true,
	"revoke": true, "revoke-credential": true, "reset-mfa": true,
	"invalidate-all": true, "cancel-invite": true,
}

// iamUnkeyed refuses -op-id on a subcommand whose route reads none,
// saying what to do instead: sent anyway, the key would be ignored and the
// operator told nothing, which is the one outcome worse than the refusal.
//
// It is also how a mint or a reset link whose answer leaves it open ends
// ([iamPrinter.settled]): why no key can retry one is why it is run again.
func iamUnkeyed(sub string) error {
	switch sub {
	case "token":
		return errors.New("token doesn't take -op-id. Just run it " +
			"again")
	case "reset-password":
		return errors.New("reset-password doesn't take -op-id. Run " +
			"it again, and the new link replaces the old one")
	}
	return fmt.Errorf("-op-id is for retrying writes, and `iam %s` "+
		"doesn't write anything", sub)
}

// subjectTable maps a subcommand to what its positional arguments are.
type subjectTable map[string][]string

func (t subjectTable) check(sub string, given ...string) error {
	for i, what := range t[sub] {
		if i >= len(given) || strings.TrimSpace(given[i]) == "" {
			return fmt.Errorf("name %s", what)
		}
	}
	return nil
}

// orSubject prefers the flag and falls back to the positional argument, so
// `iam credentials alice` and `iam credentials -person alice` are one command.
func orSubject(flagged, positional string) string {
	if strings.TrimSpace(flagged) != "" {
		return flagged
	}
	return positional
}

func setIf(q url.Values, key, value string) {
	if strings.TrimSpace(value) != "" {
		q.Set(key, value)
	}
}

func withReason(reason string) url.Values {
	q := url.Values{}
	setIf(q, "reason", reason)
	return q
}

func withPerson(person string) url.Values {
	q := url.Values{}
	setIf(q, "person", person)
	return q
}

// iamGrantList is the -grants flag read: the permissions it names, and whether
// it named any at all.
//
// `none` IS A VALUE AND AN OMITTED FLAG IS NOT. Stripping somebody's last
// permission and not mentioning permissions at all are opposite intentions,
// and an empty string cannot carry both — so the word is explicit, and it is
// the one spelling that could never be a permission. `given` is what tells
// them apart once the word has been read: an empty list given is the overview
// alone, and an empty list not given is no `grants` field at all.
type iamGrantList struct {
	grants []iam.Grant
	given  bool
}

// body is the field the list sends: `grants` when it was given — an empty
// array for `none` — and nothing when it was not.
func (l iamGrantList) body() map[string]any {
	body := map[string]any{}
	if l.given {
		out := make([]string, 0, len(l.grants))
		for _, g := range l.grants {
			out = append(out, string(g))
		}
		body["grants"] = out
	}
	return body
}

// iamGranting is every subcommand that sends what somebody carries: an
// invitation's and a create's whole set, a grant's replacement for it, and a
// mint's choice of what the token carries.
var iamGranting = map[string]bool{
	"invite": true, "create": true, "grant": true, "token": true,
}

// iamNewPerson reports whether sub enrols a PERSON — an invitation, which is
// always one, or a create whose kind is not a machine — the one newcomer the
// command gives every app when -grants is left out.
func iamNewPerson(sub, kind string) bool {
	switch sub {
	case "invite":
		return true
	case "create":
		return iam.Kind(strings.TrimSpace(kind)) != iam.KindMachine
	}
	return false
}

// iamGrants reads the -grants flag.
//
// THE VOCABULARY IS THE ENGINE'S OWN ([iam.AllGrants]: admin, worklet,
// doclet, artifacts), comma-separated, or the one word `none`, and anything
// else is refused here, naming the set, before a config file is read or a
// request is sent. The node refuses a name it does not know too, but an
// operator who typed `worklt` needs the sentence listing the four words rather
// than a round trip to a 400, and `none` beside a permission is a contradiction nothing
// should guess the meaning of.
//
// `admin` STORES EVERY APP BESIDE IT ([iam.Expand]), as the dashboard's picker
// does: admin includes every app whether or not the row says so, but a row
// that says so keeps them when admin is later taken away: an administrator
// stepping down goes on using Worklet. The list is sent in
// [iam.AllGrants] order with each name once, whatever order it was typed in.
func iamGrants(flagged string) (iamGrantList, error) {
	flagged = strings.TrimSpace(flagged)
	switch flagged {
	case "":
		return iamGrantList{}, nil
	case "none":
		return iamGrantList{grants: []iam.Grant{}, given: true}, nil
	}
	var named []iam.Grant
	for part := range strings.SplitSeq(flagged, ",") {
		g := iam.Grant(strings.TrimSpace(part))
		switch {
		case g == "":
			continue
		case g == "none":
			return iamGrantList{}, errors.New("-grants none can't be " +
				"combined with other permissions. Pass none on its own, or " +
				"list the permissions")
		case !g.Valid():
			return iamGrantList{}, fmt.Errorf("-grants: %q isn't a permission. "+
				"Use %s, comma-separated, or none", g, iamGrantWords())
		}
		named = append(named, g)
	}
	if len(named) == 0 {
		return iamGrantList{}, fmt.Errorf("-grants is empty. Use %s, "+
			"comma-separated, or none", iamGrantWords())
	}
	expanded := iam.Expand(named)
	out := make([]iam.Grant, 0, len(iam.AllGrants))
	for _, g := range iam.AllGrants {
		if slices.Contains(expanded, g) {
			out = append(out, g)
		}
	}
	return iamGrantList{grants: out, given: true}, nil
}

// iamGrantWords is the vocabulary the help and every refusal name, read off
// [iam.AllGrants] so a permission a later build adds is named here without
// anybody remembering these sentences exist. EVERY sentence this command
// prints that lists the permissions takes it — the usage, the flag's own help
// and each refusal — because a list typed into one of them is the copy that
// goes on naming three permissions after there are four;
// TestTheCLINeverSpellsTheGrantListByHand reads the source for one.
func iamGrantWords() string { return iamWords(iam.AllGrants) }

// iamAppWords is the APPS among them ([iam.Apps]), which is what a person's
// invitation or create carries when -grants is left out — read off the same
// list, for the same reason.
func iamAppWords() string { return iamWords(iam.Apps) }

func iamWords(set []iam.Grant) string {
	words := make([]string, 0, len(set))
	for _, g := range set {
		words = append(words, string(g))
	}
	return strings.Join(words, ", ")
}

// iamHelp is [iamUsage] with its blanks filled: the default config path, and
// the permission and app vocabularies, which are the engine's own lists rather
// than words typed into the text.
func iamHelp() string {
	return fmt.Sprintf(iamUsage, defaultBootstrapPath, iamGrantWords(), iamAppWords())
}

// iamTokenBody is a mint's request: what the token carries, what it is called
// and how long it lasts — the same whoever the owner is. A list not given
// sends no `grants`, which the node reads as everything the owner holds that
// the minting session may confer.
func iamTokenBody(granted iamGrantList, label string, days int) map[string]any {
	body := granted.body()
	body["label"] = label
	if days > 0 {
		body["expires_in_days"] = days
	}
	return body
}

// --- the client ---------------------------------------------------------- //

// iamClient talks to a running node's /iam surface.
type iamClient struct {
	base  string
	token string
	http  *http.Client

	// write is the command's write: its gesture, which names it in a
	// sentence ("invite", "grant"), and the operation it is sent under —
	// minted before it asks, or the op id an earlier answer named
	// ([operationFor]) — sent as the Idempotency-Key on every write this
	// command makes, and never on a read. The operation is empty on a
	// command whose route reads none.
	write keyedWrite

	// keyed is whether the route this command writes to reads a key at all
	// ([iamKeyed]). Only where it does is a refusal's retry spelled as
	// -op-id: a mint reads none, and was told to retry with a
	// flag this command refuses on it.
	keyed bool
}

func newIAMClient(environ config.Source, boot *config.Bootstrap, override string) (*iamClient, error) {
	base, err := nodeBaseURL(boot, override, "the company's identity directory")
	if err != nil {
		return nil, err
	}
	token, err := nodeAPIToken(environ, "/iam")
	if err != nil {
		return nil, err
	}
	return &iamClient{base: base, token: token,
		http: httpx.Client(apiTimeout)}, nil
}

func (c *iamClient) get(ctx context.Context, path string, query url.Values) (
	map[string]any, error) {

	return c.call(ctx, http.MethodGet, path, query, nil)
}

func (c *iamClient) post(ctx context.Context, path string, body map[string]any) (
	map[string]any, error) {

	return c.call(ctx, http.MethodPost, path, nil, body)
}

func (c *iamClient) patch(ctx context.Context, path string, body map[string]any) (
	map[string]any, error) {

	return c.call(ctx, http.MethodPatch, path, nil, body)
}

func (c *iamClient) delete(ctx context.Context, path string, query url.Values) (
	map[string]any, error) {

	return c.call(ctx, http.MethodDelete, path, query, nil)
}

// call is the one round trip, and the one place a refusal becomes a sentence.
func (c *iamClient) call(ctx context.Context, method, path string,
	query url.Values, body map[string]any) (map[string]any, error) {

	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.write.operation != "" && method != http.MethodGet {
		req.Header.Set(opkey.Header, c.write.operation)
	}
	// NO ORIGIN, deliberately. The node's cross-site check admits a
	// BEARER with none — a browser cannot make one travel — and refuses a
	// PRESENT Origin that is not an address the deployment is reached at.
	// This client used to send its own base URL, which is the loopback
	// address it dials and almost never `api.external_url`, so every write
	// it made to a deployment behind a proxy was refused as cross-site.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.unanswered(method, fmt.Errorf("reach %s: %w", target, err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, iamMaxAnswer))
	if err != nil {
		return nil, c.unanswered(method,
			fmt.Errorf("read the answer from %s: %w", target, err))
	}
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	if resp.StatusCode >= 400 {
		refusal := iamRefusal(resp.StatusCode, resp.Header.Get("Content-Type"),
			answer, raw, c.keyed)
		if engineCode(raw) == "" {
			// NOT THE NODE'S OWN ANSWER — no engine code, so something in
			// front of it answered, most often a proxy's read timeout — and
			// the node may still have done what was asked ([notTheNode]).
			return answer, c.unanswered(method, refusal)
		}
		return answer, refusal
	}
	return answer, nil
}

// unanswered is err for a request whose outcome the node never told this
// command: for a keyed write, with the one safe retry — the same command under
// the operation it was sent with — because the write may have landed after the
// answer was lost, and unchanged for a read or a write whose route reads no
// key.
func (c *iamClient) unanswered(method string, err error) error {
	if !c.keyed || c.write.operation == "" || method == http.MethodGet {
		return err
	}
	return fmt.Errorf("%w\n\nThe node did not answer, so whether the %s landed is "+
		"unknown: %s", err, c.write.gesture, c.write.retry())
}

// personID is the person a subject names: an id as it is, and a login resolved
// through the directory listing to the ONE row holding it exactly — the
// listing's `q` narrows on a substring, so an exact match is what is taken and
// anything else is refused rather than guessed at.
//
// EVERY PAGE OF THE LISTING IS READ, following its own `next`: `q` matches a
// login or a seat anywhere in it, so in a company where more people than a
// page hold `sam` somewhere the one holding exactly `sam` can be on a later
// page, and reading the first alone answered that nobody holds it.
func (c *iamClient) personID(ctx context.Context, subject string) (string, error) {
	if _, err := uuid.Parse(subject); err == nil {
		return subject, nil
	}
	var held []string
	for after := ""; ; {
		query := url.Values{"q": {subject},
			"limit": {strconv.Itoa(iamdomain.MaxPageSize)}}
		if after != "" {
			query.Set("after", after)
		}
		answer, err := c.get(ctx, "/iam/people", query)
		if err != nil {
			return "", err
		}
		rows, _ := answer["people"].([]any)
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			if str(row["login"]) == subject {
				held = append(held, str(row["id"]))
			}
		}
		if after = str(answer["next"]); after == "" {
			break
		}
	}
	switch len(held) {
	case 1:
		return held[0], nil
	case 0:
		return "", fmt.Errorf("nobody has the login %q. Use their id instead "+
			"(`crewlet iam people` lists them)", subject)
	}
	return "", fmt.Errorf("%d people have the login %q. Use the id of the one "+
		"you mean (%s)", len(held),
		subject, strings.Join(held, ", "))
}

// iamMaxAnswer bounds one answer this command will read.
//
// 8 MiB, which is two hundred directory rows with every field filled and an
// order of magnitude of headroom. It exists so a node answering something
// unexpected cannot make the CLI hold a company's whole log in memory.
const iamMaxAnswer = 8 << 20

// iamRefusal turns a refusal into the sentence an operator acts on.
//
// THE CODE AND THE DETAIL, because the engine's refusals carry both and the
// detail is the half that names the seat, the grant or the holder. A status
// alone would make `409` indistinguishable from `409`.
//
// AND WHAT THE ANSWER SAYS ABOUT THE OPERATION: the changes an edit stopped
// partway had already made (`landed`), and — on a 503 that carries an op id —
// the retry that is safe, spelled as THIS command's flag. The node's own
// sentence says to send the id back as a header, which is right for a client
// holding the request and useless to an operator holding a terminal; the id
// used to be dropped here altogether, so the retry the answer prescribed was
// one nobody at the terminal could make. And where the answer says this node
// cannot vouch for the operation (`unvouched`), the retry goes through another
// node: asked here again it answers the same way until the change reaches it.
//
// # An unknown outcome is not a refusal
//
// A write the node could not account for is a 503 too, and the answer says so
// as a field (`outcome: "unknown"`), which is what this reads — never the
// sentence beside it. Rendered as `unavailable: …` it read as a node that did
// nothing, of a write that may have landed.
//
// keyed is whether the command's route reads a key. One that does not — a
// mint — is retried as a new write, which the node's own sentence says, so
// the op id is named for the trail and no -op-id is prescribed.
//
// contentType is the answer's, for a body that carries no engine code: what
// stood in front of the node is read through [foreignAnswer], as the other
// three clients read it, rather than printed whole.
func iamRefusal(status int, contentType string, answer map[string]any, raw []byte,
	keyed bool) error {

	if msg, ok := credentialRefusal(status, raw, true); ok {
		return errors.New(msg)
	}
	code, _ := answer["error"].(string)
	detail := answerWords(answer).said()
	var said string
	switch {
	case code != "" && detail != "":
		said = code + ": " + detail
	case code != "":
		said = code
	default:
		// NOT THE ENGINE'S ANSWER — no code of its own — so what it says is
		// read the way every other client here reads a proxy's page: its
		// title or its text, bounded, never the markup dumped whole.
		said = fmt.Sprintf("the node answered %d: %s", status,
			foreignAnswer(contentType, raw))
	}
	if outcome, _ := answer["outcome"].(string); outcome == string(statelog.OutcomeUnknown) {
		said = "the node couldn't confirm whether this change was saved: " + said
	}
	if landed := joinAny(answer["landed"]); landed != "" {
		said += "\nthese changes were saved before it: " + landed
	}
	if hint := str(answer["hint"]); hint != "" {
		said += "\n" + hint
	}
	said += iamRemedy(code, answer)
	if op := str(answer["op_id"]); op != "" {
		switch unvouched, _ := answer["unvouched"].(bool); {
		case status != http.StatusServiceUnavailable, !keyed:
			said += "\nop " + op
		case unvouched:
			// NOT THROUGH THIS NODE: its operation ledger cannot vouch
			// for the operation, so it published nothing and answers the
			// same way until the change reaches it — the retry this used
			// to prescribe, here again, was a loop.
			said += "\nthis node can't confirm it yet. Check whether it saved, " +
				"or retry on another node with -api <that node> " +
				"-op-id " + op + ". Don't use a new id"
		default:
			// THE SAME OPERATION, whichever 503 this is: an outcome
			// nobody could establish is answered from whatever of it
			// landed, and a refusal that wrote nothing runs under the
			// same key as it would have under a fresh one.
			said += "\nretry with -op-id " + op + ". A new id " +
				"would make it a second change"
		}
	}
	return errors.New(said)
}

// iamRemedy is the command that clears a refusal, spelled for this CLI, where
// the answer says which — keyed on the ANSWER and never on the subcommand,
// which says what was asked rather than what refused it. The node's own
// sentence names a route, which an operator at a terminal does not hold.
//
//   - AN OPEN INVITATION HOLDS what was asked for — an address or a seat,
//     named on the 409 as `invitation` — and cancelling it frees both.
//   - A PERSON'S SEAT LEFT OUT, `seat_required`, is one of two gestures, told
//     apart by whether the answer names somebody who exists (`id`): a person
//     being unbound, whom a move or a removal frees the seat from — `unbind`
//     is a service account's — or an invitation or a create that named no
//     seat.
func iamRemedy(code string, answer map[string]any) string {
	var out string
	if inv := str(answer["invitation"]); inv != "" {
		out += "\nan open invitation holds it (" + inv + "). Cancel it " +
			"with `crewlet iam cancel-invite " + inv + "`"
	}
	if code == string(httpjson.CodeSeatRequired) {
		if id := str(answer["id"]); id != "" {
			out += "\npeople always hold a seat. Move them with `crewlet iam " +
				"bind " + id + " SEAT`, or remove them with `crewlet iam " +
				"remove " + id + "`"
		} else {
			out += "\nname the seat with -seat <handle>. `crewlet iam seats " +
				"-unheld` lists the vacant ones"
		}
	}
	return out
}

// --- printing ------------------------------------------------------------ //

// iamPrinter renders one answer, as a table or as the raw JSON.
type iamPrinter struct {
	w   io.Writer
	raw bool

	// write is the command's own write, which a write's answer is judged
	// against before anything of it is printed ([iamPrinter.settled]) — its
	// operation empty for a write whose route reads no key, a mint and a
	// reset link — and what names it in a sentence: "suspend p-1",
	// "invalidate-all".
	write keyedWrite
	what  string
}

// settled is nil for a write's 2xx answer whose outcome says where the write
// stands — applied, or pending — and for any other the error that finishes it.
//
// A 2xx CAN LEAVE THE OUTCOME OPEN. The node answers its own `unknown` 503,
// which [iamRefusal] reads; but a node newer than this command may answer, 2xx,
// an outcome this build cannot read, and that was printed as `outcome "x"` and
// exited zero — so a script read a write nobody here could vouch for as one
// that landed. It FAILS, as every keyed write's open outcome does
// ([keyedWrite.confirmed]), naming the retry under the operation the command
// sent.
//
// A MINT OR A RESET LINK IS RUN AGAIN INSTEAD, as the node's own unknown for
// either says: their routes read no key, so there is no operation to retry
// under, and this command refuses -op-id on both ([iamUnkeyed] says why). The
// op id the answer names is said for the trail.
//
// BEFORE ANYTHING OF THE ANSWER IS PRINTED, under -json too: the node hands
// out no invitation, first password link, reset link or token for a write
// nobody can confirm — a link would answer 410 the first time somebody
// followed it — so neither does this, and the retry under -op-id hands back
// the very link an invitation's or a create's first attempt issued. A document
// printed beside a failure is one a script reads before it reads the exit
// status.
func (p *iamPrinter) settled(answer map[string]any) error {
	outcome := str(answer["outcome"])
	if p.write.operation != "" {
		return p.write.confirmed(p.what, outcome)
	}
	said := unconfirmed(outcome)
	if said == "" {
		return nil
	}
	return fmt.Errorf("%s: %s — the node could not confirm the write, so what it "+
		"answered is not shown (op %s): %w", p.what, said, dash(str(answer["op_id"])),
		iamUnkeyed(p.write.gesture))
}

func (p *iamPrinter) dump(answer map[string]any) error {
	encoded, err := json.MarshalIndent(answer, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(p.w, string(encoded))
	return nil
}

func (p *iamPrinter) people(asked url.Values, answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["people"].([]any)
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLOGIN\tNAME\tSEAT\tSTAGE\tGRANTS")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			str(row["id"]), dash(str(row["login"])), dash(personName(row)),
			dash(str(row["seat"])), str(row["stage"]),
			dash(joinAny(row["grants"])))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if next := str(answer["next"]); next != "" {
		p.more("people", asked, "after", next)
	}
	fmt.Fprintf(p.w, "as of %s\n", str(answer["position"]))
	return nil
}

// more prints a listing's last line: the command that answers its next page —
// this one AS IT WAS ASKED, every filter it narrowed on included, with its
// cursor moved to where the page ended.
//
// THE FILTERS RIDE ALONG because the cursor alone is a different listing: a
// line naming only `-after <id>` paged through the WHOLE directory from that
// row, so following `iam people -stage invited` printed everybody after it as
// though they matched. Every query parameter a listing sends is named after
// the flag that set it, which is what lets the line be composed from the query
// rather than from a second list of flags; the bool flags are [iamBoolParams].
func (p *iamPrinter) more(sub string, asked url.Values, cursor, next string) {
	again := url.Values{}
	for name, values := range asked {
		again[name] = slices.Clone(values)
	}
	again.Set(cursor, next)
	line := "crewlet iam " + sub
	for _, name := range slices.Sorted(maps.Keys(again)) {
		if iamBoolParams[name] {
			line += " -" + name
			continue
		}
		line += " -" + name + " " + shellWord(again.Get(name))
	}
	fmt.Fprintf(p.w, "\nmore: %s\n", line)
}

// iamBoolParams are the query parameters a bool flag sets, which a next-page
// line names bare: `-all true` would be read as `-all` and a stray argument.
var iamBoolParams = map[string]bool{"all": true}

// shellWord quotes a value for a line an operator pastes into a shell, and
// leaves one with nothing a shell would read specially as it is.
func shellWord(value string) string {
	special := func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		}
		return !strings.ContainsRune("-_.:@+/=,%", r)
	}
	if value != "" && strings.IndexFunc(value, special) < 0 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// personName is what to show where a name would go.
//
// THE TWO STATES ARE TWO SENTENCES. A sealed one is ciphertext this node's
// keyring cannot open — a key dropped before the values were re-sealed, or a
// restore under a different keyring — which the right keyring can; an empty
// one is somebody who gave none. Rendering both as blank would make a restore
// under the wrong keyring look like a company of anonymous people.
func personName(row map[string]any) string {
	if truthy(row["sealed"]) {
		return "(can't be read: this node's keyring is missing its key)"
	}
	return str(row["name"])
}

func (p *iamPrinter) one(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	for _, field := range []struct{ label, value string }{
		{"id", str(answer["id"])},
		{"kind", str(answer["kind"])},
		{"stage", str(answer["stage"])},
		{"login", dash(str(answer["login"]))},
		{"name", dash(personName(answer))},
		{"email", dash(str(answer["email"]))},
		{"seat", dash(str(answer["seat"]))},
		{"grants", dash(joinAny(answer["grants"]))},
		{"sign-out count", str(answer["revocation_epoch"])},
		{"version", str(answer["version"])},
	} {
		fmt.Fprintf(tw, "%s\t%s\n", field.label, field.value)
	}
	return tw.Flush()
}

func (p *iamPrinter) sessions(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["sessions"].([]any)
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LINEAGE\tSTARTED\tEXPIRES\tLIVE\tENDED\tREASON")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\t%s\n",
			str(row["lineage"]), stamp(row["created_at"]),
			stamp(row["expires_at"]), truthy(row["live"]),
			dash(stamp(row["ended_at"])), dash(str(row["ended_reason"])))
	}
	return tw.Flush()
}

func (p *iamPrinter) credentials(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["credentials"].([]any)
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMETHOD\tLABEL\tCREATED\tEXPIRES\tREVOKED")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%v\n",
			str(row["id"]), str(row["method"]), dash(str(row["label"])),
			stamp(row["created_at"]), dash(stamp(row["expires_at"])),
			truthy(row["revoked"]))
	}
	return tw.Flush()
}

func (p *iamPrinter) check(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["findings"].([]any)
	// A BINDING THE NODE COULD NOT JUDGE IS SAID FIRST, because it
	// qualifies everything below it: "nothing to report" from a node that
	// runs no company yet is not a directory with no dangling binding, it
	// is one nobody could check.
	if unchecked, _ := answer["bindings_unchecked"].(float64); unchecked > 0 {
		fmt.Fprintf(p.w, "%d seat binding(s) couldn't be checked. This "+
			"node hasn't loaded the company yet, so try another node.\n",
			int(unchecked))
	}
	if len(rows) == 0 {
		fmt.Fprintf(p.w, "no problems found, as of %s\n",
			str(answer["position"]))
		return nil
	}
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FINDING\tWHO\tDETAIL")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			str(row["kind"]), dash(findingWho(row)), str(row["detail"]))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(p.w, "\nas of %s\n", str(answer["position"]))
	return nil
}

// findingWho is the WHO column for one finding: the person it is about, by
// their login where they have one.
func findingWho(row map[string]any) string {
	if who := str(row["login"]); who != "" {
		return who
	}
	return str(row["person"])
}

func (p *iamPrinter) audit(asked url.Values, answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["events"].([]any)
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POSITION\tAT\tOP\tACTOR\tPERSON\tSUMMARY")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		// THE CREDENTIAL BESIDE THE ACTOR, in the words every other
		// trail this CLI prints uses: a token acts as its owner, so the
		// actor alone reads a token's gesture as the owner's own.
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			str(row["position"]), stamp(row["at"]), str(row["op"]),
			dash(describeAuthor(str(row["actor"]), str(row["operator_id"]))),
			dash(str(row["person"])), dash(str(row["summary"])))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if next := str(answer["next"]); next != "" && next != "0" {
		p.more("audit", asked, "before", next)
	}
	return nil
}

// invite prints the link ONCE, which is the only time it exists — and none for
// an invitation nobody can confirm ([iamPrinter.settled]).
//
// AND WHERE THE ISSUE STANDS, as every write here says ([iamPrinter.outcome]):
// it printed the link alone, so an invitation this node had made durable and
// NOT applied — a 202, whose link opens only where the record has been applied
// — read exactly like one the next read here shows. The link is printed on
// `pending` too, for a create's reason: the record is durable, and the link
// opens on every node that has applied it.
func (p *iamPrinter) invite(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if err := p.settled(answer); err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	fmt.Fprintf(p.w, "invitation %s\n", str(answer["id"]))
	p.outcome(answer)
	fmt.Fprintf(p.w, "%s\n\n", str(answer["url"]))
	fmt.Fprintf(p.w, "expires %s\n", stamp(answer["expires_at"]))
	fmt.Fprintln(p.w, "This link is shown once. Send it to them "+
		"yourself; Crewlet doesn't send email.")
	return nil
}

// invitations prints the invitations a listing answered — never a link, which
// the answer does not carry.
func (p *iamPrinter) invitations(asked url.Values, answer map[string]any,
	err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["invitations"].([]any)
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tEMAIL\tSEAT\tGRANTS\tINVITED BY\tEXPIRES")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		email := str(row["email"])
		if truthy(row["sealed"]) {
			email = "(can't be read: this node's keyring is missing its key)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			str(row["id"]), str(row["state"]), dash(email),
			dash(str(row["seat"])), dash(joinAny(row["grants"])),
			dash(str(row["invited_by"])), dash(stamp(row["expires_at"])))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if next := str(answer["next"]); next != "" {
		p.more("invitations", asked, "after", next)
	}
	fmt.Fprintf(p.w, "as of %s\n", str(answer["position"]))
	return nil
}

// seatsUncounted is the count [iamPrinter.seats] is handed where nobody asked
// how many human seats the company declares — every listing but an empty
// `-unheld` one.
const seatsUncounted = -1

// seatRows is the rows a seat listing answered.
func seatRows(answer map[string]any) []any {
	rows, _ := answer["seats"].([]any)
	return rows
}

// seats prints the human seats and what holds each — a holder, an open
// invitation, or neither — and never a link, which the answer does not carry.
//
// AN EMPTY LISTING IS SAID, in the words of what makes it empty: a company
// declaring no human seat can admit nobody until it declares one, while a
// company whose every seat is held or invited needs a seat added or freed —
// and a bare header would leave an operator about to invite somebody unable
// to tell the two apart. An empty `-unheld` listing is one of either, so it is
// told apart by `declared`, the number of human seats the whole listing held
// (`counting` is why it could not be read). Its remedy names the commands that
// FREE a seat — a removal for a person, an unbind for a service account, a
// cancellation for an invitation — and not `bind`, which MOVES somebody onto
// a vacant seat and so needs the very vacancy the listing says is not there.
func (p *iamPrinter) seats(asked url.Values, answer map[string]any,
	declared int, counting, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows := seatRows(answer)
	if len(rows) == 0 {
		unheld := asked.Get("unheld") == "true"
		switch {
		case unheld && counting != nil:
			fmt.Fprintf(p.w, "no vacant human seats found, but the node "+
				"couldn't list every seat (%v). Run `crewlet iam seats` to "+
				"check\n", counting)
			return nil
		case unheld && declared > 0:
			seats := "seat is"
			if declared != 1 {
				seats = "seats are"
			}
			fmt.Fprintf(p.w, "no vacant human seats: the company's %d human "+
				"%s taken. Free one, or add a `kind: human` seat\n",
				declared, seats)
			return nil
		}
		fmt.Fprintln(p.w, "the company has no human seats. Add a `kind: "+
			"human` seat for each person first (Org chart › Edit org, or "+
			"`crewlet config import FILE`)")
		return nil
	}
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HANDLE\tNAME\tUNIT\tHOLDER\tSTAGE\tINVITATION\tEXPIRES")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		holder, _ := row["holder"].(map[string]any)
		invitation, _ := row["invitation"].(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			str(row["handle"]), dash(str(row["name"])), dash(str(row["unit"])),
			dash(seatHolder(holder)), dash(str(holder["stage"])),
			dash(seatInvitation(invitation)),
			dash(stamp(invitation["expires_at"])))
	}
	return tw.Flush()
}

// seatHolder is the HOLDER column: whoever the directory binds to the seat,
// and a service account said to be one — because the two free a seat
// differently, a person by a move or a removal and a service account by an
// unbind.
//
// WHO IT IS DEPENDS ON WHO ASKS. An admin, and the holder themselves, are
// answered the holder's login (their id where it has none); everybody else is
// answered only the name the holder goes by, because the overview says who
// holds a seat and not what they sign in with — "name unavailable" where this
// node's keyring did not open it, and "held" for a holder enrolled with no
// name. NEVER EMPTY for a held seat: an empty cell renders as a dash, which is
// exactly how a vacant seat reads, and it read so for every reader without
// admin.
func seatHolder(holder map[string]any) string {
	if holder == nil {
		return ""
	}
	who := str(holder["login"])
	if who == "" {
		who = str(holder["person"])
	}
	if who == "" {
		who = str(holder["name"])
	}
	if who == "" && truthy(holder["sealed"]) {
		who = "(name unavailable)"
	}
	if who == "" {
		who = "(held)"
	}
	if iam.Kind(str(holder["kind"])) == iam.KindMachine {
		who += " (service account)"
	}
	return who
}

// seatInvitation is the INVITATION column: the id `cancel-invite` takes, and
// the address it was sent to where this node's keyring opens it.
func seatInvitation(invitation map[string]any) string {
	if invitation == nil {
		return ""
	}
	id := str(invitation["id"])
	switch {
	case truthy(invitation["sealed"]):
		return id + " (address can't be read: this node's keyring is missing its key)"
	case str(invitation["email"]) != "":
		return id + " (" + str(invitation["email"]) + ")"
	}
	return id
}

// reset prints a reset link ONCE, for the invitation's reason, and where the
// issue stands, as every write here says ([iamPrinter.outcome]) — on `pending`
// too, since the record is durable and the link opens wherever it has been
// applied. An issue nobody can confirm prints nothing ([iamPrinter.settled]).
func (p *iamPrinter) reset(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if err := p.settled(answer); err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	fmt.Fprintf(p.w, "password reset for %s\n", str(answer["id"]))
	p.outcome(answer)
	fmt.Fprintf(p.w, "%s\n\n", str(answer["url"]))
	fmt.Fprintf(p.w, "expires %s\n", stamp(answer["expires_at"]))
	fmt.Fprintln(p.w, "This link is shown once. It sets a new password "+
		"and signs them out everywhere. Send it to them yourself (Crewlet "+
		"doesn't send email).")
	return nil
}

// token prints the value ONCE, for the same reason, and where the mint stands —
// on `pending` too: the record is durable, and a node that has not applied it
// yet answers a request carrying the token 503, that it cannot say yet, rather
// than refusing it. A mint nobody can confirm prints nothing
// ([iamPrinter.settled]), as the node's own unknown hands out no value.
//
// THE VALUE STAYS THE FIRST LINE, alone, so whatever reads it from this
// command's output takes the line it always took.
func (p *iamPrinter) token(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if err := p.settled(answer); err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	fmt.Fprintf(p.w, "%s\n\n", str(answer["token"]))
	fmt.Fprintf(p.w, "token %s for %s, expires %s\n", str(answer["id"]),
		str(answer["person"]), stamp(answer["expires_at"]))
	p.outcome(answer)
	fmt.Fprintf(p.w, "carries %s\n", dash(joinAny(answer["grants"])))
	fmt.Fprintln(p.w, "This token is shown once. Use it as "+
		apiTokenEnv+" or in an `Authorization: Bearer` header.")
	return nil
}

// written prints a write's outcome, which is the three-valued answer rather
// than "done".
func (p *iamPrinter) written(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if err := p.settled(answer); err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	if id := str(answer["id"]); id != "" {
		fmt.Fprintf(p.w, "%s\n", id)
	}
	p.outcome(answer)
	if detail := str(answer["detail"]); detail != "" {
		fmt.Fprintln(p.w, detail)
	}
	return nil
}

// outcome prints the line saying where a write stands.
//
// READ OFF THE ANSWER'S `outcome`, never off the status: it printed "applied"
// for every 2xx, so a write the node had made durable and NOT applied — a 202,
// whose read here does not show it yet — was reported as applied. Only the two
// outcomes that say where a write stands reach here: the third is a 503, which
// [iamRefusal] says, and a 2xx carrying any other has already failed
// ([iamPrinter.settled]).
func (p *iamPrinter) outcome(answer map[string]any) {
	fmt.Fprintf(p.w, "%s at %s (op %s)\n", str(answer["outcome"]),
		dash(str(answer["position"])), dash(str(answer["op_id"])))
}

// created prints a create: who was created, under which login and on which
// seat, where the write stands — and, for a PERSON, their first password link
// ONCE, which is the only time this command shows it.
//
// THE LINK IS PRINTED ON `pending` TOO: the record is durable, and the link
// opens on every node that has applied it — this one within moments. And on
// nothing else: a create nobody can confirm prints nothing of itself
// ([iamPrinter.settled]).
//
// THE NODE'S SENTENCE BESIDE THE LINK IS NOT PRINTED, because it names the
// retry as a header: the flag this command takes is -op-id, and a
// create retried under it hands back this same link. Where the link no longer
// opens — a retry after it was spent, revoked or aged out — the node says so
// and this names the command that issues another.
func (p *iamPrinter) created(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if err := p.settled(answer); err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	id := str(answer["id"])
	fmt.Fprintf(p.w, "%s\n", id)
	fmt.Fprintf(p.w, "login %s", dash(str(answer["login"])))
	if seat := str(answer["seat"]); seat != "" {
		fmt.Fprintf(p.w, ", on seat %s", seat)
	}
	fmt.Fprintln(p.w)
	p.outcome(answer)
	switch link := str(answer["url"]); {
	case link != "":
		fmt.Fprintf(p.w, "\npassword link for %s\n%s\n\n", id, link)
		fmt.Fprintf(p.w, "expires %s\n", stamp(answer["expires_at"]))
		fmt.Fprintf(p.w, "This link is shown once. Send it to them yourself "+
			"(Crewlet doesn't send email). Run this again with "+
			"-op-id %s to show the same link.\n",
			dash(str(answer["op_id"])))
	case iam.Kind(str(answer["kind"])) == iam.KindMachine:
		fmt.Fprintf(p.w, "create its access token with `crewlet iam token -person %s`\n", id)
	default:
		if detail := str(answer["detail"]); detail != "" {
			fmt.Fprintln(p.w, detail)
		}
		fmt.Fprintf(p.w, "issue them a password reset link with `crewlet iam "+
			"reset-password %s`\n", id)
	}
	return nil
}

// --- small renderings ---------------------------------------------------- //

func str(v any) string {
	switch held := v.(type) {
	case string:
		return held
	case float64:
		return strconv.FormatFloat(held, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(held)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func truthy(v any) bool {
	held, _ := v.(bool)
	return held
}

func joinAny(v any) string {
	held, _ := v.([]any)
	out := make([]string, 0, len(held))
	for _, item := range held {
		out = append(out, str(item))
	}
	return strings.Join(out, ",")
}

// stamp renders an instant in the operator's own zone.
//
// LOCAL RATHER THAN UTC, deliberately, and only here: every instant this
// engine STORES is the broker's and every comparison is between positions, so
// the one place a zone is a kindness rather than a hazard is the line a person
// reads.
func stamp(v any) string {
	raw := str(v)
	if raw == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	if parsed.IsZero() {
		return ""
	}
	return parsed.Local().Format("2006-01-02 15:04")
}

// dash renders an empty cell as something a reader can see: a blank column
// reads as a bug, while a service account bound to no seat, or a seat nobody
// holds, is a real state the table should name.
func dash(in string) string {
	if in == "" {
		return "–"
	}
	return in
}
