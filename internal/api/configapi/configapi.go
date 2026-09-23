// Package configapi serves /config: the versioned company document, its
// history, and the write path that activates a new revision.
//
// EVERY ROUTE HERE TAKES A GRANT, reads included: `config:read` for every
// read (`config.read`) and `config:write` for every write (`config.write`),
// mounted through [authz.Router] so a route with no policy fails the boot.
// Reading this surface exposes the whole company document — its integrations
// and the name of every credential it holds — and writing it changes the
// company.
//
// THE ROUTES USED TO DECIDE NOTHING, which was sound while the only credential
// was an operator token and stopped being sound the day a person could sign in
// holding `state:read` alone: every one of them could rewrite the company
// document, revert it to any revision, and read every `${VAR}` it names.
//
// A WRITE HERE DOES NOT APPLY ANYTHING. It stores a revision and moves the
// activation pointer; every node, including this one, applies it on its own
// reconcile tick. That is what makes a write on one node reach the whole
// fleet — the failure the control plane exists to remove was a config change
// that only the process handling the request ever saw.
package configapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracing"
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

// revisionStore is the node's revision history, as this surface reads and
// writes it.
type revisionStore interface {
	Active(ctx context.Context) (store.Revision, bool, error)
	Get(ctx context.Context, revisionID string) (store.Revision, bool, error)
	List(ctx context.Context, limit, offset int) ([]store.Revision, error)
	Insert(ctx context.Context, r store.Revision) (string, error)
	Activate(ctx context.Context, revisionID string, at time.Time) (string, error)
}

// Service is the /config surface.
type Service struct {
	configs revisionStore
	plane   coord.Plane
	queue   queue.Publisher
	cipher  secrets.Cipher
	now     func() time.Time
}

// Options wire the service.
type Options struct {
	// Store holds the revisions. Required: every node that serves this
	// surface opens one, and [New] refuses to build without it.
	Store *store.DB

	// Plane publishes the fleet's activation pointer. Required: storing a
	// revision nothing points at activates nothing, and a caller that got
	// a 201 back would believe otherwise. Every node holds one, because
	// the fleet store is opened on every topology.
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

// New builds the service.
//
// A MISSING STORE OR PLANE IS REFUSED rather than served as a narrower surface.
// `crewlet run` builds this beside an engine that holds both, so a nil here is a
// wiring mistake, and a surface that quietly shrank around it (an unregistered
// /config, a write answering 503) would hide exactly that.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("configapi: Options.Store is required: the revisions " +
			"live in the node's store, which the engine opens")
	case opts.Plane == nil:
		return nil, errors.New("configapi: Options.Plane is required: a revision " +
			"takes effect only once the fleet's activation pointer names it")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		configs: opts.Store.Configs(), plane: opts.Plane,
		cipher: opts.Cipher, queue: opts.Queue, now: now,
	}, nil
}

// Routes registers the surface on the API's mux, every route through
// [authz.Router] with the verb it is decided by. A route mounted without one
// is refused here and fails the boot — see [authz.Router.Handle].
func (s *Service) Routes(mux authz.Mux) error {
	// ONE SUB-MUX BEHIND ONE WRAPPER, so no response under /config can be
	// written without the Cache-Control below: not a route added later, not
	// an error path, not a refusal, and not the 404 or 405 this mux answers
	// for a path or a method it does not serve.
	routes := http.NewServeMux()
	router := authz.NewRouter(routes, authz.ContextGuard(authz.NoChart{}))
	var failures []error
	mount := func(pattern string, a authz.Action, h http.HandlerFunc) {
		if err := router.Handle(pattern, authz.Policy{Action: a}, h); err != nil {
			failures = append(failures, err)
		}
	}
	read := func(pattern string, h http.HandlerFunc) {
		mount(pattern, authz.ActionConfigRead, h)
	}
	write := func(pattern string, h http.HandlerFunc) {
		mount(pattern, authz.ActionConfigWrite, h)
	}
	read("GET /config", s.getActive)
	write("PUT /config", s.put)
	// WHAT THIS RESOURCE TAKES, asked rather than guessed. RFC 5789 §3.1:
	// a patch format is negotiated, not assumed, and Accept-Patch is where
	// a server says which ones it speaks.
	read("OPTIONS /config", s.optionsDocument)
	// THE NARROWER WRITE. See merge.go for why one patch route covers
	// every section rather than one route per section.
	write("PATCH /config", s.patch)
	// RE-PUBLISH THE ACTIVE DOCUMENT UNCHANGED, which is the gesture a
	// rotated SECRET needs and the one thing no other route on this
	// surface performs: the pointer in the config is already correct, so
	// there is no patch to make, and with no activation there is no apply
	// and no refreshed secret snapshot. See [Service.Reload].
	write("POST /config/reload", s.reload)
	// WHICH FIELDS NAME A ${VAR}, which is what an operator needs before
	// they remove a credential. See [Service.References].
	read("GET /config/references", s.references)
	read("GET /config/revisions", s.listRevisions)
	read("GET /config/revisions/{id}", s.getRevision)
	read("GET /config/revisions/{id}/diff", s.diff)
	write("POST /config/revisions/{id}/revert", s.revert)
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
		read("GET /config/"+kind+"/{id}", s.getEntity(kind))
	}
	// AND THE WRITE ONLY WHERE THERE IS ONE. `roles` and `units` are the
	// org chart, which is a domain of its own with its own routes — so the
	// pattern is ABSENT here rather than mounted and refusing. A route that
	// exists and answers 400 to everything reads as a surface that is
	// broken; one that is not there matches what the product says, and the
	// per-entity refusal in chartdoor.go still covers the PATH, because a
	// caller who reaches it deserves the sentence rather than a 405.
	for _, kind := range WritableEntityKinds() {
		write("PUT /config/"+kind+"/{id}", s.putEntity(kind))
	}
	// THE CHART'S OWN COLLECTIONS, answered by NAME rather than by the
	// method fallthrough: a 405 on `PUT /config/roles/ceo` tells an
	// operator that the verb is wrong, when what is wrong is the surface.
	// Decided as the write it attempts, so the sentence pointing at /chart
	// is read by somebody who could have made the write here.
	for _, kind := range []string{EntityRoles, EntityUnits} {
		write("PUT /config/"+kind+"/{id}", s.refuseChartWrite(kind))
	}
	surface := noStore(routes)
	mux.Handle("/config", surface)
	mux.Handle("/config/", surface)
	return errors.Join(failures...)
}

// noStore marks every response it wraps as never to be stored.
//
// EVERY ONE, reads, refusals and 304s alike. A /config body is the whole company
// document: its org chart, its contact identities, the ${VAR} name behind every
// credential and the shape of the rest. Answered with an ETag and no
// Cache-Control, a browser keeps it in its HTTP disk cache, where it outlives
// the tab, the session and the operator token that was needed to read it. An
// error body is included because it can quote the document back (a validation
// failure names the field and the value it refused).
//
// no-store rather than private or no-cache: private still permits the browser's
// own cache, and no-cache only forces revalidation of what is stored.
// Conditional reads keep working, because a client that wants a 304 sends
// If-None-Match itself; nothing here depends on a cache holding the body.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
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

// references serves GET /config/references.
//
// 404 when nothing is active, matching GET /config: a deployment before its
// first import has no document to reference anything, and reporting that as a
// failure would make a working new install look broken.
func (s *Service) references(w http.ResponseWriter, r *http.Request) {
	refs, revision, err := s.References(r.Context())
	switch {
	case errors.Is(err, ErrNoActiveRevision):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_active_revision"})
	case err != nil:
		s.fail(w, "read the active revision", err)
	default:
		// THE SAME VALIDATOR the document itself carries: the index is
		// derived from the revision and changes exactly when it does.
		if serveConditional(w, r, revision) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"revision": revision.ID, "references": refs,
		})
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
	if matchesTag(r.Header.Get("If-None-Match"), tag) {
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
// `*` means "any current representation" and matches unconditionally here,
// because EVERY CALLER HAS ONE: each checks `found` (or holds the revision
// already) before asking, and answers the no-representation case itself with
// the message that case needs. This took that as a parameter and every call
// site passed true — a knob with one value, which reads as a decision the
// caller gets to make and is not one.
//
// A list is comma-separated and any member matching is a match. A bare
// revision id — unquoted, which is not a legal entity-tag — is accepted
// because this surface documented and shipped that form before it had tags,
// and breaking every script that reads a revision id out of a write response
// to add two quotes would be a cost with nothing on the other side.
func matchesTag(header, tag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	bare := strings.Trim(tag, `"`)
	for candidate := range strings.SplitSeq(header, ",") {
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
		"hint": "PATCH /config takes a JSON Merge Patch (RFC 7396): an object " +
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
// Every value it reports is REDACTED, so a rotated credential shows as a
// changed mask and never as either value; the comparison itself reads the
// stored documents, or a rotation would diff to nothing. See [Changes].
func (s *Service) diff(w http.ResponseWriter, r *http.Request) {
	body, err := s.Diff(r.Context(), r.PathValue("id"), r.URL.Query().Get("against"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, body)
	case errors.Is(err, ErrNoActiveRevision):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_active_revision"})
	case errors.Is(err, store.ErrNoRevision):
		// WHICH side is missing, read off the error rather than guessed
		// from the request — see [missingRevision].
		writeJSON(w, http.StatusNotFound, map[string]string{"error": missingSide(err)})
	default:
		s.fail(w, "diff revisions", err)
	}
}

// missingSide names which half of a diff could not be found.
//
// It reads the side off the error. The version this replaces compared the
// error's TEXT against the request's path value, which is wrong whenever one
// id contains the other — and `against` is unvalidated, so
// `?against=r-7-typo` on revision `r-7` reported the target as missing when
// it exists and the base does not.
//
// A revision error from anywhere else is reported as the target's, which is
// the honest default: every other producer of store.ErrNoRevision on this
// path is looking up the id in the URL.
func missingSide(err error) string {
	var missing *missingRevision
	if errors.As(err, &missing) {
		return missing.side
	}
	return sideTarget
}

// --- writes ----------------------------------------------------------------

// The machine-readable codes a refused document is answered with.
const (
	codeValidationError = httpjson.Code("validation_error")
	codeInvalidPatch    = httpjson.Code("invalid_patch")
)

// dryRunOf reads `dry_run` from a write's query, answering the refusal itself.
// ok is false when the request has been answered.
//
// READ BEFORE ANYTHING ELSE the route checks, the body and its summary
// included, so a caller who mistyped the parameter is told about the
// parameter: a check misread as a write would be refused for a missing
// summary, and a write misread as a check would store nothing and say so.
//
// Exactly `true` or `false`, or absent. Anything else (`1`, `yes`, an empty
// value, the parameter twice) is refused rather than guessed, because the two
// readings of a guess differ by whether the fleet's configuration changes.
func dryRunOf(w http.ResponseWriter, r *http.Request) (dryRun, ok bool) {
	values, present := r.URL.Query()["dry_run"]
	if !present {
		return false, true
	}
	if len(values) == 1 {
		switch values[0] {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery, map[string]any{
		"detail": "dry_run must be true or false, given once",
		"hint": "dry_run=true validates the write and answers what it would " +
			"produce without storing or activating anything; leave it out to write",
	})
	return false, false
}

// put serves PUT /config — a full-document replacement.
//
// FULL, not a merge: the body is the company from now on. A merge would make
// deleting a role impossible through this surface, which is the one operation
// an operator most needs to be sure of.
//
// With `dry_run=true` it is the same request, checked in the same order, that
// stores and activates nothing: see [Service.prepare].
func (s *Service) put(w http.ResponseWriter, r *http.Request) {
	dryRun, ok := dryRunOf(w, r)
	if !ok {
		return
	}
	// THE BODY IS READ FIRST, because the summary may be in it. See
	// [splitSummary]: a caller that cannot set a header can still put a
	// `_summary` key in the document.
	body, err := readBody(w, r)
	if err != nil {
		refuseBody(w, err)
		return
	}
	summary, sent, ok := takeSummary(w, r, body, !dryRun,
		"PUT /config needs an audit summary: the X-Summary header, "+
			"or a top-level _summary key in the body. The revision history "+
			"is the record of who changed what and why")
	if !ok {
		return
	}

	// BEFORE THE DOCUMENT IS PARSED, because the parser's answer to a
	// chart is "unknown field" and that sends an operator hunting a typo
	// they did not make. See chartdoor.go.
	if refuseChartIn(w, sent, http.MethodPut) {
		return
	}
	incoming, err := sent.company()
	if err != nil {
		refuseDocument(w, httpjson.CodeInvalidBody, err.Error(), "", &DocumentError{Err: err})
		return
	}

	active, found, err := s.configs.Active(r.Context())
	if err != nil {
		s.fail(w, "read the active revision", err)
		return
	}
	createOnly, ok := s.checkPrecondition(w, r, active, found)
	if !ok {
		return
	}
	built := ""
	if found {
		built = active.ID
	}
	prepared, err := s.prepare(r.Context(), replaceDraft(incoming, built))
	if err != nil {
		s.refuseWrite(w, err, createOnly)
		return
	}
	if dryRun {
		writeChecked(w, prepared)
		return
	}
	applied, err := s.commit(r.Context(), prepared, summary, operatorOf(r))
	if err != nil {
		s.refuseWrite(w, err, createOnly)
		return
	}
	writeApplied(w, applied)
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
	dryRun, ok := dryRunOf(w, r)
	if !ok {
		return
	}
	if !s.checkPatchMediaType(w, r) {
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		refuseBody(w, err)
		return
	}
	summary, sent, ok := takeSummary(w, r, body, !dryRun,
		"PATCH /config needs an audit summary: the X-Summary "+
			"header, or a top-level _summary key in the body. A patch is "+
			"the change least visible in a diff, so the sentence saying "+
			"what it was for matters most here")
	if !ok {
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
	if _, ok := s.checkPrecondition(w, r, active, found); !ok {
		return
	}
	// ON WHAT THE CALLER SENT, never on the merge: a revision written
	// before the chart's split still carries one inside it, so judging the
	// merged document would refuse an operator patching a mission for a
	// chart they did not send and cannot see. See chartdoor.go.
	if refuseChartIn(w, sent, http.MethodPatch) {
		return
	}

	prepared, err := s.prepare(r.Context(), patchDraft(ApplyRequest{
		Patch: sent.text, Summary: summary, Operator: operatorOf(r), Expect: active.ID,
	}, sent.doc))
	if err != nil {
		s.refuseApply(w, err)
		return
	}
	if dryRun {
		writeChecked(w, prepared)
		return
	}
	applied, err := s.commit(r.Context(), prepared, summary, operatorOf(r))
	if err != nil {
		s.refuseApply(w, err)
		return
	}
	writeApplied(w, applied)
}

// writeChecked answers a dry run: the write is valid, what it was checked
// against, and what it would produce. Nothing was stored.
//
// base_revision_id is the revision the check was built on, and empty when
// nothing was active. A client comparing it with the revision its draft was
// built on learns about a write that landed between its read and this check
// without a second request.
func writeChecked(w http.ResponseWriter, p *prepared) {
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": true, "base_revision_id": p.base,
		"warnings": p.warnings,
	})
}

// writeApplied answers a write that stored and activated a revision.
func writeApplied(w http.ResponseWriter, applied Applied) {
	writeJSON(w, http.StatusCreated, map[string]any{
		"revision_id": applied.RevisionID, "epoch": applied.Epoch,
		"warnings": applied.Warnings,
	})
}

// refuseWrite is [Service.refuseApply] for a write that may have been
// create-only.
//
// A create-only write that finds a revision where it expected none is not a
// lost edit to re-derive; it is a company that exists, so it is answered as
// its precondition would have been had the revision been there to see.
func (s *Service) refuseWrite(w http.ResponseWriter, err error, createOnly bool) {
	var raced *RacedError
	if createOnly && errors.As(err, &raced) {
		body := map[string]any{
			"error": "already_configured",
			"hint": "If-None-Match asked for this write to land only on a " +
				"config that is not there; one was activated first",
		}
		// NAMED ONLY WHEN KNOWN, as on every other refusal here: the
		// pointer is re-read after a lost activation, and a read that
		// failed has no revision to name. An empty id is not "none", and a
		// client fetching /config/revisions/ to see what won would be
		// sent to a route that is not there.
		if raced.Current != "" {
			body["current_revision_id"] = raced.Current
		}
		if raced.Stored != "" {
			body["stored_revision_id"] = raced.Stored
		}
		writeJSON(w, http.StatusPreconditionFailed, body)
		return
	}
	s.refuseApply(w, err)
}

// refuseApply maps a write's failure onto this surface's answers.
//
// ONE MAPPING, so a programmatic caller and an HTTP one cannot disagree about
// what a stale base, an unreadable patch or an invalid company means.
func (s *Service) refuseApply(w http.ResponseWriter, err error) {
	var raced *RacedError
	var patchErr *PatchError
	var invalid *ValidationError
	switch {
	case errors.Is(err, ErrNoActiveRevision):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "no_active_revision",
			"hint": "there is nothing to patch; import a company first, or " +
				"use PUT /config to send a whole document",
		})
	case errors.As(err, &raced):
		body := map[string]any{
			"error": "revision_advanced",
			"hint": "another write activated first; re-read /config and send " +
				"the edit again",
		}
		if raced.Base != "" {
			body["your_base"] = raced.Base
		}
		if raced.Stored != "" {
			body["stored_revision_id"] = raced.Stored
			body["hint"] = "another write activated first; this revision was " +
				"stored but not activated. Re-read /config and send the edit again"
		}
		if raced.Current != "" {
			body["current_revision_id"] = raced.Current
		}
		writeJSON(w, http.StatusConflict, body)
	case errors.As(err, &patchErr):
		refuseDocument(w, codeInvalidPatch, patchErr.Err.Error(),
			"the patched document was refused; an unknown key in a "+
				"patch is refused here rather than ignored", err)
	case errors.As(err, &invalid):
		refuseDocument(w, codeValidationError, invalid.Err.Error(),
			"the WHOLE document a write produces is validated, not only "+
				"the part it changed, so a section that is fine on its own is "+
				"still refused when the company it leaves is invalid", err)
	default:
		s.fail(w, "apply the config", err)
	}
}

// refuseDocument answers a refused document with its detail, its hint, and the
// structured half every surface shares ([RefusalFields]).
func refuseDocument(w http.ResponseWriter, code httpjson.Code, detail, hint string, err error) {
	fields := RefusalFields(err)
	fields["detail"] = detail
	if hint != "" {
		fields["hint"] = hint
	}
	httpjson.FailWithFields(w, http.StatusBadRequest, code, fields)
}

// reload serves POST /config/reload.
//
// No body, and no If-Match: it changes nothing, so there is no edit to lose a
// race with. What it produces is a new revision carrying the SAME document,
// which advances the epoch and makes every node re-apply — re-reading the
// secret store as it does, which is the whole point.
func (s *Service) reload(w http.ResponseWriter, r *http.Request) {
	// HEADER ONLY, like revert: this route reads no body, and it already
	// knows what it did, so an unset summary defaults rather than
	// answering 400.
	applied, err := s.Reload(r.Context(), strings.TrimSpace(r.Header.Get("X-Summary")), operatorOf(r))
	if err != nil {
		s.refuseApply(w, err)
		return
	}
	writeApplied(w, applied)
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
	document, company, err := s.openDocument(target)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "unreadable_revision", "detail": err.Error(),
			"hint": "the target revision is sealed under a key that is no longer " +
				"in the keyring; restore it to the node's secrets.keys first",
		})
		return
	}
	prepared, err := s.prepare(r.Context(), draft{
		// VALIDATED SEPARATELY from the open, so each refusal says what is
		// true. Opening holds a stored revision to no rule, and a revert is
		// an apply: an old revision this build can no longer run is refused
		// naming the field, where the open used to fold it into the keyring
		// hint above.
		//
		// The RUNNABLE rules only, like every apply: an old revision that
		// breaks an admission rule added since still runs, and reverting to
		// a working company must not be refused over a rule it predates.
		// The answer's warnings name each one, and each node warns about
		// it when it applies the epoch.
		rules: (*config.Company).ValidateRunnable,
		// THE TARGET'S BYTES AS THEY WERE STORED. A revert carries the old
		// document, and this build's struct of it is not that document when
		// a newer peer wrote it: every field this build cannot represent
		// would be gone from the revision the fleet reverts to.
		build: func(base) (*config.Company, []byte, error) {
			return company, document, nil
		},
	})
	var invalid *ValidationError
	switch {
	case errors.As(err, &invalid):
		refuseDocument(w, codeValidationError, invalid.Err.Error(),
			"revision "+target.ID+" does not pass this build's "+
				"validation, so it cannot be re-activated as it stands; send a "+
				"corrected document with PUT /config instead", err)
		return
	case err != nil:
		s.refuseApply(w, err)
		return
	}
	// HEADER ONLY, and no [takeSummary]: this route reads no body, so there
	// is no `_summary` to lift and nothing to refuse for. A revert also
	// already knows what it did, so an unset header defaults rather than
	// answering 400, the one write here that can name itself.
	summary := r.Header.Get("X-Summary")
	if summary == "" {
		summary = "revert to " + target.ID
	}
	applied, err := s.commit(r.Context(), prepared, summary, operatorOf(r))
	if err != nil {
		s.refuseApply(w, err)
		return
	}
	writeApplied(w, applied)
}

// operatorOf is who a write on this request is attributed to.
//
// TOTAL, never empty: every route on this surface is always guarded, so a
// request reaching here carries a resolved principal — and the case that is
// left, a handler somebody mounted outside the guard, records the name config
// refuses to every real credential rather than an empty `created_by` that
// reads as a revision nobody wrote.
func operatorOf(r *http.Request) string {
	return auth.OperatorOf(r.Context())
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
	}, tracing.TraceOf(ctx))
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
//
// createOnly reports that the request asked for create-only semantics
// (`If-None-Match: *`), which a caller needs to answer a revision that appears
// after this check as the precondition would have. ok is false when the
// request has been answered.
func (s *Service) checkPrecondition(w http.ResponseWriter, r *http.Request, active store.Revision, found bool) (createOnly, ok bool) {
	// IF-NONE-MATCH FIRST, because `*` on a write is the create-only
	// precondition (RFC 9110 §13.1.2): "store this only if the company has
	// not been configured yet". It is the only spelling of that condition
	// this surface takes: every If-Match value but `*` is an entity-tag.
	if none := r.Header.Get("If-None-Match"); none != "" {
		createOnly = strings.TrimSpace(none) == "*"
		if !found {
			// THE FLEET'S POINTER TOO, not just this node's store. A node
			// that has joined a fleet and not reconciled yet, or whose
			// best-effort copy of the pointer failed, has an empty store
			// while the fleet runs a company. "Only if nothing is
			// configured" asked of the local store alone would let the
			// dashboard's create flow replace that company outright:
			// renaming it changes every seat id derived from the name, and
			// orphans all of their memory.
			return createOnly, !createOnly || s.checkFleetAbsent(w, r)
		}
		if matchesTag(none, etagOf(active)) {
			writeJSON(w, http.StatusPreconditionFailed, map[string]any{
				"error": "already_configured", "current_revision_id": active.ID,
				"hint": "If-None-Match asked for this write to land only on a " +
					"config that is not there; one is active",
			})
			return createOnly, false
		}
		return createOnly, true
	}

	expected := strings.TrimSpace(r.Header.Get("If-Match"))
	switch {
	case expected == "":
		// Unconditional, and permitted: a first import has nothing to
		// match against, and a script that owns the config outright has
		// no race to lose.
		return false, true
	case !found:
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{
			"error": "no_active_revision",
			"hint": "there is no revision to match against; retry without " +
				"If-Match, or send If-None-Match: * to require that",
		})
		return false, false
	case !matchesTag(expected, etagOf(active)):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "revision_advanced", "current_revision_id": active.ID,
			"your_base": expected,
		})
		return false, false
	default:
		return false, true
	}
}

// checkFleetAbsent reports whether the FLEET has no activation, answering the
// refusal itself when it has one.
//
// A plane that is not there is not an answer: this process cannot activate
// anything, so the write is refused with 503 further on, where that is what
// the caller needs to hear. A plane that cannot be READ is three-valued and
// the third value is refused rather than guessed: "the fleet has nothing" and
// "I could not ask" send a create-only write to opposite outcomes, and
// guessing the first is the one that overwrites a running company.
func (s *Service) checkFleetAbsent(w http.ResponseWriter, r *http.Request) bool {
	if s.plane == nil {
		return true
	}
	target, found, err := s.plane.Target(r.Context())
	switch {
	case err != nil:
		s.fail(w, "read the fleet's activation", err)
		return false
	case !found:
		return true
	}
	writeJSON(w, http.StatusPreconditionFailed, map[string]any{
		"error": "already_configured", "current_revision_id": target.RevisionID,
		"hint": "If-None-Match asked for this write to land only on a config " +
			"that is not there; the fleet is running revision " + target.RevisionID +
			", which this node has not caught up with yet. Read /config again " +
			"once it has, and edit that",
	})
	return false
}

// --- plumbing --------------------------------------------------------------

// open decrypts a stored revision into a config, holding it to NO rule.
//
// Every reader on this surface goes through here: GET /config, a revision
// read, a diff, and the prior a write restores masks from or splices into. A
// revision that fails validation must stay readable to all of them, or the
// document an operator needs to see and replace is the one thing the surface
// refuses to serve. A caller that ACTIVATES what it opened (reload, revert)
// validates it itself; see [config.DecodeCompany].
func (s *Service) open(revision store.Revision) (*config.Company, error) {
	_, company, err := s.openDocument(revision)
	return company, err
}

// openDocument is [Service.open] plus the unsealed bytes it decoded, which is
// what a write that must keep fields this build cannot represent works from.
func (s *Service) openDocument(revision store.Revision) ([]byte, *config.Company, error) {
	document, err := secrets.Open(s.cipher, revision.Payload)
	if err != nil {
		return nil, nil, err
	}
	company, err := config.DecodeCompany(document)
	if err != nil {
		return nil, nil, err
	}
	return document, company, nil
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

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return httpjson.ReadBody(w, r, MaxBodyBytes)
}

func refuseBody(w http.ResponseWriter, err error) { httpjson.Refuse(w, err) }

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

// writeJSON is [httpjson.Write] under this package's own name.
func writeJSON(w http.ResponseWriter, status int, body any) {
	httpjson.Write(w, status, body)
}
