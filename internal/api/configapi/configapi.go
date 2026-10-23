// Package configapi serves /config: the versioned company document, its
// history, and the write path that activates a new revision.
//
// EVERY ROUTE HERE IS ADMIN'S, reads included — `config.read` for every read
// and `config.write` for every write, both decided on admin (ADR-0033) —
// mounted through [authz.Router] so a route with no policy fails the boot.
// Reading this surface exposes the whole company document — its org chart,
// integrations and the name of every credential it holds — and writing it
// changes the company.
//
// THE ORG CHART IS ALSO ITS LEADS'. The routes that can change a seat or a
// unit — the whole-document writes, a revert and the seat and unit entity
// routes — are `config.org.write`, and the two entity reads
// `config.org.read`: admin as before, or a lead inside the units they lead.
// What a lead's write changes is decided place by place (admission.go), and a
// setting, a credential or a `${VAR}` is never theirs.
//
// THE ROUTES USED TO DECIDE NOTHING, which was sound while the only credential
// was an operator token and stopped being sound the day a person could sign in
// holding a read-only permission alone: every one of them could rewrite the
// company document, revert it to any revision, and read every `${VAR}` it
// names.
//
// A WRITE HERE DOES NOT APPLY ANYTHING. It stores a revision and moves the
// activation pointer; every node, including this one, applies it on its own
// reconcile tick. That is what makes a write on one node reach the whole
// fleet — the failure the control plane exists to remove was a config change
// that only the process handling the request ever saw.
//
// EVERY ANSWER IS THE FLEET'S, never this node's. GET /config and its
// entity-tag, the revision list's `is_active`, a diff against `active` and
// the base every write is built on are all the revision the activation
// pointer names ([Service.current]), the same on every node whatever that
// node runs: a node behind the pointer, or one that refused its revision,
// still answers the company, so a write it serves lands, and the read after a
// write answers the write. What a node boots on and what it serves are the
// reconciler's facts (configplane's package doc, ADR-0057), and the fleet
// view says them per node. A coordination store that cannot be read answers
// 503 with Retry-After rather than a guess, because "the fleet has no
// company" sends a write to replace it. Every other condition of the node, a
// store too busy to take a write's revision, one closed for a snapshot, a
// request out of time, is 503 too, in a person's words for it
// ([operator.AnswerCondition]) and under a Retry-After exactly where waiting
// clears it: only an error no condition claims is the fault a 500 says. NO
// READ WRITES: a revision this node
// holds no row for yet is answered from the body the pointer carries, and the
// reconciler keeps it.
//
// A WRITE STORED AND THEN UNANSWERED IS AN UNKNOWN OUTCOME. The revision is
// stored before the pointer moves, so an activation the coordination store
// did not answer may have landed: it is 503 with `outcome: unknown` naming the
// stored revision and the node whose history alone holds it
// ([ActivationUnknownError], [RefuseActivationUnknown]), whichever surface the
// write came through, never the plain 503 that says nothing was written and
// sends a client to write a second revision over one that may already be the
// company. A write that lost its activation keeps its revision too, inert, and
// its 409 or 412 names that node beside it the same way ([KeptRevision]):
// every answer naming a stored revision names the one node it can be diffed and
// reverted on.
//
// A DEPLOYMENT MAY NAME WHO WRITES IT. When Tier A lists
// `api.auth.company_writers`, the document is managed by the system holding
// those tokens and every other credential is refused a change to it, here,
// on every path onto it — ADR-0030, and managed.go for what counts as a
// change.
package configapi

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/words"
)

var log = logging.Get("api.config")

// MaxBodyBytes bounds a config upload.
//
// The document is the whole company — its org chart, providers, integrations,
// MCP servers and worker templates: the largest real one in this repository is
// tens of kilobytes, so 4 MiB is two orders of magnitude of headroom and still
// finite. The route is guarded, so this is a bound on a
// mistake rather than on an attacker.
const MaxBodyBytes = 4 << 20

// DefaultPage and MaxPage bound a history listing.
const (
	DefaultPage = store.DefaultRevisionPage
	MaxPage     = 500
)

// revisionStore is the node's revision history, as this surface reads and
// writes it.
//
// NOTHING HERE READS OR MOVES WHAT THIS NODE BOOTS ON, and the interface is
// what holds that: the company this surface answers for is the fleet's pointer
// ([Service.current]), and the boot row is the reconciler's alone to move,
// once its engine has applied a revision. A write here stores its revision as
// history and moves the pointer; the reconciler makes it what each node boots
// on as each applies it.
type revisionStore interface {
	Get(ctx context.Context, revisionID string) (store.Revision, bool, error)
	List(ctx context.Context, limit, offset int) ([]store.Revision, error)
	InsertOn(ctx context.Context, base, r store.Revision) (string, error)
	NoteActivation(ctx context.Context, revisionID string, at time.Time) error
	Restorable(ctx context.Context) (store.Revision, bool, error)
}

// Service is the /config surface.
type Service struct {
	configs revisionStore
	plane   coord.Plane
	queue   queue.Publisher
	cipher  secrets.Cipher
	holders Holders
	// writers is the deployment's managed-document policy: the one field
	// of Tier A's `api.auth` this surface reads, through
	// [config.APIAuth.MayWriteCompany], the one reading of it.
	writers config.APIAuth
	// node is this node's id ([Options.Node]), which an unknown outcome and a
	// lost race name as the one node holding the revision they stored.
	node string
	now  func() time.Time
}

// Options wire the service.
type Options struct {
	// Node is this node's id, the engine's own name for it — the one its
	// presence lease and `/health` carry. Required: a write stores its
	// revision in this node's history alone, so an answer that cannot say
	// whether the activation landed ([ActivationUnknownError]), and one whose
	// activation lost to another write ([RacedError]), names this node as the
	// one to diff or revert the stored revision on. Behind a
	// shared address a client sent there by "the node that answered" reaches
	// another, which holds no copy and answers 404, and concludes the write
	// is lost.
	Node string

	// Store holds the revisions. Required: every node that serves this
	// surface opens one, and [New] refuses to build without it.
	Store *store.DB

	// Plane publishes the fleet's activation pointer. Required: storing a
	// revision nothing points at activates nothing, and a caller that got
	// a 201 back would believe otherwise. Every node holds one, because
	// the fleet store is opened on every topology.
	Plane coord.Plane

	// Cipher seals every revision this surface writes and opens every one
	// it reads, under the keyring every node holds — the engine's own,
	// which the reconciler authenticates a peer's revision under.
	// Required: [New] refuses to build without it.
	Cipher secrets.Cipher

	// Holders reads what holds a human seat — whoever the identity
	// directory binds to it, or an open invitation onto it — so a write
	// that takes a human seat away while it is held is refused naming the
	// holder (seatheld.go). Required: every node serving this surface holds
	// the estate and runs the identity domain from boot, and a surface that
	// skipped the check would remove a
	// colleague's seat from under them, or an invitee's from under their
	// link, with nothing said.
	Holders Holders

	// CompanyWriters is Tier A's `api.auth.company_writers`: the ids of the
	// Tier A tokens that alone may change the company document (managed.go,
	// ADR-0030). Empty leaves every write to the authority table.
	//
	// THE ONE FIELD, never the whole Tier A document: nothing else of the
	// deployment's is this surface's to read, and a surface handed all of
	// it has a way to start deciding on any of it.
	CompanyWriters []string

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
// A MISSING STORE, PLANE OR KEYRING IS REFUSED rather than served as a narrower
// surface. `crewlet run` builds this beside an engine that holds all three, so a
// nil here is a wiring mistake, and a surface that quietly shrank around it (an
// unregistered /config, a write answering 503) would hide exactly that.
//
// The keyring most of all, because the surface built without one did not
// shrink, it changed meaning: it stored every revision in plaintext, which
// every node holding a keyring — every node — refuses to apply, so the fleet
// converged on nothing while this surface answered 201. The e2e harness was
// wired that way until it was noticed by hand.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("configapi: Options.Store is required: the revisions " +
			"live in the node's store, which the engine opens")
	case opts.Plane == nil:
		return nil, errors.New("configapi: Options.Plane is required: a revision " +
			"takes effect only once the fleet's activation pointer names it")
	case opts.Cipher == nil:
		return nil, errors.New("configapi: Options.Cipher is required: every " +
			"revision is sealed under the keyring (secrets.keys) every node holds, " +
			"and one written without it is a revision no node applies")
	case opts.Holders == nil:
		return nil, errors.New("configapi: Options.Holders is required: a write " +
			"that removes a human seat is refused while the identity directory " +
			"binds somebody to it or an open invitation holds it, and that " +
			"needs the directory")
	case strings.TrimSpace(opts.Node) == "":
		return nil, errors.New("configapi: Options.Node is required: a write whose " +
			"activation goes unanswered names the node whose history alone holds " +
			"the revision it stored, and the engine's node id is that name")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		configs: opts.Store.Configs(), plane: opts.Plane,
		cipher: opts.Cipher, holders: opts.Holders, queue: opts.Queue, now: now,
		writers: config.APIAuth{CompanyWriters: slices.Clone(opts.CompanyWriters)},
		node:    opts.Node,
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
	// Within, so the table this surface is mounted on keeps each route's
	// verb although it holds only the two prefixes below.
	router := authz.NewRouter(authz.Within(routes, mux), authz.ContextGuard(authz.NoChart{}))
	var failures []error
	mount := func(pattern string, a authz.Action, h http.HandlerFunc) {
		if err := router.Handle(pattern, authz.Policy{Action: a}, h); err != nil {
			failures = append(failures, err)
		}
	}
	read := func(pattern string, h http.HandlerFunc) {
		mount(pattern, authz.ActionConfigRead, h)
	}
	// THE ORG CHART'S ROUTES ARE ITS LEADS' TOO. The route admits whoever
	// could lead some unit — admin, or anybody bound to a
	// seat — before the body is read, and what the write changes is then
	// decided place by place against the two documents (admission.go), a
	// read against where the seat or unit sits.
	orgPolicy := func(a authz.Action) authz.Policy {
		return authz.Policy{Action: a, Object: func(*http.Request) authz.Object {
			return authz.Object{Kind: authz.KindUnit, Unresolved: true}
		}}
	}
	org := func(pattern string, a authz.Action, h http.HandlerFunc) {
		if err := router.Handle(pattern, orgPolicy(a), h); err != nil {
			failures = append(failures, err)
		}
	}
	// A DOCUMENT WRITE ANSWERS `dry_run=true` AND REQUIRES A SUMMARY, which
	// its route declares (ADR-0055): [dryRunOf] and [requireSummary] are
	// what such a handler reads, and the declaration is what a caller (the
	// assistant's preview) reads instead of the handler.
	documentWrite := func(pattern string, h http.HandlerFunc) {
		if err := router.Handle(pattern, orgPolicy(authz.ActionOrgWrite).Checked().Summarised(),
			h); err != nil {
			failures = append(failures, err)
		}
	}
	// AN ENTITY'S WRITE SAYS WHAT IT DOES AT AN EMPTY ADDRESS, from the
	// same table its handler decides by ([EntityAbsent]).
	entityPolicy := func(kind string, a authz.Action) authz.Policy {
		p := authz.Policy{Action: a}
		if inOrgChart(kind) {
			p.Object = func(*http.Request) authz.Object {
				return authz.Object{Kind: authz.KindUnit, Unresolved: true}
			}
		}
		// AN ENTITY'S PUT REPLACES IT whole, in the shape its GET answers.
		return p.WhenAbsent(EntityAbsent(kind)).Checked().Summarised().Replacing()
	}
	entity := func(pattern string, p authz.Policy, h http.HandlerFunc) {
		if err := router.Handle(pattern, p, h); err != nil {
			failures = append(failures, err)
		}
	}
	read("GET /config", s.getActive)
	// THE WHOLE DOCUMENT IS REPLACED by a PUT, in the shape GET /config
	// answers: what the body leaves out is removed.
	if err := router.Handle("PUT /config",
		orgPolicy(authz.ActionOrgWrite).Checked().Summarised().Replacing(),
		http.HandlerFunc(s.put)); err != nil {
		failures = append(failures, err)
	}
	// WHAT THIS RESOURCE TAKES, asked rather than guessed. RFC 5789 §3.1:
	// a patch format is negotiated, not assumed, and Accept-Patch is where
	// a server says which ones it speaks.
	read("OPTIONS /config", s.optionsDocument)
	// THE NARROWER WRITE. See merge.go for why one patch route covers
	// every section rather than one route per section.
	documentWrite("PATCH /config", s.patch)
	// RE-PUBLISH THE ACTIVE DOCUMENT UNCHANGED, which is the gesture a
	// rotated SECRET needs and the one thing no other route on this
	// surface performs: the pointer in the config is already correct, so
	// there is no patch to make, and with no activation there is no apply
	// and no refreshed secret snapshot. See [Service.Reload].
	if err := router.Handle("POST /config/reload",
		authz.Policy{Action: authz.ActionConfigWrite}.Rewriting(),
		http.HandlerFunc(s.reload)); err != nil {
		failures = append(failures, err)
	}
	// WHICH FIELDS NAME A ${VAR}, which is what an operator needs before
	// they remove a credential. See [Service.References].
	read("GET /config/references", s.references)
	// THE CHARTER, one resource of three fields: what Company › Principles
	// edits in place. See charter.go for why it is not PATCH /config, and
	// for why it is decided on verbs of its own.
	mount("GET /config/charter", authz.ActionPrinciplesRead, s.getCharter)
	// A CHARTER WRITE CHECKS ON `dry_run=true` and names its own summary
	// when it is sent none ([charterSummary]), so it declares only the check.
	if err := router.Handle("PUT /config/charter",
		authz.Policy{Action: authz.ActionPrinciplesWrite}.Checked().Replacing(),
		http.HandlerFunc(s.putCharter)); err != nil {
		failures = append(failures, err)
	}
	read("GET /config/revisions", s.listRevisions)
	read("GET /config/revisions/{id}", s.getRevision)
	read("GET /config/revisions/{id}/diff", s.diff)
	// A REVERT IS JUDGED ON ITS WHOLE DIFF, from the active revision to the
	// one reverted to, like any other write.
	if err := router.Handle("POST /config/revisions/{id}/revert",
		orgPolicy(authz.ActionOrgWrite).Rewriting(), http.HandlerFunc(s.revert)); err != nil {
		failures = append(failures, err)
	}
	// THE ENTITY ROUTES, one pair per addressable collection rather than a
	// single {kind} wildcard: a wildcard would also match
	// /config/revisions/{id}, and a route that answers for a path it was
	// never meant to serve is worse than a line per collection. See
	// entities.go for what a write does.
	for _, kind := range EntityKinds() {
		// THE READ AND THE WRITE ON ONE URI. The entity was addressable
		// for writing long before it was readable here, so the documented
		// loop fetched from /query/config_entities — a different URI
		// space, answering a {kind, id, entity} envelope that PUT does
		// not accept. GET here answers the entity itself, so `GET | PUT`
		// round-trips with nothing in between.
		if inOrgChart(kind) {
			org("GET /config/"+kind+"/{id}", authz.ActionOrgRead, s.getEntity(kind))
			entity("PUT /config/"+kind+"/{id}", entityPolicy(kind, authz.ActionOrgWrite),
				s.putEntity(kind))
			continue
		}
		read("GET /config/"+kind+"/{id}", s.getEntity(kind))
		entity("PUT /config/"+kind+"/{id}", entityPolicy(kind, authz.ActionConfigWrite),
			s.putEntity(kind))
	}
	// AND THOSE TWO ARE JSON, like every other answer here: see
	// [httpjson.Mux].
	surface := noStore(httpjson.Mux(routes))
	mux.Handle("/config", surface)
	mux.Handle("/config/", surface)
	return errors.Join(failures...)
}

// noStore marks every response it wraps as never to be stored.
//
// EVERY ONE, reads, refusals and 304s alike. A /config body is the company's
// settings: which integrations are wired and where, the ${VAR} name behind every
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
		refuseNoCompany(w)
	case err != nil:
		s.fail(w, r, "read the active revision", err)
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
		refuseNoCompany(w)
	case err != nil:
		s.fail(w, r, "read the active revision", err)
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
		s.fail(w, r, "encode the config as yaml", err)
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
	httpjson.FailWithFields(w, http.StatusUnsupportedMediaType,
		httpjson.CodeUnsupportedPatchMediaType, httpjson.Detail{
			"accept_patch": acceptPatch,
			"you_sent":     media,
			"hint": "PATCH /config takes a JSON Merge Patch (RFC 7396): an object " +
				"shaped like the document. A JSON Patch (RFC 6902) list of " +
				"operations is a different format this surface does not serve. " +
				"Editing one list member is its own route: editing one seat is " +
				"PUT /config/roles/{handle}, and PUT /config/{kind}/{id} " +
				"edits one member of " + words.Or(EntityKinds()),
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
		s.fail(w, r, "list revisions", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// getRevision serves GET /config/revisions/{id} — one revision with its
// redacted payload.
func (s *Service) getRevision(w http.ResponseWriter, r *http.Request) {
	revision, ok := s.lookup(w, r, r.PathValue("id"), httpjson.CodeNotFound)
	if !ok {
		return
	}
	company, err := s.open(revision)
	if err != nil {
		s.fail(w, r, "open revision", err)
		return
	}
	cur, _, err := s.current(r.Context())
	if err != nil {
		s.fail(w, r, "read the fleet's revision", err)
		return
	}
	body := meta(revision.revision, cur.revision.ID)
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
		refuseNoCompany(w)
	case errors.Is(err, store.ErrNoRevision):
		// WHICH side is missing, read off the error rather than guessed
		// from the request — see [missingRevision].
		httpjson.Fail(w, http.StatusNotFound, missingSide(err))
	default:
		s.fail(w, r, "diff revisions", err)
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
func missingSide(err error) httpjson.Code {
	var missing *missingRevision
	if errors.As(err, &missing) {
		return missing.side
	}
	return sideTarget
}

// --- writes ----------------------------------------------------------------

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
	return flagOf(w, r, "dry_run", "dry_run=true validates the write and answers "+
		"what it would produce without storing or activating anything; leave it "+
		"out to write")
}

// importOf reads `import` from a PUT /config query, answering the refusal
// itself; ok is false when the request has been answered. See
// [Service.mayImport] for what it changes.
func importOf(w http.ResponseWriter, r *http.Request) (importing, ok bool) {
	return flagOf(w, r, "import", "import=true writes the document as a restored "+
		"file, whose charter faults are warned about rather than refused; leave "+
		"it out for an edit")
}

// flagOf reads a boolean query parameter: exactly `true` or `false`, given
// once, or absent for false. Anything else is refused naming the parameter,
// because the two readings of a guess write different things.
func flagOf(w http.ResponseWriter, r *http.Request, name, hint string) (set, ok bool) {
	values, present := r.URL.Query()[name]
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
		"detail": name + " must be true or false, given once",
		"hint":   hint,
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
	if s.refusedManaged(w, r) {
		return
	}
	dryRun, ok := dryRunOf(w, r)
	if !ok {
		return
	}
	importing, ok := importOf(w, r)
	if !ok || (importing && !s.mayImport(w, r)) {
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
	summary, sent, ok := takeSummary(w, r, body)
	if !ok {
		return
	}

	incoming, err := sent.company()
	if err != nil {
		refuseDocument(w, httpjson.CodeInvalidBody, err.Error(), "", &DocumentError{Err: err})
		return
	}

	cur, found, err := s.current(r.Context())
	if err != nil {
		s.fail(w, r, "read the fleet's revision", err)
		return
	}
	cond, ok := s.checkPrecondition(w, r, cur.revision, found)
	if !ok {
		return
	}
	built := ""
	if found {
		built = cur.revision.ID
	}
	d := replaceDraft(incoming, built, attributionOf(r))
	d.stated = cond.stated
	if importing {
		d.rules, d.source = importRules, importSource
	}
	if d.principal, ok = principalOf(w, r); !ok {
		return
	}
	prepared, err := s.prepare(r.Context(), d)
	if err != nil {
		s.refuseWrite(w, r, err, cond.createOnly)
		return
	}
	if dryRun {
		writeChecked(w, prepared)
		return
	}
	if !requireSummary(w, summary, "PUT /config needs an audit summary: the "+
		"X-Summary header, or a top-level _summary key in the body. The "+
		"revision history is the record of who changed what and why") {
		return
	}
	applied, err := s.commit(r.Context(), prepared, summary)
	if err != nil {
		s.refuseWrite(w, r, err, cond.createOnly)
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
	if s.refusedManaged(w, r) {
		return
	}
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
	summary, sent, ok := takeSummary(w, r, body)
	if !ok {
		return
	}

	cur, found, err := s.current(r.Context())
	if err != nil {
		s.fail(w, r, "read the fleet's revision", err)
		return
	}
	if !found {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoActiveRevision,
			map[string]string{
				"hint": "there is nothing to patch; import a company first, or " +
					"use PUT /config to send a whole document",
			})
		return
	}
	cond, ok := s.checkPrecondition(w, r, cur.revision, found)
	if !ok {
		return
	}

	d := patchDraft(ApplyRequest{
		Patch: sent.text, Summary: summary, By: attributionOf(r), Expect: cur.revision.ID,
		Stated: cond.stated,
	}, sent.doc)
	if d.principal, ok = principalOf(w, r); !ok {
		return
	}
	prepared, err := s.prepare(r.Context(), d)
	if err != nil {
		s.refuseApply(w, r, err)
		return
	}
	if dryRun {
		writeChecked(w, prepared)
		return
	}
	if !requireSummary(w, summary, "PATCH /config needs an audit summary: the "+
		"X-Summary header, or a top-level _summary key in the body. A patch "+
		"is the change least visible in a diff, so the sentence saying what "+
		"it was for matters most here") {
		return
	}
	applied, err := s.commit(r.Context(), prepared, summary)
	if err != nil {
		s.refuseApply(w, r, err)
		return
	}
	writeApplied(w, applied)
}

// writeChecked answers a dry run: the write is valid, what it was checked
// against, and what it would produce, each warning marked with whether this
// write introduces it ([checkedWarnings]). Nothing was stored.
//
// base_revision_id is the revision the check was built on, and empty when
// nothing was active. A client comparing it with the revision its draft was
// built on learns about a write that landed between its read and this check
// without a second request.
func writeChecked(w http.ResponseWriter, p *prepared) {
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": true, "base_revision_id": p.base.ID,
		"warnings": checkedWarnings(p), "derived": p.derived,
	})
}

// writeApplied answers a write that stored and activated a revision.
func writeApplied(w http.ResponseWriter, applied Applied) {
	writeJSON(w, http.StatusCreated, map[string]any{
		"revision_id": applied.RevisionID, "epoch": applied.Epoch,
		"warnings": applied.Warnings, "derived": applied.Derived,
	})
}

// refuseWrite is [Service.refuseApply] for a write that may have been
// create-only.
//
// A create-only write that finds a revision where it expected none is not a
// lost edit to re-derive; it is a company that exists, so it is answered as
// its precondition would have been had the revision been there to see.
func (s *Service) refuseWrite(w http.ResponseWriter, r *http.Request, err error, createOnly bool) {
	var raced *RacedError
	if createOnly && errors.As(err, &raced) {
		hint := "If-None-Match asked for this write to land only on a " +
			"config that is not there; one was activated first"
		if raced.Stored != "" {
			hint += "; " + keptSentence(raced)
		}
		body := KeptRevision(raced)
		body["hint"] = hint
		// NAMED ONLY WHEN KNOWN, as on every other refusal here: the
		// pointer is re-read after a lost activation, and a read that
		// failed has no revision to name. An empty id is not "none", and a
		// client fetching /config/revisions/ to see what won would be
		// sent to a route that is not there.
		if raced.Current != "" {
			body["current_revision_id"] = raced.Current
		}
		httpjson.FailWithFields(w, http.StatusPreconditionFailed,
			httpjson.CodeAlreadyConfigured, body)
		return
	}
	s.refuseApply(w, r, err)
}

// refuseApply maps a write's failure onto this surface's answers.
//
// ONE MAPPING, so a programmatic caller and an HTTP one cannot disagree about
// what a stale base, an unreadable patch or an invalid company means.
func (s *Service) refuseApply(w http.ResponseWriter, r *http.Request, err error) {
	var raced *RacedError
	var patchErr *PatchError
	var invalid *ValidationError
	var held *SeatHeldError
	var unknown *HoldersUnavailableError
	var unanswered *ActivationUnknownError
	var refused *AdmissionError
	var managed *ManagedError
	switch {
	// THE CREDENTIAL'S REFUSAL FIRST, as [Service.prepare] decides it: a
	// managed document refuses whoever is asking before anything they sent
	// is judged, so no other refusal can be answered in its place.
	case errors.As(err, &managed):
		RefuseManaged(w, managed)
	case errors.As(err, &refused):
		refuseAdmission(w, refused)
	case errors.Is(err, ErrNoActiveRevision):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoActiveRevision,
			map[string]string{
				"hint": "there is nothing to patch; import a company first, or " +
					"use PUT /config to send a whole document",
			})
	case errors.As(err, &raced):
		body := httpjson.Detail{
			"hint": "another write activated first; re-read /config and send " +
				"the edit again",
		}
		if raced.Base != "" {
			body["your_base"] = raced.Base
		}
		if raced.Stored != "" {
			maps.Copy(body, KeptRevision(raced))
			body["hint"] = "another write activated first; " + keptSentence(raced) +
				". Re-read /config and send the edit again"
		}
		if raced.Current != "" {
			body["current_revision_id"] = raced.Current
		}
		httpjson.FailWithFields(w, raced.Status(), httpjson.CodeRevisionAdvanced, body)
	case errors.As(err, &patchErr):
		refuseDocument(w, httpjson.CodeInvalidPatch, patchErr.Err.Error(),
			"the patched document was refused; an unknown key in a "+
				"patch is refused here rather than ignored", err)
	case errors.As(err, &invalid):
		refuseDocument(w, httpjson.CodeValidationError, invalid.Err.Error(),
			"the WHOLE document a write produces is validated, not only "+
				"the part it changed, so a section that is fine on its own is "+
				"still refused when the company it leaves is invalid", err)
	case errors.As(err, &held):
		// A REMEDY PER KIND OF HOLDER, because a person is never unbound —
		// they hold a human seat for as long as they exist (ADR-0032) — so
		// the one remedy this used to name, unbinding them, was refused by
		// the very directory it sent an administrator to. Each `held` entry
		// says which kind holds its seat.
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeSeatHeld,
			httpjson.Detail{
				"detail": held.Error(),
				"held":   held.Held,
				"hint": "move each person to another human seat (PATCH " +
					"/iam/people/{person} with seat) or remove them, unbind a " +
					"service account (PATCH /iam/people/{person} with no seat), " +
					"cancel an open invitation (DELETE /iam/invitations/{id}), " +
					"each an admin's gesture, then send the write again",
			})
	case errors.As(err, &unanswered):
		RefuseActivationUnknown(w, unanswered, nil)
	case errors.As(err, &unknown):
		log.Warn("config_seat_holders_unreadable",
			"seats", unknown.Seats, "error", unknown.Err)
		// WHAT CLEARS IT, in the words that agree with the hint: the
		// identity estate's own sentence where its log refused the read (a
		// node evicted, a log full, which no wait clears), as every identity
		// 503 is told (auth.UnavailableIdentity). A fixed sentence told a
		// stuck refusal only that waiting wouldn't help, never what would.
		said, ok := statelog.ConditionSentence(unknown.Err)
		if !ok {
			said = "This node couldn't read who holds the human seats this " +
				"change removes, so nothing was written. Try again in a moment."
		}
		httpjson.UnavailableWith(w, httpjson.CodeSeatHoldersUnavailable,
			auth.RetryIdentity(unknown.Err), httpjson.Detail{
				"seats": unknown.Seats, "detail": said,
			})
	default:
		s.fail(w, r, "apply the config", err)
	}
}

// RefuseActivationUnknown answers a write that was stored and whose activation
// was not answered: an unknown outcome ([httpjson.UnknownOutcome]) naming the
// stored revision, never "nothing was written", whichever surface the write
// came through.
//
// NO OPERATION ID, because a config write carries none: what tells the caller
// whether it landed is the pointer, which `GET /config` reads, naming the
// stored revision or not. Sending the write again makes another revision —
// over this one, if it landed — so the hint says how to look first, and how
// to make the stored revision the company if it did not land.
//
// AND THE NODE, because that remedy is one node's. The revision is stored in
// the history of the node that took the write and nowhere else, and only an
// activation naming it carries it to the others: if it did not land, its diff
// and its revert answer on that node alone, and every other answers 404. A
// client behind a shared address told only to revert it reached another node,
// was told the revision does not exist, and concluded the write was lost. So
// the body names the node (`node`) and the hint sends both requests there —
// what the command line's own unknown answer and a withheld offline change's
// remedy already said.
//
// extra is a surface's own detail beside it — the setup surface's names of the
// credentials it sealed first — and never wins over the revision, the node or
// the hint.
func RefuseActivationUnknown(w http.ResponseWriter, e *ActivationUnknownError, extra httpjson.Detail) {
	body := make(httpjson.Detail, len(extra)+4)
	maps.Copy(body, extra)
	maps.Copy(body, httpjson.Detail{
		"stored_revision_id": e.Stored,
		"node":               e.Node,
		"hint": "revision " + e.Stored + " was stored on node " + e.Node + ", and the " +
			"coordination store did not answer its activation, so whether the fleet runs " +
			"it is unknown. GET /config names the active revision once the store answers " +
			"again. If it is not " + e.Stored + ", the revision is in node " + e.Node +
			"'s history alone: send GET /config/revisions/" + e.Stored + "/diff, and POST " +
			"/config/revisions/" + e.Stored + "/revert to make it the company, to that " +
			"node — by its own address where a shared one fronts several, since any other " +
			"node holds no copy and answers 404. Sending the write again makes another " +
			"revision",
	})
	if e.Base != "" {
		body["your_base"] = e.Base
	}
	httpjson.UnknownOutcome(w, authz.RetryUndecidedSeconds, "", false, body)
}

// KeptRevision is what an answer to a write that lost its activation says of
// the revision the write left behind: its id, `stored_revision_id`, and the node
// that stored it, `node` — both, or neither when the race was caught before
// anything was stored. The config surface's 409 and 412 carry it, and so does
// the setup surface's 409, from the one place.
//
// THE NODE BESIDE THE ID, for [RefuseActivationUnknown]'s reason: the kept
// revision is a row of that node's history alone, since only an activation
// naming it would have carried it to the others, and this one lost. A client
// behind a shared address that went to revert to it, as the answer invites,
// reached another node, was told it does not exist, and concluded the work was
// gone.
func KeptRevision(e *RacedError) httpjson.Detail {
	if e.Stored == "" {
		return httpjson.Detail{}
	}
	return httpjson.Detail{"stored_revision_id": e.Stored, "node": e.Node}
}

// keptSentence is the hint's account of a kept revision: where it is, and that
// it is reached there alone.
func keptSentence(e *RacedError) string {
	return "this write was kept, inert, as revision " + e.Stored + " in node " + e.Node +
		"'s history alone: GET /config/revisions/" + e.Stored + "/diff and POST " +
		"/config/revisions/" + e.Stored + "/revert reach it on that node — by its own " +
		"address where a shared one fronts several — and any other node answers 404"
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
	applied, err := s.Reload(r.Context(), strings.TrimSpace(r.Header.Get("X-Summary")),
		attributionOf(r), "")
	if err != nil {
		s.refuseApply(w, r, err)
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
	if s.refusedManaged(w, r) {
		return
	}
	target, ok := s.lookup(w, r, r.PathValue("id"), httpjson.CodeNotFound)
	if !ok {
		return
	}
	// OPENED, not copied. A revision this node cannot open cannot be
	// reverted to, and finding that out now beats activating a document
	// every node will fail to read. The refusal is the one every reader of
	// a revision answers ([RefuseUnreadable]).
	document, company, err := s.openDocument(target)
	if err != nil {
		if !RefuseUnreadable(w, err) {
			s.fail(w, r, "open the revision to revert to", err)
		}
		return
	}
	principal, ok := principalOf(w, r)
	if !ok {
		return
	}
	prepared, err := s.prepare(r.Context(), draft{
		by: attributionOf(r),
		// VALIDATED SEPARATELY from the open, so each refusal says what is
		// true. Opening holds a stored revision to no rule, and a revert is
		// an apply: an old revision this build can no longer run is refused
		// naming the field, where the open used to fold it into the keyring
		// hint above.
		//
		// The RUNNABLE rules only, like every apply: an admission rule is
		// one an apply does not enforce, since a newer peer may have
		// admitted the revision under rules this build does not share, and
		// reverting to a company that runs must not be refused over one.
		// The answer's warnings name each one, and each node warns about
		// it when it applies the epoch.
		rules: runnableRules,
		// THE TARGET'S BYTES AS THEY WERE STORED. A revert carries the old
		// document, and this build's struct of it is not that document when
		// a newer peer wrote it: every field this build cannot represent
		// would be gone from the revision the fleet reverts to.
		build: func(base) (*config.Company, []byte, error) {
			return company, document, nil
		},
		principal: principal,
	})
	var invalid *ValidationError
	switch {
	case errors.As(err, &invalid):
		refuseDocument(w, httpjson.CodeValidationError, invalid.Err.Error(),
			"revision "+target.revision.ID+" does not pass this build's "+
				"validation, so it cannot be re-activated as it stands; send a "+
				"corrected document with PUT /config instead", err)
		return
	case err != nil:
		s.refuseApply(w, r, err)
		return
	}
	// HEADER ONLY, and no [takeSummary]: this route reads no body, so there
	// is no `_summary` to lift and nothing to refuse for. A revert also
	// already knows what it did, so an unset header defaults rather than
	// answering 400, the one write here that can name itself.
	summary := r.Header.Get("X-Summary")
	if summary == "" {
		summary = "revert to " + target.revision.ID
	}
	applied, err := s.commit(r.Context(), prepared, summary)
	if err != nil {
		s.refuseApply(w, r, err)
		return
	}
	writeApplied(w, applied)
}

// attributionOf is who a write on this request is attributed to: the author,
// its kind and the credential beside it.
//
// TOTAL, never empty: every route on this surface is always guarded, so a
// request reaching here carries a resolved principal — and the case that is
// left, a handler somebody mounted outside the guard, records the name config
// refuses to every real credential rather than an empty `created_by` that
// reads as a revision nobody wrote.
func attributionOf(r *http.Request) iam.Actor {
	return auth.AttributionOf(r.Context())
}

// revisionSource is the source every revision this surface writes records,
// but an import's.
const revisionSource = "api"

// importSource is the source an import records ([Service.mayImport]): the
// one an offline `crewlet config import` records, since both are a file
// somebody imported rather than an edit made here.
const importSource = "file"

// mayImport decides `PUT /config?import=true`, answering the refusal itself;
// it reports whether the write may go on.
//
// # An import is a restore, not an edit
//
// `crewlet config import` against a running node sends the file here. It is
// how a company is restored and migrated, and a file that ran under an earlier
// build must import today: judged as an edit, a file whose charter still
// carries a fault in a part that differs from the stored company's (a policy
// a newer revision rewrote, restored to its older, longer self) was refused,
// and the restore with it. So an import judges the charter WARN-ONLY
// ([importRules]), exactly as an offline import and a boot import do; every
// other rule holds as it does for any write, and its faults are named in the
// answer's warnings.
//
// ADMIN'S, never a lead's: the route admits a lead for a write inside their
// own units, and an import replaces the whole document and relaxes a rule, so
// it is asked of the company (`config.write`, a recent proof included) before
// the body is read. It is recorded as any write is (its author, kind and
// credential, the summary and the activation event), under [importSource].
// The dashboard never sends it: the org builder and the Principles editor are
// edits, and an edit is judged on what it changes.
func (s *Service) mayImport(w http.ResponseWriter, r *http.Request) bool {
	p, ok := principalOf(w, r)
	if !ok {
		return false
	}
	policy := authz.Policy{Action: authz.ActionConfigWrite}
	d := authz.Decide(r.Context(), *p, policy.Action,
		authz.Object{Kind: authz.KindCompany}, authz.NoChart{}, s.now())
	if d.Unknown() || !d.Allowed {
		authz.EnvelopeRefusal(w, r, policy, d)
		return false
	}
	return true
}

// nudge tells every node an activation happened.
//
// BEST EFFORT and deliberately thin — the event carries no payload, because
// the authoritative path is the pointer and a node acts by re-reading it. That
// is what makes losing one cost a poll interval rather than a revision, and
// what makes an ephemeral broadcast the right delivery: every node has to
// hear it, and none of them has to.
func (s *Service) nudge(ctx context.Context, revisionID, summary, source string, by iam.Actor) {
	if s.queue == nil {
		return
	}
	ev := events.New(types.ConfigRevisionActivated{
		RevisionID: revisionID, RevisionSummary: summary, RevisionSource: source,
		CreatedBy: by.Name, CreatedByKind: string(by.Kind), OperatorID: by.OperatorID,
	}, tracing.TraceOf(ctx))
	ev.Timestamp = s.now()
	ev.Source = by.Name
	if err := s.queue.Publish(ctx, topics.ConfigRevisionActivated, ev); err != nil {
		log.WarnContext(ctx, "activation_nudge_not_published", "revision", revisionID,
			"error", err, "detail", "peers converge on their reconcile interval instead")
	}
}

// condition is what a write's precondition headers asked of the fleet's
// revision, as [Service.checkPrecondition] judged them, carried on to the
// write's refusal so that the refusal is decided by what was judged rather than
// by reading the request again.
type condition struct {
	// createOnly is `If-None-Match: *`: store this only where no company is
	// configured, which a caller needs to answer a revision that appears
	// after the check as the precondition would have.
	createOnly bool
	// stated is an If-Match naming the revision the write is built on: a
	// write that then loses its activation's compare-and-set failed the
	// precondition the request stated ([RacedError.Stated]).
	stated bool
}

// checkPrecondition enforces If-Match, the optimistic-concurrency guard,
// against the fleet's revision ([Service.current]).
//
// Two operators editing one company through a full-document PUT is a
// last-writer-wins race that silently discards the other's change. If-Match
// turns it into a 412 the loser can see, and the document they need to re-read
// is named in the answer.
//
// The answer is the condition the request stated ([condition]), which the
// write carries on to its refusal; ok is false when the request has been
// answered.
func (s *Service) checkPrecondition(w http.ResponseWriter, r *http.Request, active store.Revision, found bool) (cond condition, ok bool) {
	expected := strings.TrimSpace(r.Header.Get("If-Match"))
	// IF-NONE-MATCH FIRST, because `*` on a write is the create-only
	// precondition (RFC 9110 §13.1.2): "store this only if the company has
	// not been configured yet". It is the only spelling of that condition
	// this surface takes: every If-Match value but `*` is an entity-tag.
	if none := r.Header.Get("If-None-Match"); none != "" {
		// NEVER BESIDE IF-MATCH, as an entity write refuses the pair: the
		// two ask opposite things of one company, and taking either alone
		// would judge a condition the caller stated as though it had not
		// been sent, so a stale If-Match beside it was never refused.
		if expected != "" {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeConflictingPreconditions,
				map[string]string{
					"hint": "If-None-Match: * asks for a first import and If-Match names " +
						"the revision an edit was built on; send one",
				})
			return condition{}, false
		}
		cond.createOnly = strings.TrimSpace(none) == "*"
		if !found {
			// NOTHING IS CONFIGURED, asked of the fleet's pointer first —
			// [Service.current] reads it, so a node that joined a fleet and
			// has not reconciled yet answers the company the fleet runs. A
			// pointer published after this read refuses the write at the
			// compare-and-set, which is create-only for a write built on
			// nothing, and answers as this would have.
			return cond, true
		}
		if matchesTag(none, etagOf(active)) {
			httpjson.FailWithFields(w, http.StatusPreconditionFailed,
				httpjson.CodeAlreadyConfigured, httpjson.Detail{
					"current_revision_id": active.ID,
					"hint": "If-None-Match asked for this write to land only on a " +
						"config that is not there; one is active",
				})
			return cond, false
		}
		return cond, true
	}

	switch {
	case expected == "":
		// NO PRECONDITION, and permitted: a first import has nothing to
		// match against, and a script that owns the config outright need
		// not read before it writes. The activation is a compare-and-set
		// all the same — built on the revision current() answers, expecting
		// it (or expecting none) — so a write that lands between that read
		// and the activation is answered 409 revision_advanced, never
		// replaced: only the HTTP precondition is waived, not the race.
		return condition{}, true
	case !found:
		httpjson.FailWith(w, http.StatusPreconditionFailed, httpjson.CodeNoActiveRevision,
			map[string]string{
				"hint": "there is no revision to match against; retry without " +
					"If-Match, or send If-None-Match: * to require that",
			})
		return condition{}, false
	case !matchesTag(expected, etagOf(active)):
		// THE CALLER'S OWN CONDITION FAILED, so 412 — as an If-Match
		// against no company is above and If-None-Match against one is,
		// and as a commit the tag lost to after this check is
		// ([RacedError.Status]).
		httpjson.FailWithFields(w, http.StatusPreconditionFailed, httpjson.CodeRevisionAdvanced,
			httpjson.Detail{"current_revision_id": active.ID, "your_base": expected})
		return condition{}, false
	default:
		// `*` NAMES NO REVISION: its condition, that a company exists, is
		// still true of whichever company wins a race.
		return condition{stated: expected != "*"}, true
	}
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
func (s *Service) open(revision fleetRevision) (*config.Company, error) {
	_, company, err := s.openDocument(revision)
	return company, err
}

// openDocument is [Service.open] plus the unsealed bytes it decoded, which is
// what a write that must keep fields this build cannot represent works from.
//
// A revision the keyring does not open is an [UnreadableRevisionError], told by
// where it came from: this node's own history, or a body off the fleet's
// pointer this node holds no row for ([fleetRevision.fleet]).
func (s *Service) openDocument(revision fleetRevision) ([]byte, *config.Company, error) {
	document, err := secrets.Open(s.cipher, revision.revision.Payload)
	if err != nil {
		return nil, nil, &UnreadableRevisionError{ID: revision.revision.ID, Fleet: revision.fleet, Err: err}
	}
	company, err := config.DecodeCompany(document)
	if err != nil {
		return nil, nil, err
	}
	return document, company, nil
}

// refuseNoCompany answers a read of the company where there is none: the
// fleet has no activation, and this node knows no company either
// ([Service.current]). 404 under the code whose message ends "The detail says
// how.", WITH the detail that says it: the reads answered the bare code, so
// nothing did.
func refuseNoCompany(w http.ResponseWriter) {
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoActiveRevision, map[string]string{
		"detail": "The fleet runs no company, and this node knows none. Send one with " +
			"PUT /config, or import a file with `crewlet config import`.",
	})
}

// lookup fetches a revision by id, answering the refusal itself.
func (s *Service) lookup(w http.ResponseWriter, r *http.Request, id string,
	missing httpjson.Code) (fleetRevision, bool) {

	if id == "" {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidRevisionID)
		return fleetRevision{}, false
	}
	revision, found, err := s.revision(r.Context(), id)
	if err != nil {
		s.fail(w, r, "read revision", err)
		return fleetRevision{}, false
	}
	if !found {
		httpjson.Fail(w, http.StatusNotFound, missing)
		return fleetRevision{}, false
	}
	return revision, true
}

// meta is a revision without its payload, `is_active` when it is the fleet's
// revision, whose id is fleets.
//
// THE FLEET'S, not the one this node boots on: the listing marks the company,
// which is what every other answer here calls active, and a node that refused
// the fleet's revision would otherwise mark the one it still runs — telling an
// operator the fleet is on a company it left.
func meta(revision store.Revision, fleets string) map[string]any {
	body := map[string]any{
		"revision_id": revision.ID,
		"created_at":  revision.CreatedAt.Format(time.RFC3339Nano),
		"created_by":  revision.CreatedBy,
		// THE KIND AND THE CREDENTIAL BESIDE THE AUTHOR, as every other
		// trail answers them. The kind is always one of iam's (the store
		// refuses a revision without one); the credential is empty on a
		// revision no credential made, such as the engine's own boot seed.
		"created_by_kind": revision.CreatedByKind,
		"operator_id":     revision.OperatorID,
		"source":          revision.Source,
		"summary":         revision.Summary,
		"is_active":       fleets != "" && revision.ID == fleets,
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
		// THE QUERY VOCABULARY'S OWN CODE: this was `invalid_pagination`,
		// a spelling of "a query parameter is not a value this route
		// accepts" nothing else answered with.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": name + " must be a number"})
		return 0, false
	}
	return value, true
}

// fail logs the reason and answers without it.
//
// The reason reaches the LOG, never the caller: a store error can carry a
// database path or a driver's own message, and this surface is the one an
// operator reaches from a browser.
//
// A CONDITION OF THE NODE IS ANSWERED AS ONE, never as the fault `500` says:
// a store busy with other writes, one closed while the node adopts a snapshot
// or shuts down, a deadline the request ran out of. Each is `503` in a
// person's words for it ([operator.AnswerCondition]), under a Retry-After
// exactly where waiting clears it; only an error no condition claims is a
// fault.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	// A REVISION THIS NODE CANNOT OPEN IS NOT A FAULT, and every route that
	// reads one reaches here: GET /config, a revision read, a diff, the
	// references, an entity, and a write whose prior is the active
	// revision. It was a 500 on all of them, which said nothing about a
	// state an operator can fix and whose fix depends on the revision.
	if RefuseUnreadable(w, err) {
		return
	}
	// THE FLEET'S POINTER COULD NOT BE READ, which every answer here is
	// about: not a fault of this node's, and retryable within a raft
	// election, so a client comes back rather than reading a 500 as a
	// broken install. Retried at the authorizer's own interval, which is
	// the one every undecided request on this node is answered with.
	//
	// A READ, before anything was stored: a write whose activation went
	// unanswered after it was stored is [RefuseActivationUnknown]'s, which
	// [Service.refuseApply] answers before it gets here.
	if errors.Is(err, coord.ErrUnavailable) {
		log.Warn("config_request_unavailable", "what", what, "error", err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			httpjson.Detail{"reason": "the fleet's activation could not be read " +
				"from the coordination store"})
		return
	}
	if operator.AnswerCondition(r.Context(), w, log, "config_request_unavailable", err, nil,
		"what", what) {
		return
	}
	log.ErrorContext(r.Context(), "config_request_failed", "what", what, "error", err)
	httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
}

// UnreadableRevisionError is a stored revision this node's keyring does not
// open: stored without a seal, or sealed under a key the keyring does not hold.
type UnreadableRevisionError struct {
	ID string
	// Fleet reports a body this node holds no copy of: the fleet's revision,
	// read off its pointer ([fleetRevision.fleet]). Its causes are the
	// fleet's rather than this node's history: a key the fleet seals under
	// that this node was never given, or a body somebody put on the pointer
	// without a seal.
	Fleet bool
	Err   error
}

func (e *UnreadableRevisionError) Error() string {
	return "configapi: revision " + e.ID + " does not open under this node's " +
		"keyring (" + e.Err.Error() + "): " + e.Remedy()
}

func (e *UnreadableRevisionError) Unwrap() error { return e.Err }

// Unsealed reports whether the revision was stored without a seal, rather
// than sealed under a key this keyring does not hold.
func (e *UnreadableRevisionError) Unsealed() bool {
	return errors.Is(e.Err, secrets.ErrUnsealedWithKey)
}

// Code is the refusal code: one per cause and origin, because an envelope's
// message is the code's sentence and the four share no remedy.
func (e *UnreadableRevisionError) Code() httpjson.Code {
	switch {
	case e.Fleet && e.Unsealed():
		return httpjson.CodeUnsealedFleetRevision
	case e.Fleet:
		return httpjson.CodeUnreadableFleetRevision
	case e.Unsealed():
		return httpjson.CodeUnsealedRevision
	default:
		return httpjson.CodeUnreadableRevision
	}
}

// Said is what happened, in a person's words: the refusal's `detail`, where
// the keyring's own error, the secrets package's words, names nothing a person
// changes and goes to the log.
func (e *UnreadableRevisionError) Said() string {
	switch {
	case e.Fleet && e.Unsealed():
		return "The fleet's revision arrived without a seal, so this node won't read it."
	case e.Fleet:
		return "This node doesn't hold the key the fleet sealed this revision under."
	case e.Unsealed():
		return "This node's copy of that revision is stored without a seal, so " +
			"nothing vouches for who wrote it."
	default:
		return "This node's keyring doesn't hold the key that revision is sealed under."
	}
}

// Remedy is what brings the revision's document back: the refusal's `hint`.
func (e *UnreadableRevisionError) Remedy() string {
	switch {
	case e.Fleet && e.Unsealed():
		return "every node seals what it activates, so this body came from " +
			"outside the fleet; send the document again with PUT /config, which " +
			"activates it sealed"
	case e.Fleet:
		return "add the key the fleet seals under to this node's secrets.keys " +
			"and restart it"
	case e.Unsealed():
		return "it is stored without a seal, so nothing vouches for who wrote it " +
			"and it is neither shown nor reverted to; to have its document again, " +
			"import it from your own copy with PUT /config or `crewlet config " +
			"import`, which stores it sealed"
	default:
		return "it is sealed under a key that is no longer in the keyring; " +
			"restore that key to the node's secrets.keys first"
	}
}

// RefuseUnreadable answers a revision this node cannot open, and reports
// whether err was one: on this surface, and on a surface built on it that
// reads the fleet's revision through [Service.FleetCompany].
//
// 409 under the code its cause and origin name ([UnreadableRevisionError.Code])
// — four codes and not one with four hints, because an envelope's message is
// the table's sentence and always wins: under one code, an unsealed revision
// was answered with a message telling the caller to put a key back that was
// never involved, and a body this node never held with one telling it to put
// back a key the deployment still has. It names the revision, what happened in
// a person's words as the detail ([UnreadableRevisionError.Said]) and the
// remedy as the hint; the keyring's own error goes to the log.
func RefuseUnreadable(w http.ResponseWriter, err error) bool {
	return RefuseUnreadableWith(w, err, nil)
}

// RefuseUnreadableWith is [RefuseUnreadable] with a surface's own detail beside
// it, nil for none: what a `/setup` submission sealed before it met the
// revision (`wrote_secrets`), which it must still be told.
func RefuseUnreadableWith(w http.ResponseWriter, err error, extra httpjson.Detail) bool {
	var unreadable *UnreadableRevisionError
	if !errors.As(err, &unreadable) {
		return false
	}
	log.Warn("config_revision_unreadable", "revision", unreadable.ID,
		"fleet", unreadable.Fleet, "error", err)
	detail := httpjson.Detail{}
	maps.Copy(detail, extra)
	detail["revision_id"] = unreadable.ID
	detail["detail"] = unreadable.Said()
	detail["hint"] = unreadable.Remedy()
	httpjson.FailWithFields(w, http.StatusConflict, unreadable.Code(), detail)
	return true
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
