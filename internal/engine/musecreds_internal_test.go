package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A MUSE CODE EXECUTOR SIGNS IN AS ITS PROFILE SAYS IN EVERY CELL: its
// `muse login` (`.config/muse/auth.json`) reaches a local box alone, so an
// entry on that login alone is refused on e2b, naming the local cell, while its
// metered META_API_KEY, which travels, signs a remote run in; and a local
// cell, seeded with the login, needs no key.
//
// Not parallel: META_API_KEY is read from the process environment by
// inherit-env, emptied here so a key exported on the machine cannot sign the
// entry in.
//
// Mutation: drop api_key_env or credential_paths from the muse-code profile,
// and the key no longer signs a remote run in, or the local cell is refused.
func TestAMuseCodeLoginStaysInALocalCellAndItsKeyTravels(t *testing.T) {
	t.Setenv("META_API_KEY", "")
	login, loginSeat := creditedCompanyAuth(t, "muse-code", harness.Auth{Mode: harness.AuthSubscription})
	err := guardOn(t, login, loginSeat, sandbox.E2B, nil)
	var credErr *SandboxCredentialError
	if !errors.As(err, &credErr) || !strings.Contains(err.Error(), string(sandbox.Direct)) {
		t.Fatalf("a login-only muse-code entry on e2b answered %v, want a refusal naming the local cell", err)
	}
	if err := guardOn(t, login, loginSeat, sandbox.Container, nil); err != nil {
		t.Errorf("a login-only muse-code entry in a local cell was refused: %v", err)
	}
	key, keySeat := creditedCompanyAuth(t, "muse-code", harness.Auth{Mode: harness.AuthAPIKey})
	env := runOn(t, key, keySeat, map[string]string{"META_API_KEY": "sk-placeholder-not-a-key"}).env
	if err := guardOn(t, key, keySeat, sandbox.E2B, env); err != nil {
		t.Errorf("a muse-code entry whose seat brings META_API_KEY was refused on e2b: %v", err)
	}
}
