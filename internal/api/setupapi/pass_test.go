package setupapi_test

import (
	"context"
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
