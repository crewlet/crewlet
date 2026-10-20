package artifactsapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

const checkBody = `{"kind":"html","entry":"index.html","files":[{"path":"index.html","text":"<p>\"hi\"</p>"}]}`

// A CHECK IS ANSWERED 200 WITH WHAT A PUBLISH WOULD SAY, refusals included: a
// refused page is the answer a person writing it needs, not an error. It takes
// the artifacts app, as a create does, and no operation key, since it writes
// nothing.
func TestACheckAnswersTheFindingsAndWritesNothing(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.pipeline.report = artifactupload.CheckReport{
		Refusals: []lint.Finding{{Path: "index.html", Line: 2, Code: lint.CodeRemoteScript, Message: "no"}},
		Warnings: []lint.Finding{},
	}
	noApp := person("dana")
	noApp.Grants = nil
	if w := g.do(noApp, "POST", "/artifacts/checks", checkBody); w.Code != http.StatusForbidden {
		t.Errorf("a person without the app checking: %d %s", w.Code, w.Body)
	}
	w := g.do(person("dana"), "POST", "/artifacts/checks", checkBody)
	if w.Code != http.StatusOK {
		t.Fatalf("a check: %d %s", w.Code, w.Body)
	}
	got := fields(t, w)
	refusals, _ := got["refusals"].([]any)
	if len(refusals) != 1 || refusals[0].(map[string]any)["line"] != float64(2) {
		t.Errorf("the check answered %v", got)
	}
	if files := g.pipeline.checks[0]; len(files) != 1 || string(files[0].Text) != `<p>"hi"</p>` {
		t.Errorf("the pipeline was asked to check %+v", files)
	}
	if verbs := g.pipeline.verbs(); len(verbs) != 1 || verbs[0] != "check" {
		t.Errorf("a check asked the pipeline %v, want the check alone", verbs)
	}
	if len(g.writes.all()) != 0 {
		t.Errorf("a check wrote %v", g.writes.all())
	}
}

// A CHECK THE PIPELINE REFUSES WHOLE is answered as a publish's refusal is,
// with no operation id, since there is no operation to retry.
func TestARefusedCheckCarriesNoOperation(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.pipeline.err = artifactupload.ErrInvalid
	w := g.do(person("dana"), "POST", "/artifacts/checks", checkBody)
	if w.Code != http.StatusUnprocessableEntity || fields(t, w)["error"] != string(httpjson.CodeInvalid) {
		t.Fatalf("a refused check: %d %s", w.Code, w.Body)
	}
	if _, ok := fields(t, w)["op_id"]; ok {
		t.Errorf("a refused check names an operation: %s", w.Body)
	}
}

// A CHECK IS HELD TO A BUDGET: a client checking on every keystroke is told
// when to come back, with a Retry-After, and never locked out.
func TestChecksAreHeldToABudget(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	var last int
	for range 30 {
		w := g.do(person("dana"), "POST", "/artifacts/checks", checkBody)
		if last = w.Code; w.Code == http.StatusTooManyRequests {
			if w.Header().Get("Retry-After") == "" || fields(t, w)["error"] != string(httpjson.CodeCheckBudget) {
				t.Errorf("a throttled check: %s %s", w.Header(), w.Body)
			}
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatal("thirty checks at once were all answered, want the budget to hold some")
	}
	if w := g.do(person("bob"), "POST", "/artifacts/checks", checkBody); w.Code != http.StatusOK {
		t.Errorf("another person's check, held by dana's budget: %d %s", w.Code, w.Body)
	}
}

// A CHECK'S BODY IS BOUNDED before it is read whole.
func TestACheckBodyIsBounded(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	huge := `{"kind":"html","files":[{"path":"index.html","text":"` +
		strings.Repeat("a", 3*artifactupload.MaxCheckBytes+1) + `"}]}`
	if w := g.do(person("dana"), "POST", "/artifacts/checks", huge); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized check: %d", w.Code)
	}
	if len(g.pipeline.checks) != 0 {
		t.Error("an oversized check reached the pipeline")
	}
}
