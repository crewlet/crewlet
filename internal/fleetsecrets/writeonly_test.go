package fleetsecrets_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
)

// fakeHeader is a stand-in header value, long enough to be withheld from any
// sentence and plainly not a credential anybody issued.
const fakeHeader = "Bearer lin_api_not-a-real-key"

func mustSetWriteOnly(t *testing.T, s *fleetsecrets.Store, name, value string) {
	t.Helper()
	if err := s.SetWriteOnly(t.Context(), name, value, sam, secrets.SourceMCP, clock); err != nil {
		t.Fatalf("SetWriteOnly(%s): %v", name, err)
	}
}

// A write-only value reaches the resolver and no reveal: Get and Values refuse it
// unopened. Mutation: let Get open it, and this is red.
func TestAWriteOnlyValueIsResolvedAndNeverRevealed(t *testing.T) {
	t.Parallel()
	s, _ := fleetStore(t, ring(t, "k1"))
	mustSetWriteOnly(t, s, "MCP_LINEAR_AUTHORIZATION", fakeHeader)
	mustSet(t, s, "GITLAB_TOKEN", "glpat-not-a-real-token")

	all, err := s.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if all["MCP_LINEAR_AUTHORIZATION"] != fakeHeader {
		t.Fatal("the resolver's snapshot does not hold the write-only value, so no ${VAR} reaches it")
	}

	value, err := s.Get(t.Context(), "MCP_LINEAR_AUTHORIZATION")
	if !errors.Is(err, secrets.ErrWriteOnly) || value != "" {
		t.Fatalf("Get = (%q, %v), want it refused as write-only with no value", value, err)
	}
	if strings.Contains(err.Error(), "lin_api") {
		t.Fatalf("the refusal carries the value: %v", err)
	}

	values, err := s.Values(t.Context(), []string{"GITLAB_TOKEN", "MCP_LINEAR_AUTHORIZATION"})
	var refused *fleetsecrets.WriteOnlyError
	if !errors.As(err, &refused) || !errors.Is(err, secrets.ErrWriteOnly) || values != nil {
		t.Fatalf("Values = (%v, %v), want the whole read refused as write-only", values, err)
	}
	if !slices.Equal(refused.Names, []string{"MCP_LINEAR_AUTHORIZATION"}) {
		t.Errorf("the refusal names %v, want the write-only name alone", refused.Names)
	}
	if got, err := s.Values(t.Context(), []string{"GITLAB_TOKEN"}); err != nil ||
		got["GITLAB_TOKEN"] == "" {
		t.Errorf("a read naming no write-only row = (%v, %v), want it answered", got, err)
	}

	row, found, err := s.Describe(t.Context(), "MCP_LINEAR_AUTHORIZATION")
	if err != nil || !found || !row.WriteOnly || row.Source != secrets.SourceMCP {
		t.Errorf("Describe = (%+v, %t, %v), want the row marked write-only from mcp", row, found, err)
	}
	listed, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range listed {
		if r.WriteOnly != (r.Name == "MCP_LINEAR_AUTHORIZATION") {
			t.Errorf("the listing marks %s write-only = %t", r.Name, r.WriteOnly)
		}
	}
}

// A later write keeps the write-only mark; only a removal clears it.
// Mutation: have put ignore the row it replaces, and this is red.
func TestAWriteOverAWriteOnlyRowKeepsTheMark(t *testing.T) {
	t.Parallel()
	s, _ := fleetStore(t, ring(t, "k1"))
	mustSetWriteOnly(t, s, "MCP_LINEAR_AUTHORIZATION", fakeHeader)
	mustSet(t, s, "MCP_LINEAR_AUTHORIZATION", "Bearer lin_api_rotated-not-real")
	if _, err := s.Get(t.Context(), "MCP_LINEAR_AUTHORIZATION"); !errors.Is(err, secrets.ErrWriteOnly) {
		t.Fatalf("a plain write over a write-only row made it readable: %v", err)
	}
	all, err := s.All(t.Context())
	if err != nil || all["MCP_LINEAR_AUTHORIZATION"] != "Bearer lin_api_rotated-not-real" {
		t.Fatalf("the rotation did not land: %v", err)
	}
	if _, err := s.Unset(t.Context(), "MCP_LINEAR_AUTHORIZATION"); err != nil {
		t.Fatalf("Unset: %v", err)
	}
	mustSet(t, s, "MCP_LINEAR_AUTHORIZATION", "Bearer lin_api_again-not-real")
	if got, err := s.Get(t.Context(), "MCP_LINEAR_AUTHORIZATION"); err != nil || got == "" {
		t.Errorf("a value stored after a removal = (%q, %v), want it readable: a removal clears the mark", got, err)
	}
}

// A rekey keeps a row write-only and resolved.
func TestARekeyKeepsTheWriteOnlyMark(t *testing.T) {
	t.Parallel()
	old, fleet := fleetStore(t, ring(t, "k1"))
	mustSetWriteOnly(t, old, "MCP_LINEAR_AUTHORIZATION", fakeHeader)
	rotated := fleetsecrets.New(fleet, ring(t, "k2", "k1"))
	moved, err := rotated.Rekey(t.Context(), "k2")
	if err != nil || !slices.Equal(moved.Moved, []string{"MCP_LINEAR_AUTHORIZATION"}) {
		t.Fatalf("Rekey = (%+v, %v)", moved, err)
	}
	if _, err := rotated.Get(t.Context(), "MCP_LINEAR_AUTHORIZATION"); !errors.Is(err, secrets.ErrWriteOnly) {
		t.Fatalf("the rekey made a write-only row readable: %v", err)
	}
	if all, err := rotated.All(t.Context()); err != nil || all["MCP_LINEAR_AUTHORIZATION"] != fakeHeader {
		t.Fatalf("the re-sealed row no longer resolves: %v", err)
	}
}
