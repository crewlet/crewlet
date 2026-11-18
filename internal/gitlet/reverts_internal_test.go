package gitlet

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ONLY AN ENGINE-BUILT REVERT NAMES WHAT IT REVERTS: a proposal's change
// carrying `reverts` is refused naming change.reverts unless the engine built
// the push, and a negative number is refused from anybody.
//
// Mutation: drop the proposal's rule on change.reverts (the git push's case
// validates, red).
func TestOnlyAnEngineBuiltRevertNamesWhatItReverts(t *testing.T) {
	t.Parallel()
	of := func(via PushVia, reverts int64) Proposal {
		p := proposalOf(cmd("refs/heads/revert-3", ZeroOID, oid("R")))
		p.RepoID, p.DropEpoch, p.Via = uuid.Must(uuid.NewV7()).String(), 0, via
		p.Change = &ChangeOption{Target: "refs/heads/main", Title: "Revert web!3", Reverts: reverts}
		return p
	}
	if err := of(ViaEngine, 3).Validate(); err != nil && strings.Contains(err.Error(), "change.reverts") {
		t.Fatalf("an engine-built revert naming change 3: %v", err)
	}
	if err := of(ViaGit, 0).Validate(); err != nil {
		t.Fatalf("a push opening a change that reverts nothing: %v", err)
	}
	for name, p := range map[string]Proposal{
		"a push git received": of(ViaGit, 3),
		"a negative number":   of(ViaEngine, -1),
	} {
		if err := p.Validate(); !errors.Is(err, ErrInvalidProposal) || !strings.Contains(err.Error(), "change.reverts") {
			t.Errorf("%s: %v, want refused naming change.reverts", name, err)
		}
	}
}
