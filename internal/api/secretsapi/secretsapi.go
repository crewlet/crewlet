// Package secretsapi serves /secrets: the fleet's credential store, written
// through the one process that can reach it.
//
// # Why the CLI cannot just write the KV itself
//
// On the default topology the coordination broker runs INSIDE the engine's
// process and does not listen on a socket at all, so a second process has no
// way to reach the bucket. That is the whole reason this surface exists: the
// rows are fleet-wide, and the engine is the only thing that
// can put one there.
//
// # Every route is admin's, reads included
//
// Each is mounted through [authz.Router] with the verb the authority table
// decides it by: the listing and one row's metadata are `secrets.list`,
// revealing a value is `secrets.reveal`, and storing, rotating, deleting and
// re-keying are `secrets.write` — all three admin's (ADR-0033), and the two
// that hand out or change a credential also ask a recent proof, the reveal a
// person present besides. A listing says which credentials a company holds and
// when each last changed, which is reconnaissance even without the values, so
// it is not part of everybody's overview.
//
// THE THREE STAY THREE VERBS although one permission admits them: what
// differs between them is the proof and the presence each row asks, and the
// route names the verb so the table decides those, rather than a check here
// that could drift from it.
//
// THE ROUTES USED TO DECIDE NOTHING. They resolved the caller for the audit
// line and asked no permission, which was sound while the only credential this
// engine had was an operator token: "somebody resolved" meant "the operator".
// It stopped being sound the day a person could sign in holding a read-only
// permission alone — every one of them could then read, reveal, overwrite and
// delete the company's credentials.
//
// # The engine's own key material is not reachable from here at all
//
// The same bucket holds the identity estate's blind-index key, under a name in
// the engine's own namespace ([secrets.Reserved]). Every route that takes a
// name refuses one with `403 reserved_name` before anything else — whatever the
// caller holds — and the listing counts them per keyring key without naming
// any. It was once an ordinary name here, and that made a DELETE a way to
// orphan every address in the directory.
//
// # A rekey is the whole rotation, the identity estate included
//
// The keyring that seals this bucket also seals every person's name, address
// and second factor in the identity estate, whose rows are derived from a log
// and can only be changed by a record. So `POST /secrets/rekey` moves both —
// the store's rows at the version it read, and the identity estate's through
// the node's own writer ([Identity]) — and the listing counts both, because
// the answer an operator retires the old key on is "nothing is under it".
//
// The Dashboard Assistant's conversations are sealed under the same keyring
// and are not moved: each value leaves with its conversation, so the listing
// counts them too ([Assistant]) and the old key stays on the ring until that
// count under it reaches zero.
//
// # Two routes return a value, and both are break-glass
//
// `GET /secrets/{name}` requires an explicit ?reveal=true — a path that cannot
// be reached by accident or by a crawl — and `POST /secrets/reveal`, which
// answers the values of the names its body asks for, is a POST no link or
// crawl can make. Both are decided on `secrets.reveal` and both log the access
// by operator and name. A read-back that leaves no trace is indistinguishable
// from an exfiltration, and the name is the whole of what can be logged,
// because logging the value would be the leak.
//
// The second exists for one reader: a CLI run resolving a company document's
// `${VAR}` references through a running node, which reads the variables its
// command reads that the node lists, and asks for them together rather than
// one request per credential. It is handed exactly the names it asks for —
// never the store.
//
// # A write re-activates the revision that reads it (ADR-0070)
//
// A `${VAR}` in the company document is resolved when a provider or a
// transport is built, from a snapshot an apply takes, so a value sealed and
// left there changes nothing that is running: the old credential keeps
// signing every model and integration in until something re-activates, and
// nothing a person was shown said so. So every write here (`PUT
// /secrets/{name}`, the batch `PUT /secrets` and `DELETE /secrets/{name}`)
// asks the config surface which paths of the active revision read the names it
// wrote ([Reactivator]), and when any does, re-activates that revision
// SYNCHRONOUSLY after the store write, in the same request: the answer names
// the paths (`read_by`) and the revision and epoch it produced (`reload`), so
// the page that made the write can show the epoch its change lands at.
//
// Synchronous rather than through the engine's coalescing republisher, which
// exists to bound a provisioning loop that might never converge: a person's
// write is bounded by the person and by step-up, and the answer has to name
// what it produced.
//
// THE STATUS STAYS 200 whatever the reload did. The value is sealed before the
// reload is asked for, so a reload that lost a race or found the company
// unrunnable is said in `reload.error` beside a write that happened:
// crewlet-operator reads exactly 200 from this route. And the write asks
// `config.write` of the guard before anything is sealed when a reload will
// follow, as `/setup` asks the credential write on top of its own verb: both
// are admin's with a recent proof, so nobody this route admitted before is
// refused now.
//
// A MULTI-SECRET ROTATION IS ONE ACTIVATION. `PUT /secrets` seals every value
// of its body and then reloads once, so rotating four credentials an entry
// reads moves the fleet onto one new epoch rather than four, and the single
// route is a batch of one through the same function.
package secretsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
)

var log = logging.Get("api.secrets")

// MaxJSONValueBytes is the longest one stored value can be once it is spelled
// as a JSON string, which is how every reveal answers it: [encoding/json]
// writes each byte it escapes — a control character, `<`, `>`, `&` — as a
// six-byte \u escape, so six times [secrets.MaxValueBytes], the size every
// store holds a value to, and its two quotes. It is what a reader of a
// revealed value has to be prepared to take, since a value a write admitted
// and the read refused would be stored and unreadable.
const MaxJSONValueBytes = 6*secrets.MaxValueBytes + 2

// MaxRevealNames bounds the names one POST /secrets/reveal asks for.
//
// SIXTY-FOUR, from the answer it bounds: each name can carry a value of up to
// [MaxJSONValueBytes], so the largest answer is [MaxRevealAnswerBytes] —
// about 24 MiB — which is what a client has to be prepared to read in one
// piece. A company document names a few variables per seat (the shipped
// example reads 22 for eight seats), so most reads are one request, and a
// caller asking for more than this splits the set into requests of this size
// rather than one per name.
const MaxRevealNames = 64

// MaxRevealBodyBytes bounds a POST /secrets/reveal body: [MaxRevealNames]
// names of a kilobyte each, which no variable a document references is near.
const MaxRevealBodyBytes = MaxRevealNames << 10

// MaxRevealAnswerBytes is the longest answer POST /secrets/reveal can give:
// every name asked — whose bytes the body bounds — once, each beside a value
// of up to [MaxJSONValueBytes] and its punctuation, inside the envelope.
const MaxRevealAnswerBytes = MaxRevealBodyBytes + MaxRevealNames*(MaxJSONValueBytes+8) + 64

// RevealAnswerBytes is the longest answer `GET /secrets/{name}?reveal=true`
// can give for name: the name and its value, each at the length JSON spells it
// in — six bytes a byte at worst, the value's at [MaxJSONValueBytes] — inside
// the envelope. A function of the name because nothing else bounds one: the
// reader knows the name it asked for, and the value is the write's to bound.
func RevealAnswerBytes(name string) int {
	return 6*len(name) + 2 + MaxJSONValueBytes + 64
}

// Identity is the identity estate's half of a keyring rotation: the personal
// values — names, addresses, second factors — the same keyring seals in rows
// no store write can reach. The engine's `IdentityKeyring` is what a running
// node hands in.
type Identity interface {
	// SealedKeys counts the values those rows hold, by the keyring key
	// each is sealed under, opening none of them.
	SealedKeys(ctx context.Context) (iamdomain.SealedCount, error)

	// Reseal moves every person's values onto the active key.
	Reseal(ctx context.Context) (iamdomain.ResealReport, error)
}

// Assistant is the Dashboard Assistant's half of a keyring rotation: the
// conversations, cards and proposals the same keyring seals, which a rotation
// waits out rather than moves (assistantdomain.SealedCount says why).
type Assistant interface {
	// SealedKeys counts the values those rows hold, by the keyring key each
	// is sealed under, opening none of them.
	SealedKeys(ctx context.Context) (assistantdomain.SealedCount, error)
}

// IdentityResealed is what a rekey moved in the identity estate, COUNTED: how
// many people's records it published and how many values those carried. Never
// the ids — a rekey answers the person who ran it, and the count is what they
// need to retire the old key, while who carries a value under it is the
// directory's to answer.
type IdentityResealed struct {
	People int `json:"people"`
	Values int `json:"values"`
}

// Service is the /secrets surface.
type Service struct {
	store     *fleetsecrets.Store
	config    Reactivator
	identity  Identity
	assistant Assistant
	keyID     string
	now       func() time.Time

	// guard decides every route and the reveal a GET asks for on top of
	// its route. No chart: not one verb here asks a relation.
	guard authz.Guard
}

// Options wire the service.
type Options struct {
	// Fleet is the coordination backend holding the rows. Required: every
	// node opens the fleet store, and [New] refuses to build without it.
	Fleet coord.Secrets

	// Cipher seals and opens a value, under the keyring every node holds.
	// Required: [New] refuses to build without it.
	Cipher secrets.Cipher

	// ActiveKeyID is the keyring key a rekey re-seals onto: Tier A's
	// secrets.active_key_id. Required for the same reason.
	ActiveKeyID string

	// Identity is the identity estate's half of a rotation. NIL-ABLE for a
	// suite about the store alone — `crewlet run` always hands one over,
	// since every node serving the API holds the estate (ingress requires
	// the `data` role, ADR-0025) and runs the identity estate from boot —
	// and every
	// answer made without one SAYS so (`identity: null`) rather than
	// reading as an estate with nothing under an old key.
	Identity Identity

	// Assistant is the Dashboard Assistant's half of a rotation, NIL-ABLE
	// for the same reason as Identity, and answered `assistant_values: null`
	// without one.
	Assistant Assistant

	// Config re-activates the revision that reads a name this surface
	// wrote (ADR-0070): configapi's Service in `crewlet run`. Required, for
	// Fleet's reason: every node serving this surface holds the config
	// surface beside it (ingress requires `data`), and a surface built
	// without one would seal a rotated credential and leave every model
	// and integration signing in with the old one, saying nothing.
	Config Reactivator

	// Now is injectable so a test can pin a row's timestamp.
	Now func() time.Time

	// Guard decides each route's policy and every in-handler admission;
	// nil is the request context's caller with no org chart, which is what
	// the engine serves. A test refuses one action through it.
	Guard authz.Guard
}

// Reactivator is the config surface as a secret write needs it: which paths
// of the active revision read a name, and the re-activation that makes a
// running node resolve it again. configapi's Service is the one in `crewlet
// run`.
type Reactivator interface {
	// Reads reports, for each name the active revision reads, the config
	// paths that read it ([config.Company.VariablesRead]); a name nothing
	// reads is absent.
	Reads(ctx context.Context, names []string) (map[string][]string, error)
	// Reload re-publishes the active revision unchanged
	// ([configapi.Service.Reload]).
	Reload(ctx context.Context, summary string, by iam.Actor, expect string) (configapi.Applied, error)
}

// New builds the service.
//
// A MISSING FLEET OR KEYRING IS REFUSED rather than served as a narrower
// surface: `crewlet run` builds this beside an engine whose fleet store is open
// on every topology and whose Tier A carries a keyring and an active key —
// validation refuses a file without them and the engine refuses to start — so
// a nil here is a wiring mistake. An unregistered /secrets answering 404 would
// hide it, and so did the surface this was: built with no keyring, it answered
// every route 503 no_keyring, describing a deployment that cannot exist.
func New(opts Options) (*Service, error) {
	if opts.Fleet == nil {
		return nil, errors.New("secretsapi: Options.Fleet is required: the " +
			"company's credentials live in the fleet's coordination store")
	}
	if opts.Cipher == nil {
		return nil, errors.New("secretsapi: Options.Cipher is required: every " +
			"node holds the keyring (secrets.keys) a credential is sealed under")
	}
	if opts.ActiveKeyID == "" {
		return nil, errors.New("secretsapi: Options.ActiveKeyID is required: it " +
			"is the key (secrets.active_key_id) a rekey re-seals onto")
	}
	if opts.Config == nil {
		return nil, errors.New("secretsapi: Options.Config is required: a write " +
			"re-activates the revision that reads the value, or the running " +
			"company goes on signing in with the old one (ADR-0070)")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	guard := opts.Guard
	if guard == nil {
		guard = authz.ContextGuard(authz.NoChart{})
	}
	return &Service{
		store:     fleetsecrets.New(opts.Fleet, opts.Cipher),
		config:    opts.Config,
		identity:  opts.Identity,
		assistant: opts.Assistant,
		keyID:     opts.ActiveKeyID,
		now:       now,
		guard:     guard,
	}, nil
}

// Routes registers the surface on the API's mux, every route through
// [authz.Router] with the verb it is decided by. A route mounted without one
// is refused here and fails the boot — see [authz.Router.Handle].
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, s.guard)
	var failures []error
	mount := func(pattern string, a authz.Action, h http.HandlerFunc) {
		if err := router.Handle(pattern, authz.Policy{Action: a}, h); err != nil {
			failures = append(failures, err)
		}
	}
	mount("GET /secrets", authz.ActionSecretList, s.list)
	// REKEY IS A POST, and that is what keeps it from swallowing a secret
	// a company legitimately calls "rekey". Registration order is NOT what
	// separates them — the mux prefers the more specific pattern whichever
	// way round they are declared — so the method is doing the work: a
	// GET, PUT or DELETE of that name reaches the {name} routes, and no
	// spelling of a secret's name can reach the rekey handler.
	mount("POST /secrets/rekey", authz.ActionSecretWrite, s.rekey)
	// THE VALUES OF SEVERAL NAMES, for a reader resolving one company
	// document — a POST for the reason rekey is one, since no POST reaches
	// the {name} routes, and because the names it asks for travel in a body
	// rather than in a URL a proxy logs and bounds. The reveal's own verb
	// on the route itself, since there is no metadata half to admit first.
	mount("POST /secrets/reveal", authz.ActionSecretReveal, s.revealMany)
	// SEVERAL VALUES, ONE ACTIVATION: a rotation of every credential one
	// entry or integration reads, sealed together and re-activated once.
	// The collection's PUT, which no {name} route can reach.
	mount("PUT /secrets", authz.ActionSecretWrite, s.putMany)
	// THE METADATA'S VERB, and a reveal asks the value's on top — see
	// [Service.get]. The pattern cannot see `?reveal=true`.
	mount("GET /secrets/{name}", authz.ActionSecretList, s.get)
	// A PUT STORES OR ROTATES, so an empty name is created ([Service.put]).
	if err := router.Handle("PUT /secrets/{name}", authz.Policy{Action: authz.ActionSecretWrite}.
		WhenAbsent(authz.CreatesWhenAbsent()), http.HandlerFunc(s.put)); err != nil {
		failures = append(failures, err)
	}
	mount("DELETE /secrets/{name}", authz.ActionSecretWrite, s.delete)
	return errors.Join(failures...)
}

// list serves GET /secrets — every name, with no values.
//
// THE ENGINE'S OWN KEYS ARE COUNTED AND NEVER NAMED. The blind-index key is not
// the operator's to read, write or delete, and a listing of it was the first
// step to every one of those; what an operator does need is to see a rotation
// reach it, so `engine_keys` says how many there are under each keyring key and
// nothing else. `identity_values` is the same count for the identity estate's
// sealed values, by key — null where this surface was given no identity
// estate, which `crewlet run` never does: every node serving the API holds the
// estate (ingress requires `data`) and runs the identity estate from boot.
// `assistant_values` is the same count for the Dashboard Assistant's sealed
// values, which a rotation waits out, null where this surface was given none.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.List(r.Context())
	if err != nil {
		s.fail(w, "list the secrets", err)
		return
	}
	engine, err := s.store.EngineKeys(r.Context())
	if err != nil {
		s.fail(w, "count the engine's keys", err)
		return
	}
	var identity *iamdomain.SealedCount
	if s.identity != nil {
		counted, err := s.identity.SealedKeys(r.Context())
		if err != nil {
			s.fail(w, "count the identity estate's sealed values", err)
			return
		}
		identity = &counted
	}
	var assistant *assistantdomain.SealedCount
	if s.assistant != nil {
		counted, err := s.assistant.SealedKeys(r.Context())
		if err != nil {
			s.fail(w, "count the assistant's sealed values", err)
			return
		}
		assistant = &counted
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, render(row))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"secrets": out, "engine_keys": engine, "identity_values": identity,
		"assistant_values": assistant,
	})
}

// reserved refuses a name in the engine's own namespace, and says what to do
// instead. Every route that takes a name asks it FIRST — before the verb a
// reveal asks and before a body is read — because no caller of this surface,
// whatever it holds, may address one.
func reserved(w http.ResponseWriter, name string) bool {
	if !secrets.Reserved(name) {
		return false
	}
	httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeReservedName, map[string]string{
		"detail": secrets.ErrReservedName.Error(),
		"hint": "A rekey moves it with everything else (crewlet secrets " +
			"rekey).",
	})
	return true
}

// get serves GET /secrets/{name} — metadata, or the value with ?reveal=true.
//
// THE VALUE NEEDS THE FLAG. Without it this answers what a listing answers
// for one name, which is the overwhelmingly common question ("is X set, and
// when did it change") and does not put a credential into a browser's history
// or a proxy's access log.
func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidName,
			map[string]string{"detail": "the path names no secret"})
		return
	}
	if reserved(w, name) {
		return
	}
	if r.URL.Query().Get("reveal") != "true" {
		row, found, err := s.store.Describe(r.Context(), name)
		switch {
		case err != nil:
			s.fail(w, "read the secret", err)
		case !found:
			httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		default:
			httpjson.Write(w, http.StatusOK, render(row))
		}
		return
	}
	// THE VALUE IS ITS OWN VERB. The route admitted a caller who may see
	// that the credential exists; revealing it is `secrets.reveal` — a
	// person present and a recent proof on top of the listing's admin —
	// asked here because the pattern cannot see the flag, and asked BEFORE
	// the store is read, so a refused caller never has the value in flight.
	if !authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionSecretReveal}) {
		return
	}
	value, err := s.store.Get(r.Context(), name)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	case err != nil:
		s.fail(w, "open the secret", err)
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	by := iam.ActorFor(caller)
	log.WarnContext(r.Context(), "secret_revealed", "name", name,
		"by", by.Name, "operator", by.OperatorID)
	// NO-STORE, and it is not decoration: without it a value can sit in a
	// shared proxy's cache, which is a credential leak with no log line
	// anywhere and no way to find it afterwards.
	w.Header().Set("Cache-Control", "no-store")
	httpjson.Write(w, http.StatusOK, map[string]any{"name": name, "value": value})
}

// revealRequest is the body POST /secrets/reveal reads.
type revealRequest struct {
	Names []string `json:"names"`
}

// revealMany serves POST /secrets/reveal — the values of the names a body
// asks for, in one answer: `values` for every name held, `missing` for every
// name nothing holds.
//
// IT IS THE BREAK-GLASS READ, AND NOTHING LESS. A CLI run resolving a company
// document through a running node (`crewlet gitlab provision` and its
// siblings, `crewlet llm`) reads the variables its command reads that the
// node lists, and asking for them through `GET /secrets/{name}?reveal=true`
// one at a time would make one decision, one log line and one round trip per
// credential where the run has one question. So it is mounted on
// `secrets.reveal` itself — admin, a person present, a recent proof — decided
// before the body is read, and the access is logged by name like the single
// reveal. It answers exactly the names asked, never the whole store: a run is
// handed the values it asked for and nothing beside them.
//
// A NAME OUTSIDE THE REFERENCE GRAMMAR IS REFUSED rather than answered
// missing, unlike the single read: the one reader of this route asks only for
// names the listing gave it, which every store holds to the grammar, so a name
// no store can hold is a caller asking for something other than a command's
// values — and keeping every name a plain identifier is what bounds the answer
// at [MaxRevealAnswerBytes]. A reserved name is refused first, as on every route
// here.
func (s *Service) revealMany(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r, "POST /secrets/reveal",
		"send the variables to read as the body, {\"names\": [...]}, and "+
			"nothing in the query") {
		return
	}
	raw, err := httpjson.ReadBody(w, r, MaxRevealBodyBytes)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var req revealRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&req); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "the body is not {\"names\": [...]}: " + err.Error(),
			"hint":   "send the variables to read as a JSON list under names",
		})
		return
	}
	names := slices.Compact(slices.Sorted(slices.Values(req.Names)))
	switch {
	case len(names) == 0:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "names is empty",
			"hint":   "name at least one variable to read",
		})
		return
	case len(names) > MaxRevealNames:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": strconv.Itoa(len(names)) + " names, and one request reads at most " +
				strconv.Itoa(MaxRevealNames),
			"hint": "split the names into requests of at most " +
				strconv.Itoa(MaxRevealNames),
		})
		return
	}
	for _, name := range names {
		if reserved(w, name) {
			return
		}
		if err = secrets.CheckName(name); err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidName, map[string]string{
				"detail": err.Error(),
				"hint": "this route reads the variables a ${VAR} reference can " +
					"name, and nothing else",
			})
			return
		}
	}
	values, err := s.store.Values(r.Context(), names)
	if err != nil {
		s.fail(w, "open the secrets", err)
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	missing := make([]string, 0, len(names)-len(values))
	for _, name := range names {
		if _, held := values[name]; !held {
			missing = append(missing, name)
		}
	}
	by := iam.ActorFor(caller)
	// EVERY NAME, the ones nothing holds included: a read that asked for a
	// credential is the access whether or not one was there.
	log.WarnContext(r.Context(), "secrets_revealed", "names", names,
		"by", by.Name, "operator", by.OperatorID)
	w.Header().Set("Cache-Control", "no-store")
	httpjson.Write(w, http.StatusOK, map[string]any{"values": values, "missing": missing})
}

// put serves PUT /secrets/{name} — store or rotate one value.
//
// THE BODY IS THE VALUE, raw bytes, not a JSON wrapper. A credential is
// arbitrary text — a PEM key has newlines, a token can be anything — and
// making the caller escape it into JSON puts an encoding step between the
// operator and the byte sequence the vendor will check.
//
// NOTHING ELSE IS THE REQUEST'S TO SAY. The row's author, kind and credential
// are the caller the guard authenticated, and its source is this route's own,
// [secrets.SourceAPI], whoever calls it — see [secrets.Source] for what a
// source the caller named cost.
func (s *Service) put(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if reserved(w, name) {
		return
	}
	// BEFORE THE BODY, so a name the grammar cannot reference is refused
	// without moving 64 KiB of credential through the process first. Only
	// the write checks: a name outside the grammar names no row, so a read
	// of one answers not_found and a delete answers removed:false — and
	// the engine's own names are refused reserved_name before either,
	// since no route here addresses them at all.
	if err := secrets.CheckName(name); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidName, map[string]string{
			"detail": err.Error(),
			"hint": "a secret is keyed by environment-variable name, because " +
				"that is what a ${VAR} in the company config resolves through",
		})
		return
	}
	if !noQuery(w, r, "PUT /secrets/{name}",
		"who stored a value is the credential this request authenticated "+
			"with (updated_by, updated_by_kind, operator_id), and its source "+
			"is this route's own, "+string(secrets.SourceAPI)+"; send the "+
			"value as the body and nothing in the query") {
		return
	}
	// THE SHARED READER, not a copy of it. This route answered
	// `value_too_large` where configapi answered `body_too_large` for the
	// same 413, and its own reader is also the one place a body arrived
	// with no time bound on it — see [httpjson.BodyReadTimeout].
	body, err := httpjson.ReadBody(w, r, secrets.MaxValueBytes)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	written, ok := s.write(w, r, map[string]string{name: string(body)}, true)
	if !ok {
		return
	}
	by := written.by
	// THE PROVENANCE THE ROW RECORDS, in the answer: a client has no say in
	// any of it, so the only honest confirmation is the node's. And what the
	// write did to the running company: the paths that read the name, and
	// the re-activation that made them read the new value (ADR-0070).
	httpjson.Write(w, http.StatusOK, map[string]any{
		"name": name, "bytes": len(body), "key_id": s.keyID,
		"updated_by": by.Name, "updated_by_kind": string(by.Kind),
		"operator_id": by.OperatorID, "source": secrets.SourceAPI,
		"read_by": nonNil(written.readBy[name]), "reload": written.reload,
	})
}

// MaxBatchBodyBytes bounds a `PUT /secrets` body: [MaxRevealNames] names of a
// kilobyte each, beside a value of up to [MaxJSONValueBytes] each (the
// longest a value is once spelled as a JSON string) inside the envelope. The
// reveal answer's bound, because the two are the same set of pairs travelling
// in opposite directions.
const MaxBatchBodyBytes = MaxRevealAnswerBytes

// batchRequest is the body `PUT /secrets` reads.
type batchRequest struct {
	Values map[string]string `json:"values"`
	// DeferReload seals the values and re-activates nothing, for a writer
	// that seals several values one at a time and re-activates once at its
	// end: a provisioning run, whose every minted credential has to be
	// durable the moment it exists (provision.TokenSink) and whose last
	// write re-activates for all of them. The answer's `reload` is
	// `{"deferred": true}`, and `read_by` says what the later re-activation
	// will reach. A writer that never re-activates (a process that died)
	// leaves the values sealed and inactive until the next re-activation.
	DeferReload bool `json:"defer_reload"`
}

// putMany serves PUT /secrets: store or rotate several values, re-activating
// the revision that reads them ONCE.
//
// ALL OR NOTHING AT THE DOOR. Every name is held to the reference grammar and
// every value to the store's bound before anything is sealed, and a fault in
// any refuses the whole body naming each one, because a rotation of four
// credentials that sealed three is a company signing in with a mix of the old
// and the new one, the state a batch exists to avoid.
func (s *Service) putMany(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r, "PUT /secrets",
		"send the values as the body, {\"values\": {\"NAME\": \"value\"}}, and "+
			"nothing in the query") {
		return
	}
	raw, err := httpjson.ReadBody(w, r, MaxBatchBodyBytes)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var req batchRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&req); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "the body is not {\"values\": {...}}: " + err.Error(),
			"hint":   "send each variable's value under values, keyed by its name",
		})
		return
	}
	switch {
	case len(req.Values) == 0:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "values is empty",
			"hint":   "name at least one variable to store",
		})
		return
	case len(req.Values) > MaxRevealNames:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": strconv.Itoa(len(req.Values)) + " values, and one request stores at most " +
				strconv.Itoa(MaxRevealNames),
			"hint": "split the values into requests of at most " + strconv.Itoa(MaxRevealNames),
		})
		return
	}
	names := slices.Sorted(maps.Keys(req.Values))
	for _, name := range names {
		if reserved(w, name) {
			return
		}
	}
	invalid := map[string]string{}
	for _, name := range names {
		if err := secrets.CheckName(name); err != nil {
			invalid[name] = err.Error()
		} else if err := secrets.CheckValue(req.Values[name]); err != nil {
			invalid[name] = err.Error()
		}
	}
	if len(invalid) > 0 {
		bad := slices.Sorted(maps.Keys(invalid))
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody, httpjson.Detail{
			"detail":  strings.Join(bad, ", ") + " cannot be stored, so nothing was",
			"invalid": invalid,
			"hint": "a secret is keyed by environment-variable name and holds at most " +
				strconv.Itoa(secrets.MaxValueBytes>>10) + " KiB; correct each one named " +
				"and send the whole body again",
		})
		return
	}
	written, ok := s.write(w, r, req.Values, !req.DeferReload)
	if !ok {
		return
	}
	sizes := make(map[string]int, len(names))
	readBy := make(map[string][]string, len(names))
	for _, name := range names {
		sizes[name] = len(req.Values[name])
		readBy[name] = nonNil(written.readBy[name])
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"names": names, "bytes": sizes, "read_by": readBy, "reload": written.reload,
	})
}

// written is what [Service.write] did: who it wrote as, the paths of the
// active revision reading each name, and the re-activation that followed, nil
// when nothing reads any of them.
type written struct {
	by     iam.Actor
	readBy map[string][]string
	reload *configapi.Reloaded
}

// write seals every value and re-activates the revision that reads any of them
// once, answering the refusal itself; ok is false when the request has been
// answered. The names and values are already held to the grammar and the
// bound.
//
// IN THIS ORDER: which paths read the names, then `config.write` when a
// reload will follow (asked of the same guard the route was, BEFORE anything
// is sealed, so a caller who may store a credential and may not re-activate the
// company leaves nothing behind), then every seal, then the one reload.
//
// reactivates is false for a write whose caller re-activates at its own end
// ([batchRequest.DeferReload]): nothing is re-activated, and `config.write`
// is asked of the write that does.
func (s *Service) write(w http.ResponseWriter, r *http.Request, values map[string]string,
	reactivates bool) (written, bool) {
	ctx := r.Context()
	names := slices.Sorted(maps.Keys(values))
	readBy, err := s.config.Reads(ctx, names)
	if err != nil {
		s.fail(w, "read which paths read the secret", err)
		return written{}, false
	}
	if reactivates && len(readBy) > 0 &&
		!authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionConfigWrite}) {
		return written{}, false
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return written{}, false
	}
	by := iam.ActorFor(caller)
	// A DEFERRED WRITE RE-ACTIVATES NOTHING, its failure included: the
	// caller's last write does, for every name it sealed.
	reactivate := func(names []string) *configapi.Reloaded {
		if !reactivates {
			// SAID PLAINLY, never a null a reader could take for "nothing
			// reads it": the value is sealed and not active yet.
			return &configapi.Reloaded{Deferred: true}
		}
		return s.reactivate(ctx, names, readBy, by, "written")
	}
	author := secrets.Author{Name: by.Name, Kind: string(by.Kind), OperatorID: by.OperatorID}
	var sealed []string
	for _, name := range names {
		// THE AUTHOR AND THE CREDENTIAL BOTH, on the row: a row that named
		// only the credential named, for a person's machine token,
		// something the identity sweep deletes a week after it lapses.
		if err := s.store.Set(ctx, name, values[name], author, secrets.SourceAPI, s.now()); err != nil {
			// WHAT DID LAND STILL REACHES THE COMPANY: a row sealed and
			// left unread is the trap this surface closes, so the names
			// sealed before the failure are re-activated and named.
			log.ErrorContext(ctx, "secret_write_failed", "name", name, "sealed", sealed,
				"error", err, "by", by.Name, "operator", by.OperatorID)
			httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeInternalError,
				httpjson.Detail{
					"sealed": nonNil(sealed),
					"reload": reactivate(sealed),
					"hint": "the values named under sealed are stored; send the others " +
						"again",
				})
			return written{}, false
		}
		sealed = append(sealed, name)
		// The NAME and the byte count. Confirming the value would undo the
		// reason it was sent as a body in the first place. No source: it
		// is this route's constant, so the line says nothing by carrying it.
		log.InfoContext(ctx, "secret_written", "name", name,
			"bytes", len(values[name]), "by", by.Name, "operator", by.OperatorID)
	}
	return written{by: by, readBy: readBy, reload: reactivate(sealed)}, true
}

// reactivate re-activates the active revision once, when it reads any of
// names, and says what that did; nil when it reads none of them, which is the
// `"reload": null` an answer carries.
func (s *Service) reactivate(ctx context.Context, names []string, readBy map[string][]string,
	by iam.Actor, what string) *configapi.Reloaded {

	var read []string
	for _, name := range names {
		if len(readBy[name]) > 0 {
			read = append(read, name)
		}
	}
	if len(read) == 0 {
		return nil
	}
	summary := "reload: secret " + read[0] + " " + what
	if len(read) > 1 {
		summary = "reload: secrets " + strings.Join(read, ", ") + " " + what
	}
	applied, err := s.config.Reload(ctx, summary, by, "")
	outcome := configapi.ReloadOutcome(applied, err)
	if err != nil {
		log.WarnContext(ctx, "secret_reload_failed", "names", read, "error", err,
			"by", by.Name, "operator", by.OperatorID,
			"detail", "the values are sealed; POST /config/reload re-activates the company")
	} else {
		log.InfoContext(ctx, "secret_reloaded", "names", read, "revision", applied.RevisionID,
			"epoch", applied.Epoch, "by", by.Name, "operator", by.OperatorID)
	}
	return outcome
}

// noQuery refuses a request to a route that takes no query string, naming its
// parameters and the route, and reports whether the request may go on. hint
// says what the route reads instead.
//
// REFUSED, NEVER IGNORED. `?source=` once chose the word a row recorded, and a
// client still sending it would otherwise be told its write landed while the
// row says something else — a client that judged its own rows by that word
// would read every one as overwritten and write it again, for ever, with
// nothing failing. So any parameter is a 400 before the body is read, and the
// answer says what the route reads instead.
func noQuery(w http.ResponseWriter, r *http.Request, route, hint string) bool {
	if r.URL.RawQuery == "" {
		return true
	}
	names := slices.Sorted(maps.Keys(r.URL.Query()))
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, strconv.Quote(name))
	}
	// A query that parses to no name at all (`?&`) is still a query, and is
	// refused in general terms rather than by a name it does not have.
	detail := route + " takes no query parameters"
	switch {
	case len(quoted) == 1:
		detail = quoted[0] + " is not a parameter of " + route + ", which takes none"
	case len(quoted) > 1:
		detail = strings.Join(quoted, ", ") + " are not parameters of " +
			route + ", which takes none"
	}
	httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery, map[string]string{
		"detail": detail, "hint": hint,
	})
	return false
}

// delete serves DELETE /secrets/{name}.
//
// 200 EITHER WAY, with `removed` saying which happened. A 404 for a name that
// was already gone would make a cleanup script fail on its second run, and
// "it was not there" is the outcome the caller wanted rather than an error.
func (s *Service) delete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidName,
			map[string]string{"detail": "the path names no secret"})
		return
	}
	if reserved(w, name) {
		return
	}
	// A REMOVAL OF A NAME THE COMPANY READS IS A CHANGE TO THE COMPANY too:
	// whatever read it falls back to its convention or signs out, and the
	// running company learns that only at a re-activation. Asked before the
	// row goes, as a write asks.
	readBy, err := s.config.Reads(r.Context(), []string{name})
	if err != nil {
		s.fail(w, "read which paths read the secret", err)
		return
	}
	if len(readBy) > 0 && !authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionConfigWrite}) {
		return
	}
	removed, err := s.store.Unset(r.Context(), name)
	if err != nil {
		s.fail(w, "remove the secret", err)
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	by := iam.ActorFor(caller)
	log.InfoContext(r.Context(), "secret_removed", "name", name,
		"removed", removed, "by", by.Name, "operator", by.OperatorID)
	var reload *configapi.Reloaded
	if removed {
		reload = s.reactivate(r.Context(), []string{name}, readBy, by, "removed")
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"name": name, "removed": removed, "read_by": nonNil(readBy[name]), "reload": reload,
	})
}

// rekey serves POST /secrets/rekey — re-seal every stale row, and every person's
// values in the identity estate.
//
// THE NAMES, not a count: a pass that moved 12 of 13 rows raises a question a
// number cannot answer, and this is what an operator reads before retiring
// the old key. The engine's key and the identity estate's values are counted,
// for the listing's reason.
//
// THE STORE FIRST, THEN THE ESTATE, and a failure in either is the one
// `rekey_incomplete` carrying what did move: both halves are their own retry,
// so running the gesture again finishes it.
func (s *Service) rekey(w http.ResponseWriter, r *http.Request) {
	// THE CALLER'S EXPECTED KEY, refused on a mismatch rather than
	// ignored. A CLI whose Tier A names a different active key than this
	// node's is an operator rekeying onto a key the fleet will not seal
	// with — and a silent success there reports a completed rotation over
	// rows sealed under something else, which is exactly the state they
	// are about to retire the old key on the strength of.
	if want := r.URL.Query().Get("key_id"); want != "" && want != s.keyID {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeKeyIDMismatch, map[string]string{
			"key_id": s.keyID, "your_key_id": want,
			"hint": "this node seals under its own secrets.active_key_id; make " +
				"the two configs agree before rekeying",
		})
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	// WHO ROTATED is this gesture's own fact, and it is logged here: the
	// rows keep who stored each value, which a rekey did not choose.
	by := iam.ActorFor(caller)
	rekeyed, err := s.store.Rekey(r.Context(), s.keyID)
	if err != nil {
		// THE NAMES THAT DID MOVE travel with the refusal. A partial
		// rekey is a fact an operator has to act on, and a bare 500
		// would leave them re-running a pass with no idea which rows
		// are already under the new key.
		log.ErrorContext(r.Context(), "secret_rekey_failed", "error", err,
			"moved", rekeyed.Moved, "engine_keys_moved", rekeyed.EngineKeys,
			"by", by.Name, "operator", by.OperatorID)
		httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeRekeyIncomplete, httpjson.Detail{
			"moved":             nonNil(rekeyed.Moved),
			"engine_keys_moved": rekeyed.EngineKeys,
			"hint": "a row could not be opened with this node's keyring; the " +
				"key that sealed it is missing from secrets.keys",
		})
		return
	}
	var identity *IdentityResealed
	if s.identity != nil {
		report, err := s.identity.Reseal(r.Context())
		identity = &IdentityResealed{People: len(report.People), Values: report.Values}
		if err != nil || len(report.Unknown) > 0 {
			// UNKNOWN IS INCOMPLETE TOO: a record nobody could confirm may
			// not have landed, and the operator is about to drop the key
			// its value is under on the strength of this answer.
			log.ErrorContext(r.Context(), "secret_rekey_identity_incomplete",
				"error", errText(err), "moved", rekeyed.Moved,
				"engine_keys_moved", rekeyed.EngineKeys,
				"identity_people", identity.People, "identity_values", identity.Values,
				"identity_unknown", len(report.Unknown),
				"by", by.Name, "operator", by.OperatorID)
			httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeRekeyIncomplete, httpjson.Detail{
				"moved":             nonNil(rekeyed.Moved),
				"engine_keys_moved": rekeyed.EngineKeys,
				"identity":          identity,
				"identity_unknown":  len(report.Unknown),
				"hint": "Some people's details weren't moved to the new key. " +
					"Run the rekey again.",
			})
			return
		}
	}
	log.InfoContext(r.Context(), "secrets_rekeyed", "moved", rekeyed.Moved,
		"engine_keys_moved", rekeyed.EngineKeys, "identity", identity,
		"key_id", s.keyID, "by", by.Name, "operator", by.OperatorID)
	// THE ENGINE'S KEYS AND THE IDENTITY ESTATE'S VALUES AS COUNTS beside the
	// operator's names: the operator retiring the old key needs to know they
	// moved, and nothing more — and null where this node runs no identity
	// estate, which is a different answer from "nothing to move".
	httpjson.Write(w, http.StatusOK, map[string]any{
		"key_id": s.keyID, "moved": nonNil(rekeyed.Moved),
		"engine_keys_moved": rekeyed.EngineKeys, "identity": identity,
	})
}

// errText is an error as a log attribute, or empty for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// nonNil is a list as JSON renders it for a reader that ranges over it: an
// empty array, never null.
func nonNil(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

// render is one row as the surface reports it — never its value.
func render(row secrets.Record) map[string]any {
	return map[string]any{
		"name":       row.Name,
		"key_id":     row.KeyID,
		"updated_at": row.UpdatedAt.Format(time.RFC3339Nano),
		"updated_by": row.UpdatedBy,
		// THE KIND AND THE CREDENTIAL BESIDE THE AUTHOR, the credential
		// empty on a row no credential wrote: the engine's own, and one a
		// `crewlet` command wrote directly with its node's engine stopped.
		"updated_by_kind": row.UpdatedByKind,
		"operator_id":     row.OperatorID,
		"source":          row.Source,
	}
}

// fail logs the reason and answers without it.
//
// The reason reaches the LOG, never the caller: a coordination error can
// carry a bucket name and a broker address, and an error from the cipher can
// name a key id. None of that belongs in a response body on this surface.
func (s *Service) fail(w http.ResponseWriter, what string, err error) {
	log.Error("secret_request_failed", "what", what, "error", err)
	httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
}
