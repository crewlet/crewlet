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
// # Every route takes a grant, reads included
//
// Each is mounted through [authz.Router] with the verb the authority table
// decides it by: the listing and one row's metadata are `secrets.list`
// (`config:read`), revealing a value is `secrets.reveal` (`secrets:read`), and
// storing, rotating, deleting and re-keying are `secrets.write`
// (`secrets:write`). A listing says which credentials a company holds and when
// each last changed, which is reconnaissance even without the values.
//
// THE ROUTES USED TO DECIDE NOTHING. They resolved the caller for the audit
// line and asked no grant, which was sound while the only credential this
// engine had was an operator token: "somebody resolved" meant "the operator".
// It stopped being sound the day a person could sign in holding `state:read`
// alone — every one of them could then read, reveal, overwrite and delete the
// company's credentials. The split between `secrets:read` and `secrets:write`
// the grant vocabulary draws, so an automation that reseals keys can hold the
// write and never the read, meant nothing while no route asked either.
//
// # The engine's own key material is not reachable from here at all
//
// The same bucket holds a person's data key, each OIDC session's refresh
// token and the company's blind-index keys, under names in the engine's own
// namespace ([secrets.Reserved]). Every route that takes a name refuses one of
// those with `403 reserved_name` before anything else — whatever the caller
// holds — and the listing counts them per keyring key without naming any. They
// were once ordinary names here, and that made a reveal a way to copy a
// person's key before their removal shredded it, and a DELETE a removal nobody
// recorded.
//
// # There is exactly one route that returns a value, and it is break-glass
//
// It requires an explicit ?reveal=true — a path that cannot be reached by
// accident or by a crawl — and it logs the access by operator and name. A
// read-back that leaves no trace is indistinguishable from an exfiltration,
// and the name is the whole of what can be logged, because logging the value
// would be the leak.
package secretsapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
)

var log = logging.Get("api.secrets")

// MaxValueBytes bounds one secret's value.
//
// The largest real credential this engine handles is a service-account JSON
// key or a PEM private key, which are a few kilobytes; 64 KiB is an order of
// magnitude above that and still finite. The route is guarded, so this bounds
// a mistake rather than an attacker — a `curl -d @big.tar` typo should be
// refused rather than sealed.
const MaxValueBytes = 64 << 10

// Service is the /secrets surface.
type Service struct {
	store  *fleetsecrets.Store
	keyID  string
	cipher secrets.Cipher
	now    func() time.Time

	// guard decides every route and the reveal a GET asks for on top of
	// its route. No chart: not one verb here asks a relation.
	guard authz.Guard
}

// Options wire the service.
type Options struct {
	// Fleet is the coordination backend holding the rows. Required: every
	// node opens the fleet store, and [New] refuses to build without it.
	Fleet coord.Secrets

	// Cipher seals and opens a value. Nil is a node with no keyring,
	// which every route refuses rather than storing plaintext.
	Cipher secrets.Cipher

	// ActiveKeyID is the keyring key a rekey re-seals onto.
	ActiveKeyID string

	// Now is injectable so a test can pin a row's timestamp.
	Now func() time.Time
}

// New builds the service.
//
// A MISSING FLEET IS REFUSED rather than served as an absent surface: `crewlet
// run` builds this beside an engine whose fleet store is open on every
// topology, so a nil here is a wiring mistake, and an unregistered /secrets
// answering 404 would hide it.
func New(opts Options) (*Service, error) {
	if opts.Fleet == nil {
		return nil, errors.New("secretsapi: Options.Fleet is required: the " +
			"company's credentials live in the fleet's coordination store")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Cipher == nil {
		// SAID AT REGISTRATION. Every route then answers 503, and a 503
		// with no explanation anywhere is a support ticket: a node with
		// no secrets.keys genuinely cannot hold a credential, and the
		// operator's answer is `crewlet secrets keygen`.
		log.Warn("secret_routes_disabled",
			"hint", "this node has no secrets.keys, so /secrets cannot seal "+
				"anything; run `crewlet secrets keygen` and install one")
	}
	return &Service{
		store:  fleetsecrets.New(opts.Fleet, opts.Cipher),
		keyID:  opts.ActiveKeyID,
		cipher: opts.Cipher,
		now:    now,
		guard:  authz.ContextGuard(authz.NoChart{}),
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
	// THE METADATA'S VERB, and a reveal asks the value's on top — see
	// [Service.get]. The pattern cannot see `?reveal=true`.
	mount("GET /secrets/{name}", authz.ActionSecretList, s.get)
	mount("PUT /secrets/{name}", authz.ActionSecretWrite, s.put)
	mount("DELETE /secrets/{name}", authz.ActionSecretWrite, s.delete)
	return errors.Join(failures...)
}

// list serves GET /secrets — every name, with no values.
//
// THE ENGINE'S OWN KEYS ARE COUNTED AND NEVER NAMED. A person's data key and a
// session's refresh token are not the operator's to read, write or delete, and
// a listing of them was the first step to every one of those; what an operator
// does need is to see a rotation reach them, so `engine_keys` says how many
// there are under each keyring key and nothing else.
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
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, render(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secrets": out, "engine_keys": engine,
	})
}

// reserved refuses a name in the engine's own namespace, and says what to do
// instead. Every route that takes a name asks it FIRST — before the grant a
// reveal asks and before a body is read — because no caller of this surface,
// whatever it holds, may address one.
func reserved(w http.ResponseWriter, name string) bool {
	if !secrets.Reserved(name) {
		return false
	}
	httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeReservedName, map[string]string{
		"detail": secrets.ErrReservedName.Error(),
		"hint": "a person's key and a session's refresh token belong to the " +
			"identity estate: remove a person with `crewlet iam remove`, end a " +
			"session with `crewlet iam revoke`; a rekey moves these keys with " +
			"everything else",
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
			writeJSON(w, http.StatusOK, render(row))
		}
		return
	}
	// THE VALUE IS ITS OWN GRANT. The route admitted a caller who may see
	// that the credential exists; revealing it is `secrets:read`, asked
	// here because the pattern cannot see the flag — and asked BEFORE the
	// store is read, so a refused caller never has the value in flight.
	if !authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionSecretReveal}) {
		return
	}
	if !s.sealed(w) {
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
	operator := auth.OperatorID(caller)
	log.WarnContext(r.Context(), "secret_revealed", "name", name, "operator", operator)
	// NO-STORE, and it is not decoration: without it a value can sit in a
	// shared proxy's cache, which is a credential leak with no log line
	// anywhere and no way to find it afterwards.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "value": value})
}

// put serves PUT /secrets/{name} — store or rotate one value.
//
// THE BODY IS THE VALUE, raw bytes, not a JSON wrapper. A credential is
// arbitrary text — a PEM key has newlines, a token can be anything — and
// making the caller escape it into JSON puts an encoding step between the
// operator and the byte sequence the vendor will check.
func (s *Service) put(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if reserved(w, name) {
		return
	}
	// BEFORE THE BODY, so a name the grammar cannot reference is refused
	// without moving 64 KiB of credential through the process first. Only
	// the write checks: an out-of-grammar row that already exists must
	// stay readable and, above all, removable, so get and delete take the
	// name as given — every name but the engine's own, which no route here
	// addresses at all.
	if err := secrets.CheckName(name); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidName, map[string]string{
			"detail": err.Error(),
			"hint": "a secret is keyed by environment-variable name, because " +
				"that is what a ${VAR} in the company config resolves through",
		})
		return
	}
	if !s.sealed(w) {
		return
	}
	// THE SHARED READER, not a copy of it. This route answered
	// `value_too_large` where configapi answered `body_too_large` for the
	// same 413, and its own reader is also the one place a body arrived
	// with no time bound on it — see [httpjson.BodyReadTimeout].
	body, err := httpjson.ReadBody(w, r, MaxValueBytes)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	operator := auth.OperatorID(caller)
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "api"
	}
	if err := s.store.Set(r.Context(), name, string(body), operator, source, s.now()); err != nil {
		s.fail(w, "store the secret", err)
		return
	}
	// The NAME and the byte count. Confirming the value would undo the
	// reason it was sent as a body in the first place.
	log.InfoContext(r.Context(), "secret_written", "name", name,
		"bytes", len(body), "operator", operator, "source", source)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "bytes": len(body), "key_id": s.keyID,
	})
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
	removed, err := s.store.Unset(r.Context(), name)
	if err != nil {
		s.fail(w, "remove the secret", err)
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	operator := auth.OperatorID(caller)
	log.InfoContext(r.Context(), "secret_removed", "name", name,
		"removed", removed, "operator", operator)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "removed": removed})
}

// rekey serves POST /secrets/rekey — re-seal every stale row.
//
// THE NAMES, not a count: a pass that moved 12 of 13 rows raises a question a
// number cannot answer, and this is what an operator reads before retiring
// the old key.
func (s *Service) rekey(w http.ResponseWriter, r *http.Request) {
	if !s.sealed(w) {
		return
	}
	if s.keyID == "" {
		// A 503 WITH NO Retry-After: this node lacks the configuration,
		// and no wait gives it one — see [httpjson.Unavailable].
		httpjson.UnavailableWith(w, httpjson.CodeNoActiveKey, 0, httpjson.Detail{
			"hint": "this node's secrets.active_key_id is unset, so there is " +
				"no key to re-seal onto",
		})
		return
	}
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
	operator := auth.OperatorID(caller)
	rekeyed, err := s.store.Rekey(r.Context(), s.keyID, operator, s.now())
	if err != nil {
		// THE NAMES THAT DID MOVE travel with the refusal. A partial
		// rekey is a fact an operator has to act on, and a bare 500
		// would leave them re-running a pass with no idea which rows
		// are already under the new key.
		log.ErrorContext(r.Context(), "secret_rekey_failed", "error", err,
			"moved", rekeyed.Moved, "engine_keys_moved", rekeyed.EngineKeys,
			"operator", operator)
		httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeRekeyIncomplete, httpjson.Detail{
			"moved":             nonNil(rekeyed.Moved),
			"engine_keys_moved": rekeyed.EngineKeys,
			"hint": "a row could not be opened with this node's keyring; the " +
				"key that sealed it is missing from secrets.keys",
		})
		return
	}
	log.InfoContext(r.Context(), "secrets_rekeyed", "moved", rekeyed.Moved,
		"engine_keys_moved", rekeyed.EngineKeys, "key_id", s.keyID,
		"operator", operator)
	// THE ENGINE'S KEYS AS A COUNT beside the operator's names: the operator
	// retiring the old key needs to know they moved, and nothing more.
	writeJSON(w, http.StatusOK, map[string]any{
		"key_id": s.keyID, "moved": nonNil(rekeyed.Moved),
		"engine_keys_moved": rekeyed.EngineKeys,
	})
}

// nonNil is a list as JSON renders it for a reader that ranges over it: an
// empty array, never null.
func nonNil(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

// sealed refuses a write on a node with no keyring, and says what to do.
func (s *Service) sealed(w http.ResponseWriter) bool {
	if s.cipher != nil {
		return true
	}
	// A 503 WITH NO Retry-After: waiting installs no key — see
	// [httpjson.CodeNoKeyring].
	httpjson.UnavailableWith(w, httpjson.CodeNoKeyring, 0, httpjson.Detail{
		"detail": "this node has no secrets.keys, so it cannot seal or open a " +
			"secret",
		"hint": "run `crewlet secrets keygen` and install the key in Tier A",
	})
	return false
}

// render is one row as the surface reports it — never its value.
func render(row secrets.Record) map[string]any {
	return map[string]any{
		"name":       row.Name,
		"key_id":     row.KeyID,
		"updated_at": row.UpdatedAt.Format(time.RFC3339Nano),
		"updated_by": row.UpdatedBy,
		"source":     row.Source,
	}
}

// fail logs the reason and answers without it.
//
// The reason reaches the LOG, never the caller: a coordination error can
// carry a bucket name and a broker address, and an error from the cipher can
// name a key id. None of that belongs in a response body on this surface.
func (s *Service) fail(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, secrets.ErrNoKeyring) {
		s.sealed(w)
		return
	}
	log.Error("secret_request_failed", "what", what, "error", err)
	httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
}

// writeJSON is [httpjson.Write] under this package's own name.
func writeJSON(w http.ResponseWriter, status int, body any) {
	httpjson.Write(w, status, body)
}
