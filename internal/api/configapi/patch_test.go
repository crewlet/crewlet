package configapi_test

import (
	"net/http"
	"strings"
	"testing"
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
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	patchOnly(t, s, `{"mission": "ship the thing"}`, summaryHeader)

	document := s.activeDocument(t)
	if !strings.Contains(document, "ship the thing") {
		t.Fatalf("the patch did not apply: %s", document)
	}
	// EVERYTHING ELSE SURVIVED. A merge that replaced the document would
	// pass the assertion above and lose the company.
	for _, want := range []string{`"Acme"`, "zulu", "gitlab.example.com", "ceo"} {
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
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)
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
	if !strings.Contains(document, "anthropic") {
		t.Errorf("the provider's type was dropped by a nested patch: %s", document)
	}
}

// A CREDENTIAL A PATCH DID NOT MENTION IS NOT DISTURBED.
//
// The failure this guards is silent and total: a caller patches one field,
// the merge drops a ${VAR} it never named, and every seat authenticating
// through it fails hours later.
func TestAPatchLeavesUnmentionedCredentialsAlone(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

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
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

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
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

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
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)
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
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)
	before := s.activeDocument(t)

	// A signing secret whose SHAPE the validator checks. Chosen over
	// "delete the provider every seat names", which is deliberately
	// ACCEPTED: a company with no models at all is a documented authoring
	// state — an org chart written before the credentials exist — and it
	// fails at the first turn, where the failure is actionable.
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
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

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
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	res := s.do(t, http.MethodPatch, "/config", `{"mission": "x"}`,
		map[string]string{"X-Summary": "a patch", "If-Match": "not-the-active-one"})
	if res.Code != http.StatusConflict {
		t.Fatalf("PATCH = %d %s, want 409", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "current_revision_id") {
		t.Errorf("the conflict does not name what to re-read: %s", res.Body.String())
	}
}

// WITH NOTHING ACTIVE THERE IS NOTHING TO PATCH, and building a company out
// of one section is not what this route is for.
func TestAPatchWithNoActiveRevisionIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)

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
		s := newSurface(t, nil)
		s.seed(t, companyDoc, nil)
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
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	patchOnly(t, s, "mission: from yaml\n", summaryHeader)
	if !strings.Contains(s.activeDocument(t), "from yaml") {
		t.Errorf("a YAML patch did not apply: %s", s.activeDocument(t))
	}
}
