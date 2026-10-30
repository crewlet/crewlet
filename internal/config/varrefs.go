package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/crewlet/crewlet/internal/envref"
)

// VariableReferences is every `${VAR}` the document names, counted by who
// names it.
//
// # Why counted, and by whom
//
// A seat's credential is that seat's identity at a vendor, and the incident
// that made per-seat access a stored setting was one seat's provisioning sink
// pointing at the variable a company block also read: the pass sealed the
// seat's minted key over the company's organization key. Two readers of one
// variable is the whole defect, so the question every rule here asks is "who
// else names this variable", and it is answered for the whole document at
// once rather than by each rule walking it again.
//
// WALKED THROUGH THE WIRE SHAPE, not field by field: a reference can sit in any
// string of any block, and a field added to a block next year is covered
// without anybody remembering this function. Only names are collected, never
// values.
type VariableReferences struct {
	// Total counts every reference in the document.
	Total map[string]int

	// Integrations counts the references inside the company's
	// `integrations:` blocks.
	Integrations map[string]int

	// SeatCredentials counts, by seat handle, the references in the seat's
	// own credential positions: its `mcp_env` and its `integrations` blocks.
	SeatCredentials map[string]map[string]int

	// Seats counts, by seat handle, every reference in the seat's role entry.
	Seats map[string]map[string]int
}

// References counts the document's variable references. See
// [VariableReferences]. An error is a document that does not marshal, which a
// stored or validated one always does; a caller reports it rather than read an
// empty count as "nobody else names this".
func (c *Company) References() (VariableReferences, error) {
	total, err := countRefs(c)
	if err != nil {
		return VariableReferences{}, err
	}
	company, err := countRefs(c.Integrations)
	if err != nil {
		return VariableReferences{}, err
	}
	refs := VariableReferences{
		Total: total, Integrations: company,
		SeatCredentials: map[string]map[string]int{},
		Seats:           map[string]map[string]int{},
	}
	for role := range c.EachRole() {
		// BY THE DERIVED HANDLE, the org model's key, which every reader
		// of these counts holds a seat by.
		handle := role.IdentityKey()
		if handle == "" {
			continue
		}
		env, envErr := countRefs(role.MCPEnv)
		blocks, blocksErr := countRefs(role.Integrations)
		whole, wholeErr := countRefs(role)
		if joined := errors.Join(envErr, blocksErr, wholeErr); joined != nil {
			return VariableReferences{}, joined
		}
		refs.SeatCredentials[handle] = sumRefs(refs.SeatCredentials[handle], sumRefs(env, blocks))
		refs.Seats[handle] = sumRefs(refs.Seats[handle], whole)
	}
	return refs, nil
}

// OutsideSeat counts the references to name made anywhere but handle's own
// role entry: by the company, by a unit, or by another seat.
func (r VariableReferences) OutsideSeat(name, handle string) int {
	return r.Total[name] - r.Seats[handle][name]
}

// countRefs counts the references in every string v marshals to.
func countRefs(v any) (map[string]int, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("config: read the document's variable references: %w", err)
	}
	var tree any
	if err = json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("config: read the document's variable references: %w", err)
	}
	out := map[string]int{}
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case string:
			for _, name := range envref.Names(n) {
				out[name]++
			}
		case []any:
			for _, item := range n {
				walk(item)
			}
		case map[string]any:
			for _, item := range n {
				walk(item)
			}
		}
	}
	walk(tree)
	return out, nil
}

// sumRefs adds b's counts to a's, returning a new map.
func sumRefs(a, b map[string]int) map[string]int {
	out := make(map[string]int, len(a)+len(b))
	maps.Copy(out, a)
	for name, n := range b {
		out[name] += n
	}
	return out
}
