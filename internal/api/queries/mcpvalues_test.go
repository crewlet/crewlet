package queries_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A value's state is what the store holds, by name: literals set, bound keys
// not, a key reading an unheld variable not.
func TestAValuesStateIsWhatTheStoreHoldsByName(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	rows := []secrets.Record{
		{Name: "MCP_LINEAR_AUTHORIZATION", UpdatedAt: at, UpdatedBy: "jane.doe", WriteOnly: true},
		{Name: "ORG", UpdatedAt: at, UpdatedBy: "sam"},
	}
	keys := []config.MCPValueKey{
		{Key: "Authorization", As: config.MCPValueHeader, Variable: "MCP_LINEAR_AUTHORIZATION",
			Reads: []string{"MCP_LINEAR_AUTHORIZATION"}},
		{Key: "X-Team", As: config.MCPValueHeader, Variable: "TEAM", Reads: []string{"TEAM"}, Optional: true},
		{Key: "X-Region", As: config.MCPValueHeader},
		{Key: "X-Pair", As: config.MCPValueHeader, Reads: []string{"ORG", "TEAM"}},
		{Key: "Private-Token", As: config.MCPValueHeader, BoundBy: "gitlab"},
	}
	got := queries.MCPValuesOf(keys, rows)
	if len(got) != len(keys) {
		t.Fatalf("got %d states for %d keys", len(got), len(keys))
	}
	stored := got[0]
	if !stored.Set || stored.StoredAt == nil || !stored.StoredAt.Equal(at) || stored.StoredBy == nil ||
		*stored.StoredBy != "jane.doe" || stored.BoundBy != nil {
		t.Errorf("a stored value = %+v", stored)
	}
	if got[1].Set || got[1].StoredAt != nil || !got[1].Optional {
		t.Errorf("a value the store lacks = %+v", got[1])
	}
	if !got[2].Set || got[2].Variable != "" || got[2].StoredAt != nil {
		t.Errorf("a literal = %+v, want it set with no variable", got[2])
	}
	if got[3].Set {
		t.Errorf("a key reading a held and an unheld variable = %+v, want it unset", got[3])
	}
	if bound := got[4]; bound.Set || bound.BoundBy == nil || *bound.BoundBy != "gitlab" {
		t.Errorf("a bound key = %+v, want it the integration's and unset at the server's level", bound)
	}
	if empty := queries.MCPValuesOf(nil, rows); empty == nil || len(empty) != 0 {
		t.Errorf("no keys = %#v, want an empty list", empty)
	}
}
