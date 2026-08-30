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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
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
	plane   coord.Plane
	queue   queue.Publisher
	cipher  secrets.Cipher
	now     func() time.Time
}

// Options wire the service.
type Options struct {
	// Store holds the revisions. Required — without it there is no
	// surface, and the routes are not registered at all.
	Store *store.DB

	// Plane publishes the fleet's activation pointer. Required for the
	// write routes: storing a revision nothing points at activates
	// nothing, and a caller that got a 201 back would believe otherwise.
	Plane coord.Plane

	// Cipher opens and seals a stored revision. Nil reads plaintext and
	// writes plaintext, which is the documented opt-out.
	Cipher secrets.Cipher

	// Queue publishes the activation NUDGE, so an operator's change lands
	// on every node in milliseconds instead of at the next reconcile poll.
	// Nil skips it: the pointer is the authoritative path and the poll is
	// what reads it, so a missing nudge costs one interval and never a
	// revision.
	Queue queue.Publisher

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
	if opts.Plane == nil {
		// SAID AT REGISTRATION, because the write routes answer 503
		// rather than panicking and a 503 with no explanation anywhere
		// is a support ticket. A standalone API process with no
		// coordination store genuinely cannot activate anything.
		log.Warn("config_writes_disabled",
			"hint", "this process has no coordination store, so /config is read-only")
	}
	return &Service{
		configs: opts.Store.Configs(), plane: opts.Plane,
		cipher: opts.Cipher, queue: opts.Queue, now: now,
	}
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
	// WHAT THIS RESOURCE TAKES, asked rather than guessed. RFC 5789 §3.1:
	// a patch format is negotiated, not assumed, and Accept-Patch is where
	// a server says which ones it speaks.
	mux.HandleFunc("OPTIONS /config", s.optionsDocument)
	// THE NARROWER WRITE. See merge.go for why one patch route covers
	// every section rather than one route per section.
	mux.HandleFunc("PATCH /config", s.patch)
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
		// THE READ AND THE WRITE ON ONE URI. The entity was addressable
		// for writing long before it was readable here, so the documented
		// loop fetched from /query/config_entities — a different URI
		// space, answering a {kind, id, entity} envelope that PUT does
		// not accept. GET here answers the entity itself, so `GET | PUT`
		// round-trips with nothing in between.
		mux.HandleFunc("GET /config/"+kind+"/{id}", s.getEntity(kind))
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
	company, revision, err := s.documentOf(r.Context())
	switch {
	case errors.Is(err, ErrNoActiveRevision):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_active_revision"})
	case err != nil:
		s.fail(w, "read the active revision", err)
	default:
		if serveConditional(w, r, revision) {
			return
		}
		s.writeDocument(w, r, company)
	}
}

// serveConditional stamps the entity-tag and answers 304 when the caller
// already has this revision. It reports whether it answered.
//
// The tag is the revision id, because that is exactly what changes when the
// document changes — and it is the same token If-Match takes, which is the
// point: before this, the only way to learn the id a conditional write needs
// was to read a DIFFERENT resource (/config/revisions), so the read a caller
// naturally pairs a write with did not carry it.
func serveConditional(w http.ResponseWriter, r *http.Request, revision store.Revision) bool {
	tag := etagOf(revision)
	w.Header().Set("ETag", tag)
	if matchesTag(r.Header.Get("If-None-Match"), tag, true) {
		// RFC 9110 §13.1.2: on GET, a matching If-None-Match is 304 with
		// no content rather than a refusal.
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// etagOf renders a revision as a strong entity-tag.
//
// STRONG, and quoted as RFC 9110 §8.8.3 requires: a revision is immutable and
// its payload is byte-identical every time, so there is nothing weak about
// the correspondence.
func etagOf(revision store.Revision) string { return `"` + revision.ID + `"` }

// matchesTag reports whether a precondition header selects this tag.
//
// `*` means "any current representation", so it matches whenever there is
// one. A list is comma-separated and any member matching is a match. A bare
// revision id — unquoted, which is not a legal entity-tag — is accepted
// because this surface documented and shipped that form before it had tags,
// and breaking every script that reads a revision id out of a write response
// to add two quotes would be a cost with nothing on the other side.
func matchesTag(header, tag string, wildcardMatchesExisting bool) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return wildcardMatchesExisting
	}
	bare := strings.Trim(tag, `"`)
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == tag || candidate == bare {
			return true
		}
	}
	return false
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

// patchMediaTypes are the patch document formats this resource accepts.
//
// The registered type for RFC 7396 is application/merge-patch+json. Plain
// application/json and an absent type are accepted too: every example this
// project has published sends one of those, and refusing them would break
// working callers to make a point about a header.
//
// What this DOES refuse is a patch format that is not this one —
// application/json-patch+json above all, whose document is a LIST of
// operations. A merge patch that is not an object replaces the target
// outright, so an RFC 6902 document arriving here does not mean what its
// author intended; it used to be refused as a malformed merge patch, which
// told them the shape was wrong rather than that the format was.
var patchMediaTypes = []string{"application/merge-patch+json", "application/json"}

// acceptPatch is the Accept-Patch value, RFC 5789 §3.1.
const acceptPatch = "application/merge-patch+json"

// optionsDocument answers OPTIONS /config.
func (s *Service) optionsDocument(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "GET, HEAD, OPTIONS, PATCH, PUT")
	w.Header().Set("Accept-Patch", acceptPatch)
	w.WriteHeader(http.StatusNoContent)
}

// checkPatchMediaType refuses a patch document this resource cannot read,
// and reports whether the request may go on.
func (s *Service) checkPatchMediaType(w http.ResponseWriter, r *http.Request) bool {
	header := r.Header.Get("Content-Type")
	if header == "" {
		return true
	}
	media := strings.TrimSpace(strings.Split(header, ";")[0])
	if media == "" || slices.Contains(patchMediaTypes, strings.ToLower(media)) {
		return true
	}
	// 415 WITH Accept-Patch, which is the pair RFC 5789 §2.2 names: the
	// refusal has to say what would have worked.
	w.Header().Set("Accept-Patch", acceptPatch)
	writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
		"error": "unsupported_patch_media_type", "accept_patch": acceptPatch,
		"you_sent": media,
		"hint": "PATCH /config takes a JSON Merge Patch (RFC 7396) — an object " +
			"shaped like the document. A JSON Patch (RFC 6902) list of " +
			"operations is a different format this surface does not serve; " +
			"editing one seat is PUT /config/roles/{handle}",
	})
	return false
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
	// THE BODY IS READ FIRST, because the summary may be in it. See
	// [splitSummary]: a caller that cannot set a header can still put a
	// `_summary` key in the document.
	body, err := readBody(w, r)
	if err != nil {
		refuseBody(w, err)
		return
	}
	summary, body, err := splitSummary(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_body", "detail": err.Error(),
		})
		return
	}
	if header := r.Header.Get("X-Summary"); header != "" {
		// THE HEADER WINS when both are present. It is the more explicit
		// channel — a `_summary` can survive in a document somebody keeps
		// in version control long after it stopped describing the write.
		summary = header
	}
	if summary == "" {
		// Required, because the history is what an operator reads at 3am
		// to find the change that broke something. A list of revisions
		// with no summaries is a list of uuids.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "summary_required",
			"hint": "PUT /config needs an audit summary — the X-Summary header, " +
				"or a top-level _summary key in the body. The revision history " +
				"is the record of who changed what and why",
		})
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

// errEmptyPatch is a patch body carrying nothing.
var errEmptyPatch = errors.New("the patch is empty")

// patch serves PATCH /config — a JSON Merge Patch over the active document.
//
// # It is a READ-MODIFY-WRITE, which is the one thing it is not free
//
// A full PUT carries the caller's whole intended document, so a lost update
// costs whatever they did not know about. A patch is merged against whatever
// is active AT THIS INSTANT, so two patches to different sections both apply
// and two patches to the same section resolve by arrival order — with nothing
// telling the loser. `If-Match` is what closes that, and it matters MORE here
// than on the full write for exactly that reason: see [Service.checkPrecondition].
//
// # It refuses when nothing is active
//
// A patch is defined against a document. With no active revision there is
// nothing to merge onto, and building a company out of one section is not
// what this route is for — `PUT /config` shows the whole thing.
func (s *Service) patch(w http.ResponseWriter, r *http.Request) {
	if !s.checkPatchMediaType(w, r) {
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		refuseBody(w, err)
		return
	}
	summary, body, err := splitSummary(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_body", "detail": err.Error(),
		})
		return
	}
	if header := r.Header.Get("X-Summary"); header != "" {
		summary = header
	}
	if summary == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "summary_required",
			"hint": "PATCH /config needs an audit summary — the X-Summary " +
				"header, or a top-level _summary key in the body. A patch is " +
				"the change least visible in a diff, so the sentence saying " +
				"what it was for matters most here",
		})
		return
	}

	active, found, err := s.configs.Active(r.Context())
	if err != nil {
		s.fail(w, "read the active revision", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "no_active_revision",
			"hint": "there is nothing to patch; import a company first, or " +
				"use PUT /config to send a whole document",
		})
		return
	}
	if !s.checkPrecondition(w, r, active, found) {
		return
	}

	prior, err := s.open(active)
	if err != nil {
		s.fail(w, "open the active revision", err)
		return
	}
	// MERGED IN THE STORED SHAPE, not the authored one: the active
	// revision is normalised JSON, and merging onto anything else would
	// make the result depend on how the document was originally written.
	document, err := json.Marshal(prior)
	if err != nil {
		s.fail(w, "encode the active revision", err)
		return
	}
	merged, err := applyMergePatch(document, body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_patch", "detail": err.Error(),
		})
		return
	}
	// PARSED STRICTLY, which is what makes a typo in a patch a 400 rather
	// than a silently ignored section: the authored reader refuses an
	// unknown field, and it accepts the normalised form the merge emits.
	incoming, err := parseDocument(merged)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_patch", "detail": err.Error(),
			"hint": "the patched document was refused; an unknown key in a " +
				"patch is refused here rather than ignored",
		})
		return
	}
	// The masks a caller was shown come back as the values they hide, the
	// same as on the full write: a patch built from a redacted GET must
	// not replace a credential with "__redacted__".
	incoming.RestoreRedacted(prior)
	if err := incoming.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "validation_error", "detail": err.Error(),
			"hint": "a patch is validated as the WHOLE document it produces, " +
				"so a section that is fine on its own is still refused when " +
				"it leaves the company invalid",
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
	if s.plane == nil {
		// REFUSED, not stored. Writing a revision this process cannot
		// point the fleet at would answer 201 for a change that never
		// takes effect anywhere — the exact failure the control plane
		// exists to remove.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "no_control_plane",
			"detail": "this process has no coordination store, so it cannot " +
				"activate a revision",
			"hint": "post to a node running the engine",
		})
		return
	}
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
	at := s.now()
	// STORED FIRST, then pointed at. A crash between the two leaves a
	// revision nothing points at — inert, and recoverable with `crewlet
	// config activate <id>` — while the other order would point the fleet
	// at a revision no node can read.
	//
	// A COMMAND rather than a route: this surface serves no activate, and
	// the nearest thing it does serve, POST /config/revisions/{id}/revert,
	// stores a NEW revision carrying the old payload rather than pointing
	// back at the orphan.
	id, err := s.configs.InsertActive(r.Context(), store.Revision{
		ParentID: parent, Source: "api", CreatedBy: operator,
		Summary: summary, Payload: payload, CreatedAt: at,
	})
	if err != nil {
		s.fail(w, "store the config", err)
		return
	}
	// The SEALED payload travels with the pointer. It is stored here too
	// — this node's own history, diffs and revert targets read that table
	// — but the fleet's copy is what every OTHER node applies from, and
	// without it a live config change reached exactly this node.
	//
	// EXPECT IS THE REVISION THIS EDIT WAS BUILT ON, which is what `parent`
	// already means: every write on this surface reads the active revision,
	// derives from it, and names it as the parent. Passing it makes the flip
	// a compare-and-set, so a concurrent write on ANY node is refused here
	// rather than silently overwritten — and that holds whether or not the
	// caller sent If-Match, because the server knows what it read.
	//
	// Empty on a first import, where there is nothing to have raced with.
	published, err := s.plane.Activate(r.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: summary, Payload: payload, At: at, Expect: parent,
	})
	if errors.Is(err, coord.ErrActivationRaced) {
		// THE REVISION IS KEPT, not unwound. It is stored, valid and
		// inert — the operator's work survives as history they can
		// revert to — and this node's reconciler adopts whichever
		// revision actually won at its next tick. Unwinding instead
		// would mean a second write that can itself fail, on the path
		// where something has already gone wrong.
		log.InfoContext(r.Context(), "config_activation_raced",
			"revision", id, "expected", parent, "by", operator)
		current, _, terr := s.plane.Target(r.Context())
		body := map[string]any{
			"error": "revision_advanced", "your_base": parent,
			"stored_revision_id": id,
			"hint": "another write activated first; this revision was stored " +
				"but not activated. Re-read /config and send the edit again",
		}
		if terr == nil {
			body["current_revision_id"] = current.RevisionID
		}
		writeJSON(w, http.StatusConflict, body)
		return
	}
	if err != nil {
		s.fail(w, "activate the config", err)
		return
	}
	s.nudge(r.Context(), id, summary, operator)
	log.InfoContext(r.Context(), "config_revision_written",
		"revision", id, "epoch", published.Epoch, "by", operator, "summary", summary)
	writeJSON(w, http.StatusCreated,
		map[string]any{"revision_id": id, "epoch": published.Epoch})
}

// nudge tells every node an activation happened.
//
// BEST EFFORT and deliberately thin — the event carries no payload, because
// the authoritative path is the pointer and a node acts by re-reading it. That
// is what makes losing one cost a poll interval rather than a revision, and
// what makes an ephemeral broadcast the right delivery: every node has to
// hear it, and none of them has to.
func (s *Service) nudge(ctx context.Context, revisionID, summary, operator string) {
	if s.queue == nil {
		return
	}
	ev := events.New(types.ConfigRevisionActivated{
		RevisionID: revisionID, RevisionSummary: summary, CreatedBy: operator,
	}, events.NewTrace())
	ev.Timestamp = s.now()
	ev.Source = operator
	if err := s.queue.Publish(ctx, topics.ConfigRevisionActivated, ev); err != nil {
		log.WarnContext(ctx, "activation_nudge_not_published", "revision", revisionID,
			"error", err, "detail", "peers converge on their reconcile interval instead")
	}
}

// checkPrecondition enforces If-Match, the optimistic-concurrency guard.
//
// Two operators editing one company through a full-document PUT is a
// last-writer-wins race that silently discards the other's change. If-Match
// turns it into a 409 the loser can see, and the document they need to re-read
// is named in the answer.
func (s *Service) checkPrecondition(w http.ResponseWriter, r *http.Request, active store.Revision, found bool) bool {
	// IF-NONE-MATCH FIRST, because `*` on a write is the create-only
	// precondition (RFC 9110 §13.1.2) — "store this only if the company
	// has not been configured yet" — and it is the standard spelling of
	// what this surface documented as `If-Match: none`.
	if none := r.Header.Get("If-None-Match"); none != "" {
		if !found {
			return true
		}
		if matchesTag(none, etagOf(active), true) {
			writeJSON(w, http.StatusPreconditionFailed, map[string]any{
				"error": "already_configured", "current_revision_id": active.ID,
				"hint": "If-None-Match asked for this write to land only on a " +
					"config that is not there; one is active",
			})
			return false
		}
		return true
	}

	expected := strings.TrimSpace(r.Header.Get("If-Match"))
	switch {
	case expected == "":
		// Unconditional, and permitted: a first import has nothing to
		// match against, and a script that owns the config outright has
		// no race to lose.
		return true
	case expected == "none":
		// THE PRE-TAG SPELLING of If-None-Match: *, documented and
		// shipped before this surface had entity-tags, and never
		// implemented — it fell into the branch below and answered 412
		// on exactly the unconfigured node it was meant to permit.
		// Honoured rather than dropped, because the documentation
		// promised it and a script written against that promise is not
		// wrong.
		if found {
			writeJSON(w, http.StatusPreconditionFailed, map[string]any{
				"error": "already_configured", "current_revision_id": active.ID,
				"hint": "If-Match: none asked for this write to land only on an " +
					"unconfigured node; a revision is active",
			})
			return false
		}
		return true
	case !found:
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{
			"error": "no_active_revision",
			"hint": "there is no revision to match against; retry without " +
				"If-Match, or send If-None-Match: * to require that",
		})
		return false
	case !matchesTag(expected, etagOf(active), true):
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
