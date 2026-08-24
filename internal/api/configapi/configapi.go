// Package configapi serves /config: the versioned company document, its
// history, and the write path that activates a new revision.
//
// EVERY ROUTE HERE IS GUARDED, reads included, and that is not the usual
// posture on this API. Reading this surface exposes the whole company
// document — its org chart, its integrations, and the shape of every
// credential it holds — and writing it changes the company. The auth package
// makes /config the one prefix that is never eligible for
// allow_anonymous_read.
//
// A WRITE HERE DOES NOT APPLY ANYTHING. It stores a revision and moves the
// activation pointer; every node, including this one, applies it on its own
// reconcile tick. That is what makes a write on one node reach the whole
// fleet — the failure the control plane exists to remove was a config change
// that only the process handling the request ever saw.
package configapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
	"gopkg.in/yaml.v3"
)

var log = logging.Get("api.config")

// MaxBodyBytes bounds a config upload.
//
// The document is an org chart and its settings: the largest real one in this
// repository is tens of kilobytes, so 4 MiB is three orders of magnitude of
// headroom and still finite. The route is guarded, so this is a bound on a
// mistake rather than on an attacker.
const MaxBodyBytes = 4 << 20

// DefaultPage and MaxPage bound a history listing.
const (
	DefaultPage = store.DefaultRevisionPage
	MaxPage     = 500
)

// Service is the /config surface.
type Service struct {
	configs *store.Configs
	cipher  secrets.Cipher
	now     func() time.Time
}

// Options wire the service.
type Options struct {
	// Store holds the revisions. Required — without it there is no
	// surface, and the routes are not registered at all.
	Store *store.DB

	// Cipher opens and seals a stored revision. Nil reads plaintext and
	// writes plaintext, which is the documented opt-out.
	Cipher secrets.Cipher

	// Now is injectable so a test can pin the revision timestamps.
	Now func() time.Time
}

// New builds the service, or nil when there is no store to serve from.
//
// Nil rather than an error: a standalone API with no store genuinely has no
// config surface, and the routes then 404 rather than 500 — which is the
// honest answer for a process that does not implement them.
func New(opts Options) *Service {
	if opts.Store == nil {
		return nil
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{configs: opts.Store.Configs(), cipher: opts.Cipher, now: now}
}

// Routes registers the surface on the API's mux.
func (s *Service) Routes(mux *http.ServeMux) {
	if s == nil {
		log.Warn("config_surface_disabled",
			"hint", "this process has no store, so /config is not served here")
		return
	}
	mux.HandleFunc("GET /config", s.getActive)
	mux.HandleFunc("PUT /config", s.put)
	mux.HandleFunc("GET /config/revisions", s.listRevisions)
	mux.HandleFunc("GET /config/revisions/{id}", s.getRevision)
	mux.HandleFunc("GET /config/revisions/{id}/diff", s.diff)
	mux.HandleFunc("POST /config/revisions/{id}/revert", s.revert)
	// THE ENTITY ROUTES, one per addressable collection rather than a
	// single {kind} wildcard: a wildcard would also match
	// /config/revisions/{id}, and a route that answers for a path it was
	// never meant to serve is worse than four explicit lines. See
	// entities.go for what a write does.
	for _, kind := range EntityKinds() {
		mux.HandleFunc("PUT /config/"+kind+"/{id}", s.putEntity(kind))
	}
}

// --- reads -----------------------------------------------------------------

// getActive serves GET /config — the active document, redacted.
//
// 404 when nothing is active, and it is a real answer rather than an error: a
// deployment before its first import has no configuration, and reporting that
// as a failure would make a working new install look broken.
func (s *Service) getActive(w http.ResponseWriter, r *http.Request) {
	company, err := s.Document(r.Context())
	switch {
	case errors.Is(err, ErrNoActiveRevision):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_active_revision"})
	case err != nil:
		s.fail(w, "read the active revision", err)
	default:
		s.writeDocument(w, r, company)
	}
}

// writeDocument answers in the format the caller asked for.
//
// YAML because that is the form an operator edits and the form every example
// in the documentation is written in. A surface that could only speak JSON
// would make "read it, change a line, send it back" a format conversion.
func (s *Service) writeDocument(w http.ResponseWriter, r *http.Request, company *config.Company) {
	if r.URL.Query().Get("format") != "yaml" {
		writeJSON(w, http.StatusOK, company)
		return
	}
	body, err := yaml.Marshal(company)
	if err != nil {
		s.fail(w, "encode the config as yaml", err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// listRevisions serves GET /config/revisions — metadata only, newest first.
//
// METADATA ONLY. A listing that carried every payload would move the whole
// history through the process to render a table of summaries, and the
// documents are the largest rows in the database.
func (s *Service) listRevisions(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := page(w, r)
	if !ok {
		return
	}
	out, err := s.Revisions(r.Context(), limit, offset)
	if err != nil {
		s.fail(w, "list revisions", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getRevision serves GET /config/revisions/{id} — one revision with its
// redacted payload.
func (s *Service) getRevision(w http.ResponseWriter, r *http.Request) {
	revision, ok := s.lookup(w, r, r.PathValue("id"), "not_found")
	if !ok {
		return
	}
	company, err := s.open(revision)
	if err != nil {
		s.fail(w, "open revision", err)
		return
	}
	body := meta(revision)
	body["payload"] = company.Redact()
	writeJSON(w, http.StatusOK, body)
}

// diff serves GET /config/revisions/{id}/diff?against=<id|active>.
//
// Over REDACTED documents on both sides, so a rotated credential shows as a
// changed mask and never as either value. Comparing the raw documents would
// put both the old and the new secret in one response — which is strictly
// worse than the read this surface already refuses to serve.
func (s *Service) diff(w http.ResponseWriter, r *http.Request) {
	body, err := s.Diff(r.Context(), r.PathValue("id"), r.URL.Query().Get("against"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, body)
	case errors.Is(err, ErrNoActiveRevision):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_active_revision"})
	case errors.Is(err, store.ErrNoRevision):
		// WHICH side is missing. "The revision you asked about" and "the
		// one you asked to compare it with" are different mistakes, and a
		// single not_found makes the caller check both.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": missingSide(err, r)})
	default:
		s.fail(w, "diff revisions", err)
	}
}

// missingSide names which half of a diff could not be found.
func missingSide(err error, r *http.Request) string {
	if strings.Contains(err.Error(), r.PathValue("id")) {
		return "not_found"
	}
	return "against_not_found"
}

// --- writes ----------------------------------------------------------------

// put serves PUT /config — a full-document replacement.
//
// FULL, not a merge: the body is the company from now on. A merge would make
// deleting a role impossible through this surface, which is the one operation
// an operator most needs to be sure of.
func (s *Service) put(w http.ResponseWriter, r *http.Request) {
	summary := r.Header.Get("X-Summary")
	if summary == "" {
		// Required, because the history is what an operator reads at 3am
		// to find the change that broke something. A list of revisions
		// with no summaries is a list of uuids.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "summary_required",
			"hint": "PUT /config needs an audit summary in the X-Summary header — " +
				"the revision history is the record of who changed what and why",
		})
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		refuseBody(w, err)
		return
	}
	incoming, err := parseDocument(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_body", "detail": err.Error(),
		})
		return
	}

	active, found, err := s.configs.Active(r.Context())
	if err != nil {
		s.fail(w, "read the active revision", err)
		return
	}
	if !s.checkPrecondition(w, r, active, found) {
		return
	}

	if found {
		prior, err := s.open(active)
		if err != nil {
			s.fail(w, "open the active revision", err)
			return
		}
		// The masks the caller was shown come back as the values they
		// hide. Without this, a reader who fetched the config, changed
		// one line and sent it back would replace every credential in
		// the company with the mask.
		incoming.RestoreRedacted(prior)
	}

	// VALIDATED AFTER the restore, so a masked credential is judged as the
	// value it stands for. Validating first would reject a document for
	// carrying "__redacted__" where the operator changed nothing.
	if err := incoming.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "validation_error", "detail": err.Error(),
		})
		return
	}
	s.store(w, r, incoming, active.ID, summary)
}

// revert serves POST /config/revisions/{id}/revert.
//
// A NEW revision carrying the old document, never a pointer moved backwards.
// The history stays append-only, so "we reverted at 04:12" is a fact somebody
// can find later — and the epoch keeps advancing, which is what makes every
// node reconcile onto it.
func (s *Service) revert(w http.ResponseWriter, r *http.Request) {
	target, ok := s.lookup(w, r, r.PathValue("id"), "not_found")
	if !ok {
		return
	}
	// OPENED, not copied. A revision sealed under a key no longer in the
	// keyring cannot be reverted to, and finding that out now beats
	// activating a document every node will fail to read.
	company, err := s.open(target)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "unreadable_revision", "detail": err.Error(),
			"hint": "the target revision is sealed under a key that is no longer " +
				"in the keyring; restore it to the node's secrets.keys first",
		})
		return
	}
	active, found, err := s.configs.Active(r.Context())
	if err != nil {
		s.fail(w, "read the active revision", err)
		return
	}
	parent := ""
	if found {
		parent = active.ID
	}
	summary := r.Header.Get("X-Summary")
	if summary == "" {
		summary = "revert to " + target.ID
	}
	s.store(w, r, company, parent, summary)
}

// store seals and activates a document, and answers.
func (s *Service) store(w http.ResponseWriter, r *http.Request, company *config.Company, parent, summary string) {
	document, err := json.Marshal(company)
	if err != nil {
		s.fail(w, "encode the config", err)
		return
	}
	payload, err := secrets.Seal(s.cipher, document)
	if err != nil {
		s.fail(w, "seal the config", err)
		return
	}
	operator, _ := auth.OperatorFrom(r.Context())
	id, epoch, err := s.configs.InsertActive(r.Context(), store.Revision{
		ParentID: parent, Source: "api", CreatedBy: operator,
		Summary: summary, Payload: payload, CreatedAt: s.now(),
	})
	if err != nil {
		s.fail(w, "activate the config", err)
		return
	}
	log.InfoContext(r.Context(), "config_revision_written",
		"revision", id, "epoch", epoch, "by", operator, "summary", summary)
	writeJSON(w, http.StatusCreated, map[string]any{"revision_id": id, "epoch": epoch})
}

// checkPrecondition enforces If-Match, the optimistic-concurrency guard.
//
// Two operators editing one company through a full-document PUT is a
// last-writer-wins race that silently discards the other's change. If-Match
// turns it into a 409 the loser can see, and the document they need to re-read
// is named in the answer.
func (s *Service) checkPrecondition(w http.ResponseWriter, r *http.Request, active store.Revision, found bool) bool {
	expected := r.Header.Get("If-Match")
	switch {
	case expected == "":
		// Unconditional, and permitted: a first import has nothing to
		// match against, and a script that owns the config outright has
		// no race to lose.
		return true
	case !found:
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{
			"error": "no_active_revision",
			"hint":  "there is no revision to match against; retry without If-Match",
		})
		return false
	case expected != active.ID:
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "revision_advanced", "current_revision_id": active.ID,
			"your_base": expected,
		})
		return false
	default:
		return true
	}
}

// --- plumbing --------------------------------------------------------------

// open decrypts a stored revision into a config.
func (s *Service) open(revision store.Revision) (*config.Company, error) {
	document, err := secrets.Open(s.cipher, revision.Payload)
	if err != nil {
		return nil, err
	}
	return config.DecodeCompany(document)
}

// lookup fetches a revision by id, answering the refusal itself.
func (s *Service) lookup(w http.ResponseWriter, r *http.Request, id, missing string) (store.Revision, bool) {
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_revision_id"})
		return store.Revision{}, false
	}
	revision, found, err := s.configs.Get(r.Context(), id)
	if err != nil {
		s.fail(w, "read revision", err)
		return store.Revision{}, false
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": missing})
		return store.Revision{}, false
	}
	return revision, true
}

// meta is a revision without its payload.
func meta(revision store.Revision) map[string]any {
	body := map[string]any{
		"revision_id": revision.ID,
		"created_at":  revision.CreatedAt.Format(time.RFC3339Nano),
		"created_by":  revision.CreatedBy,
		"source":      revision.Source,
		"summary":     revision.Summary,
		"is_active":   revision.Active,
	}
	if revision.ParentID != "" {
		body["parent_revision_id"] = revision.ParentID
	}
	if !revision.ActivatedAt.IsZero() {
		body["activated_at"] = revision.ActivatedAt.Format(time.RFC3339Nano)
	}
	return body
}

// page reads and clamps the listing window.
func page(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, ok = intParam(w, r, "limit", DefaultPage)
	if !ok {
		return 0, 0, false
	}
	offset, ok = intParam(w, r, "offset", 0)
	if !ok {
		return 0, 0, false
	}
	// CLAMPED, not refused. A caller asking for more than the ceiling has
	// made no mistake worth a 400 — they want everything — and a page size
	// nobody bounds is one tab pulling the whole history through a process
	// every other tab shares.
	return min(max(limit, 1), MaxPage), max(offset, 0), true
}

func intParam(w http.ResponseWriter, r *http.Request, name string, fallback int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_pagination", "detail": name + " must be a number",
		})
		return 0, false
	}
	return value, true
}

// fail logs the reason and answers without it.
//
// The reason reaches the LOG, never the caller: a store error can carry a
// database path or a driver's own message, and this surface is the one an
// operator reaches from a browser.
func (s *Service) fail(w http.ResponseWriter, what string, err error) {
	log.Error("config_request_failed", "what", what, "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
}

var errBodyTooLarge = errors.New("configapi: body over the limit")

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err == nil {
		return body, nil
	}
	var overflow *http.MaxBytesError
	if errors.As(err, &overflow) {
		return nil, errBodyTooLarge
	}
	return nil, err
}

func refuseBody(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge,
			map[string]string{"error": "body_too_large"})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable_body"})
}

// parseDocument reads a config in either form the operator writes.
//
// YAML is a superset of JSON, so ONE reader covers both — and it is the
// authored reader, which fails closed on an unknown field. That strictness
// belongs here and not on the stored form: this is the door a person's
// document comes through, and a typo is a mistake to catch rather than a peer
// running a newer build.
//
// It deliberately does NOT validate: a document carrying redaction masks is
// judged after those are resolved, or an operator who changed nothing but a
// role name would be told their credentials are invalid.
func parseDocument(body []byte) (*config.Company, error) {
	cfg, err := config.ParseCompanyDocument(body)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// writeJSON is the one response writer for this surface.
func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		log.Error("config_encode_failed", "error", err)
		http.Error(w, `{"error":"encode_failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
