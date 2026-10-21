package configapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// summaryHeader is what every write on this surface needs.
var summaryHeader = map[string]string{"X-Summary": "a patch"}

// patchOnly sends a patch and returns the stored document a node would apply.
func patchOnly(t *testing.T, s *surface, body string, headers map[string]string) *surface {
	t.Helper()
	res := s.do(t, http.MethodPatch, "/config", body, headers)
	if res.Code != http.StatusCreated {
		t.Fatalf("PATCH = %d %s", res.Code, res.Body.String())
	}
	return s
}

// ONE SECTION, SENT ALONE, CHANGES ONLY THAT SECTION.
//
// The whole point: `PUT /config` makes every edit company-wide, so changing
// one turn-engine knob meant re-sending every seat, provider and integration
// — and losing a concurrent edit to any of them.
func TestAPatchChangesOnlyWhatItNames(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	patchOnly(t, s, `{"mission": "ship the thing"}`, summaryHeader)

	document := s.activeDocument(t)
	if !strings.Contains(document, "ship the thing") {
		t.Fatalf("the patch did not apply: %s", document)
	}
	// EVERYTHING ELSE SURVIVED. A merge that replaced the document would
	// pass the assertion above and lose the company.
	for _, want := range []string{`"Acme"`, "zulu", "gitlab.example.com"} {
		if !strings.Contains(document, want) {
			t.Errorf("%s is gone from the document after a patch: %s", want, document)
		}
	}
}

// A NESTED KNOB IS REACHABLE WITHOUT RESENDING ITS SIBLINGS, which is the
// case the per-entity routes could never cover: turn_engine, learning and the
// budget blocks have no members to address.
func TestAPatchReachesANestedSectionWithoutItsSiblings(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)
	before := s.activeDocument(t)
	if !strings.Contains(before, "claude-sonnet-5") {
		t.Fatalf("fixture changed: %s", before)
	}

	patchOnly(t, s,
		`{"providers": {"llm": {"zulu": {"model": "claude-opus-5"}}}}`,
		summaryHeader)

	document := s.activeDocument(t)
	if !strings.Contains(document, "claude-opus-5") {
		t.Fatalf("the nested patch did not apply: %s", document)
	}
	// The sibling keys of the thing that changed are still there — a
	// shallow merge would have replaced the whole provider with a model.
	if !strings.Contains(document, "credential_bundle") {
		t.Errorf("the provider's auth was dropped by a nested patch: %s", document)
	}
}

// A CREDENTIAL A PATCH DID NOT MENTION IS NOT DISTURBED.
//
// The failure this guards is silent and total: a caller patches one field,
// the merge drops a ${VAR} it never named, and every seat authenticating
// through it fails hours later.
func TestAPatchLeavesUnmentionedCredentialsAlone(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	patchOnly(t, s, `{"mission": "unrelated"}`, summaryHeader)

	document := s.activeDocument(t)
	for _, want := range []string{"sk-literal", "${ROTATED}", signingSecret} {
		if !strings.Contains(document, want) {
			t.Errorf("a patch that named none of them dropped %q: %s", want, document)
		}
	}
}

// A MASK SENT BACK IS RESOLVED, not stored. A caller building a patch from a
// redacted GET must not replace a credential with "__redacted__".
func TestAPatchRestoresARedactedValue(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	patchOnly(t, s,
		`{"integrations": {"gitlab": {"signing_secret": "__redacted__"}}}`,
		summaryHeader)

	document := s.activeDocument(t)
	if strings.Contains(document, "__redacted__") {
		t.Fatalf("a mask was stored as the value: %s", document)
	}
	if !strings.Contains(document, signingSecret) {
		t.Errorf("the real secret did not survive: %s", document)
	}
}

// null DELETES, which is what makes a section removable at all. Without it a
// config surface can only add, and an operator eventually edits by hand.
func TestAPatchRemovesASectionWithNull(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	patchOnly(t, s, `{"integrations": {"gitlab": null}}`, summaryHeader)

	if document := s.activeDocument(t); strings.Contains(document, "gitlab.example.com") {
		t.Fatalf("the section survived a null: %s", document)
	}
}

// AN UNKNOWN KEY IS REFUSED RATHER THAN IGNORED. A patch is the edit least
// visible in a diff, so a typo that silently changes nothing is the worst
// outcome available — the caller believes they changed something.
func TestAPatchWithAnUnknownKeyIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)
	before := s.activeDocument(t)

	res := s.do(t, http.MethodPatch, "/config",
		`{"missiom": "typo"}`, summaryHeader)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("PATCH = %d %s, want 400", res.Code, res.Body.String())
	}
	if s.activeDocument(t) != before {
		t.Error("a refused patch still wrote a revision")
	}
}

// A PATCH IS VALIDATED AS THE WHOLE DOCUMENT IT PRODUCES. A section that is
// fine on its own is still refused when it leaves the company invalid, which
// is the break a narrower surface invites: the caller never sees the rest.
func TestAPatchThatBreaksTheCompanyIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)
	before := s.activeDocument(t)

	// A signing secret whose SHAPE the validator checks. Chosen over
	// "delete the provider every seat names", which is deliberately
	// ACCEPTED: a company with no models at all is a documented authoring
	// state (an org chart written before the credentials exist), and every
	// node applies it, holding each seat's work until a provider returns.
	res := s.do(t, http.MethodPatch, "/config",
		`{"integrations": {"gitlab": {"signing_secret": "not-a-signing-secret"}}}`,
		summaryHeader)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("PATCH = %d %s, want 400", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "validation_error") {
		t.Errorf("body = %s", res.Body.String())
	}
	if s.activeDocument(t) != before {
		t.Error("an invalid patch still wrote a revision")
	}
}

// EVERY WRITE NEEDS A SUMMARY, and a patch most of all: it is the change a
// reader of the history can least reconstruct from the revision itself.
func TestAPatchNeedsASummary(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	res := s.do(t, http.MethodPatch, "/config", `{"mission": "x"}`, nil)
	if res.Code != http.StatusBadRequest ||
		!strings.Contains(res.Body.String(), "summary_required") {
		t.Fatalf("PATCH = %d %s", res.Code, res.Body.String())
	}
	// The body key works too, the same as on the full write.
	patchOnly(t, s, `{"_summary": "via the body", "mission": "x"}`, nil)
}

// If-Match IS HONOURED, and it matters more here than on the full write: a
// patch is merged against whatever is active at that instant, so without it
// two patches to one section resolve by arrival order with nothing telling
// the loser.
func TestAPatchHonoursIfMatch(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	res := s.do(t, http.MethodPatch, "/config", `{"mission": "x"}`,
		map[string]string{"X-Summary": "a patch", "If-Match": "not-the-active-one"})
	if res.Code != http.StatusPreconditionFailed {
		t.Fatalf("PATCH = %d %s, want 412: the condition the request stated is false",
			res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "current_revision_id") {
		t.Errorf("the conflict does not name what to re-read: %s", res.Body.String())
	}
}

// WITH NOTHING ACTIVE THERE IS NOTHING TO PATCH, and building a company out
// of one section is not what this route is for.
func TestAPatchWithNoActiveRevisionIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)

	res := s.do(t, http.MethodPatch, "/config", `{"mission": "x"}`, summaryHeader)
	if res.Code != http.StatusConflict {
		t.Fatalf("PATCH = %d %s, want 409", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "PUT /config") {
		t.Errorf("the refusal does not name the route that can: %s", res.Body.String())
	}
}

// AN EMPTY OR NON-OBJECT PATCH IS REFUSED rather than minting an epoch every
// node in the fleet reconciles onto for nothing.
func TestAnEmptyOrScalarPatchIsRefused(t *testing.T) {
	t.Parallel()
	// The `want` is the phrase the refusal must carry, not merely a 400:
	// a scalar body is ALSO refused downstream by the strict parser, with
	// a message about the document shape that says nothing about what a
	// patch is. Asserting the wording is what keeps the guard doing the
	// work rather than the parser doing it worse.
	for _, tc := range []struct{ body, want string }{
		{"", "empty"},
		{"   ", "empty"},
		{"null", "empty"},
		{`"just a string"`, "must be an object"},
		{`[1,2]`, "must be an object"},
	} {
		s := newSurface(t)
		s.seed(t, companyDoc)
		before := s.activeDocument(t)

		res := s.do(t, http.MethodPatch, "/config", tc.body, summaryHeader)
		if res.Code != http.StatusBadRequest {
			t.Errorf("PATCH %q = %d %s, want 400", tc.body, res.Code, res.Body.String())
		}
		if !strings.Contains(res.Body.String(), tc.want) {
			t.Errorf("PATCH %q said %s, want it to name %q",
				tc.body, res.Body.String(), tc.want)
		}
		if s.activeDocument(t) != before {
			t.Errorf("PATCH %q wrote a revision", tc.body)
		}
	}
}

// YAML WORKS TOO, for the same reason the full write accepts it: it is the
// form an operator edits, and one reader takes both.
func TestAPatchAcceptsYAML(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	patchOnly(t, s, "mission: from yaml\n", summaryHeader)
	if !strings.Contains(s.activeDocument(t), "from yaml") {
		t.Errorf("a YAML patch did not apply: %s", s.activeDocument(t))
	}
}

// A WRITE THAT LOST A RACE IS TOLD, rather than silently overwriting.
//
// There is no leader on this surface: any node's API may write the config.
// Before the activation became a compare-and-set, two operators editing at
// the same moment both got 201 and the later write won — a lost edit with a
// success in the loser's hand and nothing anywhere to find it by.
//
// Driven by moving the pointer out from under a write the way a second node
// would — between this surface's read of the pointer and its compare-and-set —
// which is the only thing a single-process test can do honestly: the
// conformance suite proves the store's half on both backends.
func TestAWriteThatLostTheRaceIsRefused(t *testing.T) {
	t.Parallel()
	plane := &interleavedPlane{}
	s := newSurfaceWith(t, func(o *configapi.Options) {
		plane.Plane = o.Plane
		o.Plane = plane
	})
	base := s.seed(t, companyDoc)
	peer, err := secrets.Seal(s.cipher, companyJSON(t, companyDoc))
	if err != nil {
		t.Fatal(err)
	}
	// A peer activates something else, expecting the same base this
	// surface built on.
	plane.other = func(ctx context.Context) {
		if _, err := plane.Plane.Activate(ctx, coord.ActivationRequest{
			RevisionID: "from-a-peer", Summary: "a peer got there first",
			Payload: peer, At: pinned, Expect: base,
			Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: "operator", Source: "api", CreatedAt: pinned},
		}); err != nil {
			t.Errorf("peer activate: %v", err)
		}
	}

	res := s.do(t, http.MethodPatch, "/config", `{"mission": "mine"}`, summaryHeader)
	if res.Code != http.StatusConflict {
		t.Fatalf("PATCH = %d %s, want 409", res.Code, res.Body.String())
	}
	for _, want := range []string{
		"revision_advanced",
		"stored_revision_id", // the operator's work survives as history
		"from-a-peer",        // what actually won, so they can re-read it
	} {
		if !strings.Contains(res.Body.String(), want) {
			t.Errorf("the conflict omits %q: %s", want, res.Body.String())
		}
	}
	// AS HISTORY, and nothing more: the write activated nothing.
	s.assertKeptInert(t, decode(t, res))
	// And the company is what WON, on this node as on every other, before
	// any tick here has kept it: the re-read the refusal sends the operator
	// to answers the revision it names.
	if got := s.do(t, http.MethodGet, "/config", "", nil); got.Header().Get("ETag") != `"from-a-peer"` {
		t.Errorf("GET /config after the refusal answers %s, want the revision that won, "+
			`"from-a-peer"`, got.Header().Get("ETag"))
	}
}

// A LOST RACE IS A FAILED PRECONDITION EXACTLY WHERE THE REQUEST NAMED A
// REVISION (RFC 9110 §13.1.1, [configapi.RacedError.Status]): an If-Match that
// matched the fleet's revision and then lost the activation's compare-and-set
// stated a condition that was false before the write could land, 412, as the
// same tag refused at the check is; a write that named none lost a race it
// never declared a condition for, and `If-Match: *` declared one ("a company
// exists") the company that won still satisfies: 409, both. One code
// throughout, because what moved is the company's revision either way.
//
// The race lands between this surface's read of the pointer and its
// compare-and-set, as a second node's write does ([interleavedPlane]).
func TestALostRaceIsAFailedPreconditionOnlyWhereARevisionWasNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		ifMatch func(base string) string
		want    int
	}{
		{"no If-Match", func(string) string { return "" }, http.StatusConflict},
		{"If-Match: *", func(string) string { return "*" }, http.StatusConflict},
		{"If-Match naming the base", func(base string) string { return `"` + base + `"` },
			http.StatusPreconditionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, plane := newRacingSurface(t)
			base := s.seed(t, companyDoc)
			plane.other = peerWins(t, s, plane, base)
			headers := map[string]string{"X-Summary": "a patch"}
			if v := tc.ifMatch(base); v != "" {
				headers["If-Match"] = v
			}
			res := s.do(t, http.MethodPatch, "/config", `{"mission": "mine"}`, headers)
			if res.Code != tc.want {
				t.Fatalf("PATCH = %d %s, want %d", res.Code, res.Body.String(), tc.want)
			}
			body := decode(t, res)
			if body["error"] != "revision_advanced" || body["current_revision_id"] != "from-a-peer" {
				t.Errorf("refusal = %v, want revision_advanced naming from-a-peer", body)
			}
		})
	}
}

// A ROUTE THAT JUDGES NO REVISION LOSES ITS RACE AS A CONFLICT, whatever
// If-Match it carries: the header an entity write sends is the entity's own
// tag, which held, and a reload and a revert read no If-Match at all, so the
// revision each lost to is one the request never named. Decided by what the
// handler judged ([configapi.RacedError.Stated]) rather than by reading the
// header again, which told each of them a precondition it never stated had
// failed.
func TestARouteThatJudgesNoRevisionLosesItsRaceAsAConflict(t *testing.T) {
	t.Parallel()
	t.Run("an entity write under its own tag, losing every attempt", func(t *testing.T) {
		t.Parallel()
		losing := &losingPlane{}
		s := newSurfaceWith(t, func(o *configapi.Options) {
			losing.Plane = o.Plane
			o.Plane = losing
		})
		s.seed(t, entityDoc)
		read := s.do(t, http.MethodGet, "/config/mcp-servers/tracker", "", nil)
		tag := read.Header().Get("ETag")
		if read.Code != http.StatusOK || tag == "" {
			t.Fatalf("GET /config/mcp-servers/tracker = %d, ETag %q", read.Code, tag)
		}
		losing.armed.Store(true)
		res := s.do(t, http.MethodPut, "/config/mcp-servers/tracker", read.Body.String(),
			map[string]string{"X-Summary": "move the tracker server", "If-Match": tag})
		if res.Code != http.StatusConflict || decode(t, res)["error"] != "revision_advanced" {
			t.Errorf("PUT under the entity's own tag = %d %s, want 409 revision_advanced",
				res.Code, res.Body.String())
		}
	})
	for _, tc := range []struct {
		name string
		path func(first string) string
	}{
		{"a reload", func(string) string { return "/config/reload" }},
		{"a revert", func(first string) string { return "/config/revisions/" + first + "/revert" }},
	} {
		t.Run(tc.name+" carrying an If-Match it never reads", func(t *testing.T) {
			t.Parallel()
			s, plane := newRacingSurface(t)
			first := s.seed(t, companyDoc)
			patchOnly(t, s, `{"mission": "second"}`, summaryHeader)
			current, _, err := s.active(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			plane.other = peerWins(t, s, plane, current.ID)
			res := s.do(t, http.MethodPost, tc.path(first), "", map[string]string{
				"X-Summary": tc.name, "If-Match": `"` + current.ID + `"`,
			})
			if res.Code != http.StatusConflict || decode(t, res)["error"] != "revision_advanced" {
				t.Errorf("POST %s = %d %s, want 409 revision_advanced", tc.path(first),
					res.Code, res.Body.String())
			}
		})
	}
}

// newRacingSurface is a surface whose plane lets a case land another writer's
// activation inside this surface's next one ([interleavedPlane]).
func newRacingSurface(t *testing.T) (*surface, *interleavedPlane) {
	t.Helper()
	plane := &interleavedPlane{}
	s := newSurfaceWith(t, func(o *configapi.Options) {
		plane.Plane = o.Plane
		o.Plane = plane
	})
	return s, plane
}

// peerWins is the other writer: a peer activating a sealed revision of its own,
// "from-a-peer", expecting base, as a write on another node does.
func peerWins(t *testing.T, s *surface, plane *interleavedPlane, base string) func(context.Context) {
	t.Helper()
	peer, err := secrets.Seal(s.cipher, companyJSON(t, companyDoc))
	if err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context) {
		if _, err := plane.Plane.Activate(ctx, coord.ActivationRequest{
			RevisionID: "from-a-peer", Summary: "a peer got there first",
			Payload: peer, At: pinned, Expect: base,
			Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: "operator", Source: "api",
				CreatedAt: pinned},
		}); err != nil {
			t.Errorf("peer activate: %v", err)
		}
	}
}

// AND A WRITE WITH NOTHING IN ITS WAY STILL LANDS, or the guard above is
// just a way of refusing every write on a fleet.
func TestAWriteWithNoRaceStillActivates(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	patchOnly(t, s, `{"mission": "mine"}`, summaryHeader)

	target, found, err := s.plane.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("Target: %v found=%v", err, found)
	}
	if !strings.Contains(s.activeDocument(t), "mine") {
		t.Fatalf("the write did not land: %s", s.activeDocument(t))
	}
	if target.RevisionID == "" {
		t.Error("the fleet was not pointed at the new revision")
	}
}

// A FIELD THIS BUILD DOES NOT KNOW SURVIVES A WRITE MADE BY THIS BUILD.
//
// A rolling upgrade puts two builds on one coordination store, and the
// activation pointer carries the payload — so an older node holds, byte for
// byte, a document a NEWER peer wrote. Decoding that into this build's struct
// and marshalling it back drops every field this build cannot represent, and
// the older node then publishes the result as the fleet's configuration: the
// newer peers reconcile onto it and lose settings nobody edited.
//
// The same rule the event envelope already keeps, and for the same reason —
// it is a contract between PEERS, which no tag count makes optional.
func TestAPatchKeepsFieldsThisBuildCannotRepresent(t *testing.T) {
	t.Parallel()
	s := newSurface(t)

	seedWithPeerField(t, s)

	patchOnly(t, s, `{"mission": "ship the thing"}`, summaryHeader)

	after := s.activeDocument(t)
	if !strings.Contains(after, "ship the thing") {
		t.Fatalf("the patch did not apply: %s", after)
	}
	if !strings.Contains(after, "a_setting_from_a_newer_build") {
		t.Errorf("a newer peer's setting was dropped by a write from this build:\n%s", after)
	}
}

// AND A TYPO IS STILL REFUSED, even on a document a newer peer extended.
//
// The two rules meet here: tolerate a key the stored bytes already carried,
// refuse one the patch invented. Read leniently across the board, the typo
// guard would be gone; read strictly, a peer's field disables the surface.
// What decides is where the unknown key came from.
func TestATypoIsRefusedEvenOnAPeerExtendedDocument(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	seedWithPeerField(t, s)

	res := s.do(t, http.MethodPatch, "/config",
		`{"missionn": "ship the thing"}`, summaryHeader)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("a misspelled key was accepted: %d %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "missionn") {
		t.Errorf("the refusal does not name the key that is wrong: %s", res.Body)
	}
}

// seedWithPeerField makes the active revision a document carrying a setting
// this build has no type for — what an older node holds after a newer peer
// wrote one, byte for byte, through the activation pointer.
//
// Seeded as RAW BYTES, because the struct is precisely what cannot carry it.
func seedWithPeerField(t *testing.T, s *surface) {
	t.Helper()
	s.seedStored(t, companyDoc, func(document map[string]any) {
		document["a_setting_from_a_newer_build"] = map[string]any{"depth": 3}
	})
}

// seedStored activates a revision as STORED BYTES: the document parsed from
// doc, marshalled, and then edited as generic JSON by mutate.
//
// It is how a test holds what this build could not have written itself: a
// field only a newer peer knows, or a document the validator refuses, which
// a newer peer's rules leave in a store. Nothing here validates
// the document or the result, which is the point. It returns the revision id.
func (s *surface) seedStored(t *testing.T, doc string, mutate func(map[string]any)) string {
	t.Helper()
	cfg, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	known, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(known, &document); err != nil {
		t.Fatal(err)
	}
	mutate(document)
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Seal(s.cipher, raw)
	if err != nil {
		t.Fatal(err)
	}
	return s.seedPayload(t, payload)
}

// A QUOTED ENTITY-TAG IS THE STANDARD SPELLING, and it has to work.
//
// `/setup`'s force-disconnect forwards the raw `If-Match` header into
// ApplyRequest.Expect, and a legal entity-tag is quoted — which never equals a
// bare revision id. A caller doing exactly what the HTTP spec says was
// answered 409, naming their own current revision as the thing they had raced
// with.
func TestAnExpectAcceptsEitherSpellingOfThePrecondition(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	id := s.seed(t, companyDoc)

	for name, expect := range map[string]string{
		"a bare revision id":   id,
		"a quoted entity-tag":  `"` + id + `"`,
		"a list containing it": `"nope", "` + id + `"`,
		"the wildcard":         "*",
	} {
		t.Run(name, func(t *testing.T) {
			// Serially, because each write advances the revision: the
			// point is the SPELLING, so each case starts from its own.
			current, found, err := s.active(t.Context())
			if err != nil || !found {
				t.Fatalf("active: %v", err)
			}
			sent := strings.ReplaceAll(expect, id, current.ID)
			if _, err := s.svc.Apply(t.Context(), configapi.ApplyRequest{
				Patch:   []byte(`{"mission": "ship the thing"}`),
				Summary: "a patch", By: iam.Actor{Name: "operator", Kind: iam.ActorOperator}, Expect: sent,
			}); err != nil {
				t.Errorf("Apply with %s (%q) = %v", name, sent, err)
			}
		})
	}
}

// AND A STALE ONE IS STILL REFUSED, or the tolerance above would have removed
// the guard rather than widened it.
func TestAStalePreconditionIsStillRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	_, err := s.svc.Apply(t.Context(), configapi.ApplyRequest{
		Patch:   []byte(`{"mission": "ship the thing"}`),
		Summary: "a patch", By: iam.Actor{Name: "operator", Kind: iam.ActorOperator},
		Expect: `"a-revision-that-never-existed"`,
	})
	var raced *configapi.RacedError
	if !errors.As(err, &raced) {
		t.Fatalf("Apply on a stale base = %v, want a RacedError", err)
	}
}
