package engine

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CURSOR EXECUTOR'S LOGIN STAYS ON THE ENGINE HOST, ITS KEY GOES ANYWHERE:
// Cursor keeps its login in the home's auth.json and cli-config.json, which a
// local cell is seeded with and writes refreshed tokens back from, and which
// never travels; so an entry signed in by that login alone is refused a remote
// box, as a configuration to change, while a key in CURSOR_API_KEY signs it in
// on every cell.
//
// Mutation: drop the cursor-agent profile's api_key_env, or let a login-only
// entry reach e2b: this goes red.
func TestACursorExecutorsLoginStaysLocalAndItsKeyTravels(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "cursor-agent")
	for _, placement := range []sandbox.Placement{sandbox.Direct, sandbox.Container} {
		if err := guardOn(t, c, seat, placement, nil); err != nil {
			t.Errorf("a login-only cursor-agent run in %q was refused: %v", placement, err)
		}
	}
	err := guardOn(t, c, seat, sandbox.E2B, nil)
	var credErr *SandboxCredentialError
	if !errors.As(err, &credErr) {
		t.Fatalf("a login-only cursor-agent run on e2b = %v, want a SandboxCredentialError", err)
	}
	if err := guardOn(t, c, seat, sandbox.E2B, map[string]string{"CURSOR_API_KEY": "crsr-not-real-0123456789"}); err != nil {
		t.Errorf("a cursor-agent run carrying its key was refused a remote box: %v", err)
	}
}
