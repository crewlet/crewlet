// Package secretsapi serves /secrets: the fleet's credential store, written
// through the one process that can reach it.
//
// # Why the CLI cannot just write the KV itself
//
// On the default topology the coordination broker runs INSIDE the engine's
// process and does not listen on a socket at all, so a second process has no
// way to reach the bucket. That is the whole reason this surface exists: the
// rows are fleet-wide (adrs/203), and the engine is the only thing that
// can put one there.
//
// # Every route is guarded, reads included
//
// [github.com/crewlet/crewlet/internal/api/auth] names /secrets alongside
// /config as a prefix that is never eligible for allow_anonymous_read. A
// listing here says which credentials a company holds and when each last
// changed, which is reconnaissance even without the values.
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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
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
}

// Options wire the service.
type Options struct {
	// Fleet is the coordination backend holding the rows. Nil serves no
	// surface at all: a process that cannot reach the fleet's store has
	// nothing to serve, and 404 is the honest answer for that.
	Fleet coord.Secrets

	// Cipher seals and opens a value. Nil is a node with no keyring,
	// which every route refuses rather than storing plaintext.
	Cipher secrets.Cipher

	// ActiveKeyID is the keyring key a rekey re-seals onto.
	ActiveKeyID string

	// Now is injectable so a test can pin a row's timestamp.
	Now func() time.Time
}

// New builds the service, or nil when there is no fleet store to serve from.
func New(opts Options) *Service {
	if opts.Fleet == nil {
		return nil
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
	}
}

// Routes registers the surface on the API's mux.
func (s *Service) Routes(mux *http.ServeMux) {
	if s == nil {
		log.Warn("secret_surface_disabled",
			"hint", "this process cannot reach the fleet's coordination store, "+
				"so /secrets is not served here")
		return
	}
	mux.HandleFunc("GET /secrets", s.list)
	// REKEY IS A POST, and that is what keeps it from swallowing a secret
	// a company legitimately calls "rekey". Registration order is NOT what
	// separates them — the mux prefers the more specific pattern whichever
	// way round they are declared — so the method is doing the work: a
	// GET, PUT or DELETE of that name reaches the {name} routes, and no
	// spelling of a secret's name can reach the rekey handler.
	mux.HandleFunc("POST /secrets/rekey", s.rekey)
	mux.HandleFunc("GET /secrets/{name}", s.get)
	mux.HandleFunc("PUT /secrets/{name}", s.put)
	mux.HandleFunc("DELETE /secrets/{name}", s.delete)
}

// list serves GET /secrets — every name, with no values.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.List(r.Context())
	if err != nil {
		s.fail(w, "list the secrets", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, render(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_name"})
		return
	}
	if r.URL.Query().Get("reveal") != "true" {
		row, found, err := s.store.Describe(r.Context(), name)
		switch {
		case err != nil:
			s.fail(w, "read the secret", err)
		case !found:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		default:
			writeJSON(w, http.StatusOK, render(row))
		}
		return
	}
	if !s.sealed(w) {
		return
	}
	value, err := s.store.Get(r.Context(), name)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	case err != nil:
		s.fail(w, "open the secret", err)
		return
	}
	operator, _ := auth.OperatorFrom(r.Context())
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
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_name"})
		return
	}
	if !s.sealed(w) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxValueBytes))
	if err != nil {
		var overflow *http.MaxBytesError
		if errors.As(err, &overflow) {
			writeJSON(w, http.StatusRequestEntityTooLarge,
				map[string]string{"error": "value_too_large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable_body"})
		return
	}
	operator, _ := auth.OperatorFrom(r.Context())
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_name"})
		return
	}
	removed, err := s.store.Unset(r.Context(), name)
	if err != nil {
		s.fail(w, "remove the secret", err)
		return
	}
	operator, _ := auth.OperatorFrom(r.Context())
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
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "no_active_key",
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
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "key_id_mismatch", "key_id": s.keyID, "your_key_id": want,
			"hint": "this node seals under its own secrets.active_key_id; make " +
				"the two configs agree before rekeying",
		})
		return
	}
	operator, _ := auth.OperatorFrom(r.Context())
	moved, err := s.store.Rekey(r.Context(), s.keyID, operator, s.now())
	if err != nil {
		// THE NAMES THAT DID MOVE travel with the refusal. A partial
		// rekey is a fact an operator has to act on, and a bare 500
		// would leave them re-running a pass with no idea which rows
		// are already under the new key.
		log.ErrorContext(r.Context(), "secret_rekey_failed", "error", err,
			"moved", moved, "operator", operator)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "rekey_incomplete", "moved": moved,
			"hint": "a row could not be opened with this node's keyring; the " +
				"key that sealed it is missing from secrets.keys",
		})
		return
	}
	log.InfoContext(r.Context(), "secrets_rekeyed", "moved", moved,
		"key_id", s.keyID, "operator", operator)
	writeJSON(w, http.StatusOK, map[string]any{"key_id": s.keyID, "moved": moved})
}

// sealed refuses a write on a node with no keyring, and says what to do.
func (s *Service) sealed(w http.ResponseWriter) bool {
	if s.cipher != nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "no_keyring",
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
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
}

// writeJSON is the one response writer for this surface.
func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		log.Error("secret_encode_failed", "error", err)
		http.Error(w, `{"error":"encode_failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
