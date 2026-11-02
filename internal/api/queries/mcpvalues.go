package queries

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// MCPValueState is one header or variable a server is sent, by name, with what
// the store holds for it: never a value. The admin's reading only.
type MCPValueState struct {
	// As is `header` for a remote server, `env` for a local one.
	Key string            `json:"key"`
	As  config.MCPValueAs `json:"as"`
	// Variable is the one variable the value reads, "" for a literal,
	// several, or a per-agent binding at the server's level.
	Variable string `json:"variable"`
	// Set is whether the store holds every variable it reads (a literal
	// is set; a per-agent binding is not, at the server's level).
	Set bool `json:"set"`
	// StoredAt and StoredBy are the variable's row's, null for none.
	StoredAt *time.Time `json:"stored_at"`
	StoredBy *string    `json:"stored_by"`
	// Optional is whether `optional_keys` names it.
	Optional bool `json:"optional"`
	// BoundBy is the integration binding it per agent, or null.
	BoundBy *string `json:"bound_by"`
}

// MCPValuesOf is keys against the store's rows (never opened). Always a list.
func MCPValuesOf(keys []config.MCPValueKey, rows []secrets.Record) []MCPValueState {
	held := make(map[string]secrets.Record, len(rows))
	for _, row := range rows {
		held[row.Name] = row
	}
	out := make([]MCPValueState, 0, len(keys))
	for _, key := range keys {
		state := MCPValueState{Key: key.Key, As: key.As, Variable: key.Variable, Optional: key.Optional}
		if key.BoundBy != "" {
			bound := key.BoundBy
			state.BoundBy = &bound
		}
		boundHere := key.BoundBy != "" && len(key.Reads) == 0
		state.Set = !boundHere &&
			!slices.ContainsFunc(key.Reads, func(name string) bool { _, ok := held[name]; return !ok })
		if row, ok := held[key.Variable]; ok && key.Variable != "" {
			at, by := row.UpdatedAt.UTC(), row.UpdatedBy
			state.StoredAt, state.StoredBy = &at, &by
		}
		out = append(out, state)
	}
	return out
}

// mcpValues is name's values in company against the fleet's secret rows.
func (s Sources) mcpValues(ctx context.Context, company *config.Company, name string) ([]MCPValueState, error) {
	keys, ok := company.MCPValueKeys(name, "")
	if !ok {
		return []MCPValueState{}, nil
	}
	var rows []secrets.Record
	if s.Secrets != nil {
		var err error
		if rows, err = s.Secrets.List(ctx); err != nil {
			return nil, err
		}
	}
	return MCPValuesOf(keys, rows), nil
}
