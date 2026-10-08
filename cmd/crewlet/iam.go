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

const iamUsage = `crewlet iam — the company's people, credentials and sessions

Usage:
  crewlet iam people [-q TERM] [-stage S] [-limit N] [-after ID]
                                                       The directory
  crewlet iam show ID                                  One person, in full
  crewlet iam seats [-unheld]                          The human seats, who holds each,
                                                       and any open invitation for one
  crewlet iam invite EMAIL -seat SEAT [-grants G,...]
                                                       Invite somebody onto a vacant human
                                                       seat; the link is shown ONCE
  crewlet iam invitations [-all] [-limit N] [-after ID]
                                                       The open invitations; -all
                                                       adds the expired and redeemed
  crewlet iam cancel-invite ID                         Withdraw an invitation: its link
                                                       opens nothing, its address and
                                                       its seat are free
  crewlet iam reset-password ID|LOGIN                  Issue a one-time password reset
                                                       link, shown ONCE, good for a day
  crewlet iam create -email E -seat SEAT [-login L] [-name N] [-grants G,...]
                                                       Create a person on a vacant human
                                                       seat; their first password link
                                                       is shown ONCE
  crewlet iam create -kind machine -login L [-seat SEAT] [-name N] [-grants G,...]
                                                       Create a service account
  crewlet iam bind ID SEAT                             Move a person to another vacant
                                                       human seat, or bind a service
                                                       account to one
  crewlet iam unbind ID                                Take a SERVICE ACCOUNT's seat
                                                       back; a person always holds one
  crewlet iam grant ID -grants G,...                   Change what somebody carries
  crewlet iam suspend ID                               Stop them acting and end their
                                                       sessions, tokens and reset link;
                                                       keep the row
  crewlet iam activate ID                              Let them act again
  crewlet iam remove ID                                Tombstone them and erase what is theirs
  crewlet iam revoke ID                                End every session and token they hold
  crewlet iam sessions ID                              Their sessions, newest first
  crewlet iam credentials [-person ID]                 What somebody proves themselves with
  crewlet iam token -login L [-label L] [-days N] [-grants G,...]
                                                       Mint YOUR OWN machine token, shown ONCE
  crewlet iam token -person ID [-label L] [-days N] [-grants G,...]
                                                       Mint a service account's, shown ONCE
  crewlet iam revoke-credential CREDENTIAL_ID [-person ID]
                                                       Withdraw one credential
  crewlet iam reset-mfa ID                             Clear the second factor and end sessions
  crewlet iam invalidate-all                           Invalidate EVERY session and machine token
  crewlet iam check                                    What is wrong with this company's access
  crewlet iam audit [-person ID] [-event OP] [-since POSITION] [-at TIME]
                    [-before POSITION]                 The identity estate's own trail

Flags:
  -config PATH   Tier A config naming the node to reach (default %q)
  -api URL       The running node's API; default is its own api.host:port
  -json          Print the raw answer rather than a table
  -reason TEXT   Recorded on the change, and read by whoever audits it
  -idempotency-key OP
                 Retry a write whose outcome was unknown, as the SAME operation
  -seat SEAT     On invite and create: the human seat they will hold — one
                 nobody holds and no open invitation is for, which "iam seats
                 -unheld" lists. Required on invite and on a person's create;
                 optional on a service account's
  -unheld        On seats: only the seats nobody holds and no open invitation
                 is for

Every write prints its outcome: "applied" means this node has the change,
"pending" that it is durable and this node has not applied it yet. A write
nothing could establish fails naming its op id; run the same command again
with -idempotency-key <op id> — a fresh attempt would be a second operation,
and for create and invite one the first attempt's person or invitation refuses,
since it holds the address and the seat. Where the node says it cannot vouch
for the operation, run it through another node with -api: asked again, that
node answers the same way until the change reaches it.

A person holds a human seat for as long as they are here: they are invited or
created onto one nobody holds, "iam bind" moves them to another, and "iam
remove" frees it. "iam unbind" is for service accounts, which may hold one or
none.

A person made by "iam create" arrives with a FIRST PASSWORD LINK, printed once,
good for a week: it sets their first password once, through the screen a reset
link opens, and signs nobody in — they sign in afterwards, enrolling a second
factor there where the deployment requires one. A create retried with
-idempotency-key <op id> hands back the SAME link, since it is derived from the
operation; one that no longer opens is replaced with "iam reset-password". A
grant added to them before they use it ends it, as it ends a reset link.

Export CREWLET_API_TOKEN to authenticate. Every /iam route is guarded, reads
included: a map of who can reach a company is worth as much as the grants.

A reset link from "iam reset-password" sets the person a new password once, ends
every session and token they hold, and signs nobody in: they sign in afterwards,
where a second factor they hold still applies. Whatever ends their sessions ends
an outstanding link too — suspend, revoke, reset-mfa and invalidate-all — so
issue the link after any of those it is needed beside. A person changes their OWN
password signed in, through POST /auth/password, which asks for the current one
— and a code from anybody holding a second factor, as a step-up does — and
needs a person present, which neither a Tier A token nor a machine token is,
so this command has no flow for it.

A token minted by "iam token" acts as the person or service account it names,
carrying at most what they hold now, for at most a year (90 days unless -days
says otherwise). It is itself a CREWLET_API_TOKEN for every other command — but
it cannot mint another token, and it cannot change how its owner signs in.

A person's token is theirs alone to mint: whoever mints one sees its value, and
it acts as them. So "iam token -login" signs you in for the one request — the
password from the terminal without echo, or the first line piped in, and a
second-factor code the same way when your account holds one, or the line after
it; never a flag, because a recovery code on a command line stays good in the
shell's history — mints, and signs out, reading no CREWLET_API_TOKEN. An administrator mints with -person for SERVICE ACCOUNTS
only; every write the token makes is recorded as its owner's, through
pat:<credential id>.
`

// runIAM dispatches `crewlet iam`.
//
// STDIN IS AN ARGUMENT for the one subcommand that reads it — `token -login`,
// whose password is never a flag — so a suite can pipe one in.
func runIAM(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprintf(stdout, iamUsage, defaultBootstrapPath)
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
		"Tier A config: the node this command reaches")
	apiURL := fs.String("api", "",
		"the running node's API; default is the api.host:port in -config")
	asJSON := fs.Bool("json", false, "print the raw answer")
	reason := fs.String("reason", "", "recorded on the change")
	grants := fs.String("grants", "",
		"a comma-separated grant list, or `none` for an empty one")
	stage := fs.String("stage", "", "narrow the directory to one enrolment stage")
	term := fs.String("q", "", "narrow the directory on a login or a seat")
	limit := fs.Int("limit", 0, "how many rows at most")
	login := fs.String("login", "",
		"the login to create somebody under, or to sign in as to mint your own token")
	email := fs.String("email", "", "the address to create somebody under")
	kind := fs.String("kind", "", "person or machine (create only)")
	name := fs.String("name", "", "the person's own name")
	person := fs.String("person", "",
		"whose credentials or sessions, and whom a minted token is for")
	label := fs.String("label", "", "what to call a minted token")
	days := fs.Int("days", 0, "how long a minted token lasts")
	event := fs.String("event", "", "narrow the trail to one operation")
	since := fs.Uint64("since", 0, "the oldest log POSITION to include")
	// THE TWO CURSORS A LISTING'S LAST LINE NAMES, which it named while
	// neither was a flag here: following "more: crewlet iam people -after
	// <id>" answered "flag provided but not defined".
	after := fs.String("after", "", "a listing's next page starts after this id")
	before := fs.Uint64("before", 0, "the trail's next page ends before this POSITION")
	at := fs.String("at", "", "an RFC 3339 instant, resolved to a position once")
	key := fs.String("idempotency-key", "",
		"retry a write whose outcome was unknown: the op id its answer named")
	seat := fs.String("seat", "",
		"the human seat an invitation or a create binds (required on invite "+
			"and for a person's create)")
	all := fs.Bool("all", false,
		"list the expired and redeemed invitations too (invitations only)")
	unheld := fs.Bool("unheld", false,
		"only the seats nobody holds and no open invitation is for (seats only)")
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
	// A KEY IS THE OP ID AN EARLIER ANSWER NAMED, held to the rule every
	// surface holds a caller's operation id to ([statelog.CheckCallerOpID])
	// before anything is read or sent, and sent as given: the node refuses
	// any other `400 op_id_invalid`, and refused here the error names this
	// command's flag rather than a header it composed.
	if *key != "" {
		if err := statelog.CheckCallerOpID(*key); err != nil {
			return fmt.Errorf("-idempotency-key: %w", err)
		}
	}
	// -seat IS invite's AND create's — the seat somebody new takes — and
	// refused anywhere else rather than ignored: `iam grant ID -seat lead`
	// read as accepted would say a seat was taken that nothing sent. Somebody
	// who already exists is moved with `bind`.
	if strings.TrimSpace(*seat) != "" && sub != "invite" && sub != "create" {
		return fmt.Errorf("-seat is invite's and create's: it names the seat "+
			"somebody new takes — move somebody who already exists with "+
			"`crewlet iam bind ID SEAT`, not with `iam %s -seat`", sub)
	}
	// -all IS invitations', refused elsewhere for -seat's reason: `iam
	// people -all` read as accepted would say every stage was listed.
	if *all && sub != "invitations" {
		return fmt.Errorf("-all is invitations': it adds the expired and "+
			"redeemed ones — `iam %s` takes no -all", sub)
	}
	// -unheld IS seats', for the same reason: `iam people -unheld` read as
	// accepted would say nobody listed holds a seat.
	if *unheld && sub != "seats" {
		return fmt.Errorf("-unheld is seats': it leaves out every seat "+
			"somebody holds or an open invitation is for — `iam %s` takes "+
			"no -unheld", sub)
	}
	// WHAT A NEW PERSON OR SERVICE ACCOUNT HAS TO BE GIVEN, told here
	// rather than by the node, before a config file is read or a request
	// is sent — the node refuses each the same way, and an operator who
	// left a flag off needs the sentence naming it rather than a round
	// trip to a 400. See [iamNewcomer].
	if err := iamNewcomer(sub, *kind, *email, *login, *seat); err != nil {
		return err
	}
	// A TOKEN HAS AN OWNER, and which kind decides who may mint it: a
	// person mints their own, signed in (-login), and a service account's
	// is minted by whoever manages people (-person). Naming both, or
	// neither, is told so here rather than by the node.
	if sub == "token" {
		owner := strings.TrimSpace(orSubject(*person, subject))
		switch self := strings.TrimSpace(*login); {
		case self != "" && owner != "":
			return errors.New("name one owner: -login signs in to mint your " +
				"own token, -person names the service account an " +
				"administrator mints one for")
		case self == "" && owner == "":
			return errors.New("name the owner: -login <your login> to mint " +
				"your own token, or -person <service account id> to mint one " +
				"for a pipeline")
		}
	}
	boot, err := config.LoadBootstrap(*bootstrapPath, config.EnvOnly())
	if err != nil {
		return fmt.Errorf("read %s: %w", *bootstrapPath, err)
	}
	ctx := context.Background()
	out := &iamPrinter{w: stdout, raw: *asJSON}
	if sub == "token" && strings.TrimSpace(*login) != "" {
		// YOUR OWN TOKEN, which only your own session may mint: this
		// signs in for the one request and never reads
		// CREWLET_API_TOKEN. See iamself.go.
		return out.token(mintOwnToken(ctx, boot, *apiURL,
			strings.TrimSpace(*login), iamTokenBody(*grants, *label, *days),
			stdin, stderr))
	}
	client, err := newIAMClient(boot, *apiURL)
	if err != nil {
		return err
	}
	client.key, client.keyed = *key, iamKeyed[sub]

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
		body := iamGrantsBody(*grants)
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
		body := iamGrantsBody(*grants)
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
		body := iamGrantsBody(*grants)
		if len(body) == 0 {
			return errors.New("name what to change: -grants")
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
			iamTokenBody(*grants, *label, *days)))
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
	fmt.Fprintf(stdout, iamUsage, defaultBootstrapPath)
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
// A PERSON HOLDS A HUMAN SEAT for as long as they are here (ADR-0026), so an
// invitation and a person's create each name one; a SERVICE ACCOUNT's is
// optional, bound only to act as it. A person's login may be left out — the
// node proposes it from their address — and a service account's may not,
// since it has no address to propose from.
//
// THE KIND IS THE FLAG'S, never guessed from the login's shape: the node reads
// a blank kind as a person, and so does this. A kind this command does not
// name is the node's to refuse, in its own words.
func iamNewcomer(sub, kind, email, login, seat string) error {
	seatless := strings.TrimSpace(seat) == ""
	switch sub {
	case "invite":
		if seatless {
			return errors.New("name the seat the invitation is for: -seat " +
				"<handle>, a human seat nobody holds and no open invitation " +
				"is for (`crewlet iam seats -unheld` lists them) — a person " +
				"always holds a seat, so nobody is invited onto none")
		}
	case "create":
		switch iam.Kind(strings.TrimSpace(kind)) {
		case iam.KindMachine:
			if strings.TrimSpace(login) == "" {
				return errors.New("name the login to create the service " +
					"account under: -login ci:release (or token:<id> for a " +
					"Tier A token) — it has no address to be found by, so " +
					"its login is how it is found and the name its changes " +
					"are recorded under")
			}
		case "", iam.KindPerson:
			if strings.TrimSpace(email) == "" {
				return errors.New("name the address to create them under: " +
					"-email jane@example.com — a person is found by it, and " +
					"their login is proposed from it unless -login names one")
			}
			if seatless {
				return errors.New("name the seat to create them on: -seat " +
					"<handle>, a human seat nobody holds and no open " +
					"invitation is for (`crewlet iam seats -unheld` lists " +
					"them) — a person always holds one; a service account " +
					"(-kind machine) may be created without one")
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

// iamUnkeyed refuses -idempotency-key on a subcommand whose route reads none,
// saying what to do instead: sent anyway, the key would be ignored and the
// operator told nothing, which is the one outcome worse than the refusal.
func iamUnkeyed(sub string) error {
	switch sub {
	case "token":
		return errors.New("a mint takes no -idempotency-key: its retry would " +
			"answer the first attempt's record beside a value that verifies " +
			"against nothing, so a mint whose outcome was unknown is minted " +
			"again, and the one that may have landed is a token nobody holds, " +
			"which expires")
	case "reset-password":
		return errors.New("a reset link takes no -idempotency-key: its retry " +
			"could not hand back the secret the first attempt was never shown, " +
			"so a link whose issue was unknown is issued again — which revokes " +
			"the one that may have landed")
	}
	return fmt.Errorf("-idempotency-key retries a write whose outcome was "+
		"unknown, and `iam %s` writes nothing", sub)
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

// iamGrantsBody turns the -grants flag into a patch body.
//
// `none` IS A VALUE AND AN OMITTED FLAG IS NOT. Stripping somebody's last
// grant and not mentioning grants at all are opposite intentions, and an
// empty string cannot carry both — so the word is explicit, and it is the one
// spelling that could never be a grant.
func iamGrantsBody(grants string) map[string]any {
	body := map[string]any{}
	switch strings.TrimSpace(grants) {
	case "":
	case "none":
		body["grants"] = []string{}
	default:
		parts := strings.Split(grants, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		body["grants"] = out
	}
	return body
}

// iamTokenBody is a mint's request: what the token carries, what it is called
// and how long it lasts — the same whoever the owner is.
func iamTokenBody(grants, label string, days int) map[string]any {
	body := iamGrantsBody(grants)
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

	// key is the operation a write RETRIES — the op id an unknown answer
	// named — sent as the Idempotency-Key on every write this command
	// makes, and never on a read.
	key string

	// keyed is whether the route this command writes to reads a key at all
	// ([iamKeyed]). Only where it does is a refusal's retry spelled as
	// -idempotency-key: a mint reads none, and was told to retry with a
	// flag this command refuses on it.
	keyed bool
}

func newIAMClient(boot *config.Bootstrap, override string) (*iamClient, error) {
	base, err := nodeBaseURL(boot, override, "the company's identity directory")
	if err != nil {
		return nil, err
	}
	token, err := nodeAPIToken("/iam")
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
	if c.key != "" && method != http.MethodGet {
		req.Header.Set(opkey.Header, c.key)
	}
	// NO ORIGIN, deliberately. The node's cross-site check admits a
	// BEARER with none — a browser cannot make one travel — and refuses a
	// PRESENT Origin that is not an address the deployment is reached at.
	// This client used to send its own base URL, which is the loopback
	// address it dials and almost never `api.external_url`, so every write
	// it made to a deployment behind a proxy was refused as cross-site.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", target, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, iamMaxAnswer))
	if err != nil {
		return nil, fmt.Errorf("read the answer from %s: %w", target, err)
	}
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	if resp.StatusCode >= 400 {
		return answer, iamRefusal(resp.StatusCode, answer, raw, c.keyed)
	}
	return answer, nil
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
		return "", fmt.Errorf("nobody in the directory holds the login %q — "+
			"name the person by their id, which `crewlet iam people` lists", subject)
	}
	return "", fmt.Errorf("%d people hold the login %q, which the directory "+
		"refuses to happen — name the one you mean by their id (%s)", len(held),
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
// the op id is named for the trail and no -idempotency-key is prescribed.
func iamRefusal(status int, answer map[string]any, raw []byte, keyed bool) error {
	if msg, ok := credentialRefusal(status, raw, true); ok {
		return errors.New(msg)
	}
	code, _ := answer["error"].(string)
	detail, _ := answer["detail"].(string)
	if detail == "" {
		detail, _ = answer["message"].(string)
	}
	var said string
	switch {
	case code != "" && detail != "":
		said = code + ": " + detail
	case code != "":
		said = code
	case len(raw) > 0:
		said = fmt.Sprintf("the node answered %d: %s", status,
			strings.TrimSpace(string(raw)))
	default:
		said = fmt.Sprintf("the node answered %d", status)
	}
	if outcome, _ := answer["outcome"].(string); outcome == string(statelog.OutcomeUnknown) {
		said = "the node could not establish whether this landed: " + said
	}
	if landed := joinAny(answer["landed"]); landed != "" {
		said += "\nthese changes DID land before it: " + landed
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
			said += "\nthis node cannot tell whether it landed and answers the " +
				"same way until it can: read whether it did, or retry it as the " +
				"same operation through another node with -api <that node> " +
				"-idempotency-key " + op + " — never a fresh key"
		default:
			// THE SAME OPERATION, whichever 503 this is: an outcome
			// nobody could establish is answered from whatever of it
			// landed, and a refusal that wrote nothing runs under the
			// same key as it would have under a fresh one.
			said += "\nretry it as the same operation with -idempotency-key " +
				op + " — whatever of it landed is answered from what landed, " +
				"where a fresh key would be a second operation"
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
		out += "\nheld by open invitation " + inv + " — `crewlet iam " +
			"cancel-invite " + inv + "` withdraws it"
	}
	if code == string(httpjson.CodeSeatRequired) {
		if id := str(answer["id"]); id != "" {
			out += "\na person always holds a seat: `crewlet iam bind " + id +
				" SEAT` moves them to another human seat and `crewlet iam " +
				"remove " + id + "` frees it — `iam unbind` is for service " +
				"accounts"
		} else {
			out += "\nname it with -seat <handle>: `crewlet iam seats " +
				"-unheld` lists the human seats nobody holds"
		}
	}
	return out
}

// --- printing ------------------------------------------------------------ //

// iamPrinter renders one answer, as a table or as the raw JSON.
type iamPrinter struct {
	w   io.Writer
	raw bool
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
		return "(sealed under a key this node's keyring does not hold)"
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
		{"revocation epoch", str(answer["revocation_epoch"])},
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
		fmt.Fprintf(p.w, "%d seat binding(s) could not be checked: this "+
			"node's company could not say whether their seats exist — ask "+
			"a node that has applied the company\n", int(unchecked))
	}
	if len(rows) == 0 {
		fmt.Fprintf(p.w, "nothing to report, as of %s\n",
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

// invite prints the link ONCE, which is the only time it exists.
func (p *iamPrinter) invite(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	fmt.Fprintf(p.w, "invitation %s\n%s\n\n", str(answer["id"]),
		str(answer["url"]))
	fmt.Fprintf(p.w, "expires %s\n", stamp(answer["expires_at"]))
	fmt.Fprintln(p.w, "This link is shown once and cannot be read back: what "+
		"the estate keeps is a hash of the secret after the id. Send it to "+
		"them yourself — this engine never sends mail.")
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
			email = "(sealed under a key this node's keyring does not hold)"
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
			fmt.Fprintf(p.w, "no human seat is vacant, and the node could not "+
				"list every seat to say whether the company declares any: "+
				"%v — `crewlet iam seats` lists them\n", counting)
			return nil
		case unheld && declared > 0:
			seats := "seat is"
			if declared != 1 {
				seats = "seats are"
			}
			fmt.Fprintf(p.w, "no human seat is vacant: the company's %d human "+
				"%s each held or an open invitation is for it — free one "+
				"(`crewlet iam remove ID` removes a person, `crewlet iam "+
				"unbind ID` unbinds a service account, `crewlet iam "+
				"cancel-invite ID` withdraws an invitation) or add a `kind: "+
				"human` seat to the company\n", declared, seats)
			return nil
		}
		fmt.Fprintln(p.w, "the company declares no human seat, so nobody can "+
			"be invited or created: a person always holds one — add a "+
			"`kind: human` seat (Agents › Edit org, or `crewlet config "+
			"import FILE`) for each person first")
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

// seatHolder is the HOLDER column: whoever the directory binds to the seat, by
// their login where they have one, and a service account said to be one —
// because the two free a seat differently, a person by a move or a removal and
// a service account by an unbind.
func seatHolder(holder map[string]any) string {
	if holder == nil {
		return ""
	}
	who := str(holder["login"])
	if who == "" {
		who = str(holder["person"])
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
		return id + " (address sealed under a key this node's keyring does not hold)"
	case str(invitation["email"]) != "":
		return id + " (" + str(invitation["email"]) + ")"
	}
	return id
}

// reset prints a reset link ONCE, for the invitation's reason.
func (p *iamPrinter) reset(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	fmt.Fprintf(p.w, "password reset for %s\n%s\n\n", str(answer["id"]),
		str(answer["url"]))
	fmt.Fprintf(p.w, "expires %s\n", stamp(answer["expires_at"]))
	fmt.Fprintln(p.w, "This link is shown once and cannot be read back. It "+
		"sets a new password once and ends every session and token the person "+
		"holds; issuing another revokes it. Send it to them yourself — this "+
		"engine never sends mail.")
	return nil
}

// token prints the value ONCE, for the same reason.
func (p *iamPrinter) token(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	fmt.Fprintf(p.w, "%s\n\n", str(answer["token"]))
	fmt.Fprintf(p.w, "credential %s for %s, expires %s\n", str(answer["id"]),
		str(answer["person"]), stamp(answer["expires_at"]))
	fmt.Fprintf(p.w, "carries %s\n", dash(joinAny(answer["grants"])))
	fmt.Fprintln(p.w, "This value is shown once. What the estate holds is a "+
		"hash of it, so nothing can read it back. Present it as "+
		apiTokenEnv+", or as an `Authorization: Bearer` header.")
	return nil
}

// written prints a write's outcome, which is the three-valued answer rather
// than "done".
func (p *iamPrinter) written(answer map[string]any, err error) error {
	if err != nil {
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
// whose read here does not show it yet — was reported as applied. The third
// value never reaches here; it is a 503, and [iamRefusal] says it.
func (p *iamPrinter) outcome(answer map[string]any) {
	position, op := dash(str(answer["position"])), dash(str(answer["op_id"]))
	switch outcome := str(answer["outcome"]); outcome {
	case "applied", "pending":
		fmt.Fprintf(p.w, "%s at %s (op %s)\n", outcome, position, op)
	default:
		// AN OUTCOME THIS BUILD CANNOT NAME is printed as what it is,
		// never promoted to "applied" — that was the bug.
		fmt.Fprintf(p.w, "outcome %q at %s (op %s)\n", outcome, position, op)
	}
}

// created prints a create: who was created, under which login and on which
// seat, where the write stands — and, for a PERSON, their first password link
// ONCE, which is the only time this command shows it.
//
// THE LINK IS PRINTED ON `pending` TOO: the record is durable, and the link
// opens on every node that has applied it — this one within moments.
//
// THE NODE'S SENTENCE BESIDE THE LINK IS NOT PRINTED, because it names the
// retry as a header: the one this command sends is -idempotency-key, and a
// create retried under it hands back this same link. Where the link no longer
// opens — a retry after it was spent, revoked or aged out — the node says so
// and this names the command that issues another.
func (p *iamPrinter) created(answer map[string]any, err error) error {
	if err != nil {
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
		fmt.Fprintf(p.w, "\nfirst password link for %s\n%s\n\n", id, link)
		fmt.Fprintf(p.w, "expires %s\n", stamp(answer["expires_at"]))
		fmt.Fprintf(p.w, "This link is shown once and cannot be read back. It "+
			"sets their first password once and signs nobody in: they sign in "+
			"afterwards. Send it to them yourself — this engine never sends "+
			"mail. Run again with -idempotency-key %s, this command hands back "+
			"this same link.\n", dash(str(answer["op_id"])))
	case iam.Kind(str(answer["kind"])) == iam.KindMachine:
		fmt.Fprintf(p.w, "mint its token with `crewlet iam token -person %s`\n", id)
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
		return "—"
	}
	return in
}
