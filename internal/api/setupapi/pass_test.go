package setupapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// THE SURFACE LEASE IS STILL HELD WHEN THE PASS'S OUTCOME IS WRITTEN.
//
// The lease is what makes these rows safe to write, and the write that
// records a pass — the phase, the findings, the attempts — used to happen
// OUTSIDE it: Execute took the guard and gave it back the moment the pass
// returned, so a reconcile tick that had read the row a moment earlier folded
// its own copy back over the top. An operator's disconnect landing in that
// window was silently overwritten and the card went from Disconnecting back
// to connected.
//
// Observable only from inside the write, because the write is the last thing
// the handler does and the release is a frame later. So the store asks for the
// guard itself: being GIVEN it is proof nobody is holding it.
func TestTheSurfaceLeaseIsHeldWhenThePassIsRecorded(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	// THE REQUIREMENTS SATISFIED, because a pass refuses to run against a
	// half-configured integration and this case is about what happens after
	// one runs.
	for _, name := range []string{"GL_SIGN", "GL_ADMIN"} {
		if err := s.vault.Set(t.Context(), name,
			"whsec_Y3Jld2xldC10ZXN0LXNpZ25pbmcta2V5LTMyYnl0ZXM=",
			"test", "test", pinned); err != nil {
			t.Fatal(err)
		}
	}
	status, runner := s.withPass(t, &recordingPass{kind: integration.KindGitLab})

	var writes, unguarded int
	status.mu.Lock()
	status.onSave = func(state integration.State) {
		if state.Kind != integration.KindGitLab {
			return
		}
		writes++
		_, release, held, err := runner.Hold(context.Background(), integration.KindGitLab)
		if err != nil {
			return
		}
		if held {
			unguarded++
			release()
		}
	}
	status.mu.Unlock()

	out := s.do(t, http.MethodPost, "/setup/integrations/gitlab/provision", `{}`, nil)
	if out.Code != http.StatusOK {
		t.Fatalf("provision = %d: %s", out.Code, out.Body)
	}
	if writes == 0 {
		t.Fatal("the premise is wrong: the pass recorded nothing")
	}
	if unguarded != 0 {
		t.Errorf("%d of %d status write(s) happened with the surface guard "+
			"free, so a reconcile tick can fold its own row over the top of "+
			"this one", unguarded, writes)
	}
}

// A RUN ID WAS UNNAMEABLE, so the route that serves one was reachable only by
// the caller that had just started it.
//
// `GET /setup/integrations/{kind}/runs/{id}` has always answered one pass with
// its findings, and nothing anywhere could tell a reader an id — so an
// operator opening a Findings tab had no way in. The listing is what makes the
// route addressable.
func TestThePassesOfOneSurfaceAreListable(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	for _, name := range []string{"GL_SIGN", "GL_ADMIN"} {
		if err := s.vault.Set(t.Context(), name,
			"whsec_Y3Jld2xldC10ZXN0LXNpZ25pbmcta2V5LTMyYnl0ZXM=",
			"test", "test", pinned); err != nil {
			t.Fatal(err)
		}
	}
	s.withPass(t, &recordingPass{kind: integration.KindGitLab})

	// BEFORE ANY PASS: an empty list, never null, and it says whose
	// history it is — a node that ran nothing here is an honest answer to
	// a question the reader did not mean to ask.
	before := s.do(t, http.MethodGet, "/setup/integrations/gitlab/runs", "", nil)
	if before.Code != http.StatusOK {
		t.Fatalf("runs = %d: %s", before.Code, before.Body)
	}
	var empty struct {
		Runs  []map[string]any `json:"runs"`
		Scope string           `json:"scope"`
	}
	if err := json.Unmarshal(before.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode the empty listing: %v", err)
	}
	if empty.Runs == nil {
		t.Error("runs is null before any pass, so a client rendering its " +
			"length has to guard the field as well")
	}
	if empty.Scope == "" {
		t.Error("the listing does not say whose history it is, and it is " +
			"this node's rather than the fleet's")
	}

	if out := s.do(t, http.MethodPost, "/setup/integrations/gitlab/provision",
		`{}`, nil); out.Code != http.StatusOK {

		t.Fatalf("provision = %d: %s", out.Code, out.Body)
	}

	after := s.do(t, http.MethodGet, "/setup/integrations/gitlab/runs", "", nil)
	var listed struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(after.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode the listing: %v", err)
	}
	if len(listed.Runs) != 1 {
		t.Fatalf("the listing has %d runs after one pass: %s",
			len(listed.Runs), after.Body)
	}
	id, _ := listed.Runs[0]["run_id"].(string)
	if id == "" {
		t.Fatal("the listed run carries no id, which is the whole point of it")
	}

	// AND THE ID ADDRESSES THE RUN, which is what the listing exists for.
	one := s.do(t, http.MethodGet, "/setup/integrations/gitlab/runs/"+id, "", nil)
	if one.Code != http.StatusOK {
		t.Fatalf("runs/%s = %d: %s", id, one.Code, one.Body)
	}

	// A SURFACE THIS BUILD DOES NOT KNOW is a 404 rather than an empty
	// list, because an empty list would say "this node ran no passes for
	// your typo".
	if bad := s.do(t, http.MethodGet, "/setup/integrations/nowhere/runs", "",
		nil); bad.Code != http.StatusNotFound {

		t.Errorf("an unknown surface listed runs with %d", bad.Code)
	}
}
