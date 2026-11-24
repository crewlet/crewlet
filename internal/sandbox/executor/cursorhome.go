package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

var _ homeMender = CursorAgent{}

// mendHome takes a killed run's servers out of the home's ~/.cursor/mcp.json
// before the next run in the box, whichever harness it runs: where the record
// of the servers a run laid in ([Paths.CursorMCPAdded]) is still there, the
// file as it stands less exactly those names ([cursorMCPLeftOut]) is put back
// and the record removed; nothing where none is there. A run that ends takes
// its own out (cursor_helper.mjs's mcp-remove).
func (CursorAgent) mendHome(ctx context.Context, box sandbox.Sandbox, p Paths) error {
	held, err := heldInBox(ctx, box, p.CursorMCPAdded())
	if err != nil || !held {
		return err
	}
	record, err := box.ReadFile(ctx, p.CursorMCPAdded())
	if err != nil {
		return fmt.Errorf("executor: reading the record of a cut-off Cursor Agent run's MCP servers: %w", err)
	}
	var live []byte
	there, err := heldInBox(ctx, box, p.CursorMCP())
	if err != nil {
		return err
	}
	if there {
		if live, err = box.ReadFile(ctx, p.CursorMCP()); err != nil {
			return fmt.Errorf("executor: reading the box home's Cursor MCP servers: %w", err)
		}
	}
	left, gone, err := cursorMCPLeftOut(live, record)
	if err != nil {
		return err
	}
	q := shellword.Quote
	if gone {
		return execOK(ctx, box, "rm -f "+q(p.CursorMCP())+" "+q(p.CursorMCPAdded()),
			"taking a cut-off Cursor Agent run's MCP servers out of the box home")
	}
	if left != nil {
		if err = box.WriteFile(ctx, p.CursorMCP(), left); err != nil {
			return fmt.Errorf("executor: putting back the box home's Cursor MCP servers: %w", err)
		}
	}
	return execOK(ctx, box, "rm -f "+q(p.CursorMCPAdded()),
		"removing the record of a cut-off Cursor Agent run's MCP servers")
}

// cursorMCPRecord is what mcp-add records before it lays a run's servers in:
// their names, and whether it created the home's file.
type cursorMCPRecord struct {
	Names   *[]string `json:"names"`
	Created bool      `json:"created"`
}

// cursorMCPLeftOut is the home's mcp.json live, as a run cut off before it took
// its servers out left it, less each name record holds, every other key as live
// holds it, an edit made since included: gone where the run created the file
// and nothing else is left in it; nil, not gone, where there is nothing to
// change (no file, or one whose mcpServers is no object).
func cursorMCPLeftOut(live, record []byte) (left []byte, gone bool, err error) {
	var rec cursorMCPRecord
	if err = json.Unmarshal(record, &rec); err != nil || rec.Names == nil {
		if err == nil {
			err = fmt.Errorf("it names no servers")
		}
		return nil, false, fmt.Errorf("executor: the record of a cut-off Cursor Agent run's MCP servers "+
			"(cursor-mcp-added.json in the box's work directory) does not read (%v), so they cannot be "+
			"taken out of ~/.cursor/mcp.json; remove that record and the run's crewlet-<suffix> servers "+
			"from the file", err)
	}
	if len(bytes.TrimSpace(live)) == 0 {
		return nil, false, nil
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(live, &doc); err != nil || doc == nil {
		return nil, false, fmt.Errorf("executor: the box home's Cursor MCP servers (~/.cursor/mcp.json) are "+
			"no JSON object, so a cut-off run's servers cannot be taken out of them (%v); fix the file in "+
			"the box", err)
	}
	// SERVERS ARE KEYED BY NAME, so mcpServers that is absent or no object
	// (null among them) holds none of the run's: nothing to take out.
	raw := bytes.TrimSpace(doc["mcpServers"])
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false, nil
	}
	var servers map[string]json.RawMessage
	if err = json.Unmarshal(raw, &servers); err != nil {
		return nil, false, fmt.Errorf("executor: the box home's Cursor MCP servers (~/.cursor/mcp.json) "+
			"hold mcpServers that do not read (%w); fix the file in the box", err)
	}
	for _, name := range *rec.Names {
		delete(servers, name)
	}
	if rec.Created && len(servers) == 0 && len(doc) == 1 {
		return nil, true, nil
	}
	if doc["mcpServers"], err = json.Marshal(servers); err != nil {
		return nil, false, fmt.Errorf("executor: rendering the box home's Cursor MCP servers: %w", err)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("executor: rendering the box home's Cursor MCP servers: %w", err)
	}
	return append(out, '\n'), false, nil
}
