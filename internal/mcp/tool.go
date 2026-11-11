package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"

	"github.com/crewlet/crewlet/internal/caveat"
)

// Result is what one tool call produced.
//
// There is ONE output field, not an output-plus-error pair, and that is
// deliberate: the text goes back to the model either way, and splitting it
// into two fields is how a real incident happened: the upstream error went to
// `error` while the loop rendered `output`, so the agent was shown a generic
// "tool execution failed" instead of the server's actual reason. It
// maps onto toolloop.ToolResult field for field.
type Result struct {
	// Output is fed back to the model as the tool message's content.
	Output string

	// Failed marks a tool that reported failure. The output still goes back —
	// that is the point — but a reader can tell a tool that ran from one that
	// refused.
	Failed bool

	// Cause is WHY a failed call failed, as a value a caller can branch on:
	// an authority refusal, a stale version, a node that could not decide.
	// Nil on a success, and nil on a failure nothing in this engine
	// classified — an MCP server's own, which reports prose and no class.
	// A call the engine refuses ABOUT a server — one that is not running,
	// or whose session ended under the call — is the engine's, and carries
	// [RefusalUnavailable] (supervise.go).
	//
	// IT IS ALSO WHERE A FIRST-PARTY REFUSAL'S CLASS RIDES. A [Refusal] is
	// an error, so a tool states its class as the cause itself — or beneath
	// the error that caused it, through [Classify] — and [RefusalOf] and
	// [UnknownOf] read it back out. ONE VALUE, never a class field beside
	// it: two classifiers of one failure drift apart, and a stored "not
	// unknown" beside a cause that says the write may have landed would
	// have a person's screen report that nothing happened.
	//
	// NEVER RENDERED, and that is the one rule this field carries. Output is
	// still the whole of what anybody reads — the incident this struct's doc
	// describes was a reason living in a second field while the first was
	// shown — so Cause adds nothing a model sees. It exists for the caller
	// that answers in a protocol with STATUS CODES, where "you may not",
	// "somebody edited it first" and "this node could not tell" are 403,
	// 409 and 503, and reading those back out of a sentence would make the
	// wording of a refusal an API.
	Cause error

	// Note is what the caller tells the MODEL beside Output: on a success,
	// the arguments the call sent outside its tool's schema, which the tool
	// did not read as sent (ADR-0059). NEVER PART OF OUTPUT, because a
	// caller may parse Output (a write tool's JSON receipt, which the work
	// API decodes); each renderer for a model places it where the model
	// reads it: the model's text, an MCP answer's own content. A person's
	// answer never carries it (ADR-0060); a surface answering a person tells
	// it in process to an assistant's model instead (internal/modelnote).
	Note string

	// Caveats are what a call that LANDED says beside its answer, each in the
	// words of every reader ([caveat.Caveat]). Nil on a failure and on a
	// success that has nothing to add.
	//
	// NEVER RENDERED, for Cause's reason: Output already carries each
	// caveat's MODEL phrasing under `warnings`, written from these same
	// values, so a model reads nothing here it has not read there. It exists
	// for the caller that answers a PERSON (the act route, ADR-0060), which
	// must answer the person's phrasing and never the model's, and could
	// only otherwise read the model's out of Output.
	Caveats []caveat.Caveat

	// PersonRefusal is a call that was REFUSED, in a person's words
	// ([caveat.Refusal]). Zero on a success, and on a refusal nobody has
	// worded for a person yet.
	//
	// NEVER RENDERED, for Cause's reason: Output is the model's sentence
	// about the same refusal, and stays the whole of what a model reads. It
	// exists for the caller that answers a PERSON (the act route,
	// ADR-0060), which answers this phrasing as the refusal's `detail` and
	// never Output; where it is zero, that caller answers the refusal
	// code's own sentence and no detail, so a refusal nobody worded for a
	// person tells them less, and never a model's words.
	PersonRefusal caveat.Refusal

	// PersonReceipt is a call that LANDED, its answer as a surface
	// answering a person answers it, where the tool wrote one apart from
	// Output: an answer that echoes words other principals wrote — a
	// room's topic, a parked run's question, a page's title — labels them
	// in Output for a model, fenced or defused under an envelope that says
	// how far to believe them, and here they are as they were stored. Nil
	// where Output is that answer already, and on a failure, which a
	// person's surface answers from PersonRefusal.
	//
	// NEVER RENDERED, for Cause's reason: a model reads Output, labelled,
	// and nothing here. It exists for the caller that answers a PERSON (the
	// act route, ADR-0060), to whom the labels are a model's framing (a
	// fence is a model's concern, ADR-0040): read off Output, a room's
	// topic reached a person's answer wrapped in a fence's two lines.
	PersonReceipt json.RawMessage
}

// Callable is the least a phase surface needs to offer something to a model
// and run what it asked for. Both a bridged MCP tool and a discovery meta-tool
// satisfy it.
type Callable interface {
	Name() string
	Description() string
	// Parameters is a JSON Schema object.
	Parameters() map[string]any
	// Call runs the tool. An ERROR means the caller's own context ended —
	// the turn is being torn down — and nothing should be reported to the
	// model. A tool that failed is an ordinary Result with Failed set.
	Call(ctx context.Context, args map[string]any) (Result, error)
}

// Tool is one MCP server tool, wrapped for the engine.
//
// It reaches its server through the SERVER, never through one session: a
// server the bridge restarts gets a new session, and a Tool held to the old
// one would dispatch into a process that is gone for the rest of the
// catalogue's life. Held to the server, the same Tool serves through each
// session in turn and refuses honestly between them — so a restart that lists
// the same tools changes no registry anywhere.
type Tool struct {
	Definition
	srv *server
}

// Definition is one tool as its server lists it, under the name and with the
// hints its entry gives it: what a reader of the server's listing is shown.
//
// IT CANNOT BE CALLED. A [Tool] is a Definition its server is called through;
// a tool the entry blocks ([Levels]) is kept as a Definition alone
// ([Bridge.BlockedTools]), so the type a blocked tool has is one no catalogue
// takes and no caller can send a call through.
type Definition struct {
	name        string // the catalogue name: prefix applied
	raw         string // the name the server knows it by
	title       string // the tool's own title, "" where it gives none
	description string
	parameters  map[string]any
	output      map[string]any // the output schema, nil where it gives none
	annotations Annotations
	declared    Annotations // the hints as the server declared them, no override applied
	digest      string      // the server's definition, hashed ([digestOf])
	parts       Parts       // each part of it, hashed apart ([partsOf])
	level       Level       // what its entry, or else its hints, set it at ([Levels.Of])
}

var _ Callable = (*Tool)(nil)

// toolsFor builds the server's contribution from a fresh listing, keeping the
// Tool it already had for every entry the listing still describes exactly.
//
// KEPT rather than rebuilt, because a Tool's identity is what every registry
// holding it compares: a restart that rebuilt them all would report every
// name as changed, and each held seat's surface would be refiled for a server
// that came back exactly as it went down. A tool whose description, schema
// or hints moved IS rebuilt, so a registry is told to take the new one.
//
// A tool the entry BLOCKS ([Spec.Levels]) is never built as a Tool: it is
// returned as a [Definition] beside the rest, in the listing's order, and
// left out of the server's contribution, so no catalogue holds it, no model
// is offered or lists it, and no call through a catalogue reaches it. It is
// judged first, so a blocked tool whose name no model could be handed either
// is blocked rather than refused: the operator's decision is the one that
// holds it back.
//
// A tool whose name, prefix included, no model can be handed is REFUSED here
// and returned beside the rest, never built ([nameRefusal]): left out of the
// server's contribution, it is not offered, not callable, and shadows no
// tool of another server that could be.
//
// The caller holds the bridge's lock.
func (s *server) toolsFor(defs []toolDef) (filed []*Tool, blocked []*Definition, refused []RefusedTool) {
	kept := make(map[string]*Tool, len(s.tools))
	for _, t := range s.tools {
		kept[t.raw] = t
	}
	out := make([]*Tool, 0, len(defs))
	for _, def := range defs {
		// Levels match the SERVER's name, before the prefix: an operator
		// setting them has the server's listing in front of them.
		if s.spec.Levels.Blocks(def.Name) {
			d := definitionOf(def, s.spec)
			blocked = append(blocked, &d)
			continue
		}
		name := s.spec.ToolPrefix + def.Name
		if reason := nameRefusal(name); reason != "" {
			refused = append(refused, RefusedTool{Name: name, Reason: reason})
			s.b.log.Warn("mcp_tool_refused", "server", s.spec.Name, "tool", name,
				"reason", string(reason), "detail", reason.Sentence(name, s.spec.ToolPrefix))
			continue
		}
		fresh := newTool(s, def, s.spec)
		if old, ok := kept[def.Name]; ok && old.sameAs(fresh) {
			out = append(out, old)
			continue
		}
		out = append(out, fresh)
	}
	sortRefused(refused)
	return out, blocked, refused
}

// sameAs reports whether two tools would put the same thing in front of a
// model (name, description, schema and hints), describe the same definition
// (its digest, which also covers the title and the output schema a listing
// shows) and run at the same level. A tool kept across a restart whose title
// moved would leave the fleet's listing naming the old one, and one kept at
// its old level would run at a level nobody set.
func (t *Tool) sameAs(other *Tool) bool {
	return t.name == other.name && t.raw == other.raw &&
		t.description == other.description &&
		t.annotations == other.annotations &&
		t.digest == other.digest && t.level == other.level &&
		reflect.DeepEqual(t.parameters, other.parameters)
}

// definitionOf is def under the name and with the hints spec gives it.
func definitionOf(def toolDef, spec Spec) Definition {
	name := spec.ToolPrefix + def.Name

	// An override may be keyed by EITHER name. The raw one is what the server
	// reports; the prefixed one is what the operator sees in the catalogue
	// and the dashboard, and keying by that must not silently do nothing.
	// Raw first, so the more specific spelling wins if both are present.
	ann := def.Annotations
	if override, ok := spec.AnnotationOverrides[def.Name]; ok {
		ann = ann.Merge(override)
	} else if override, ok := spec.AnnotationOverrides[name]; ok {
		ann = ann.Merge(override)
	}
	return Definition{
		name:        name,
		raw:         def.Name,
		title:       def.Title,
		description: def.Description,
		parameters:  maps.Clone(def.InputSchema),
		output:      maps.Clone(def.OutputSchema),
		annotations: ann,
		declared:    def.Annotations,
		digest:      digestOf(def),
		parts:       partsOf(def),
		// THE HINTS THE OPERATOR'S OVERRIDES LEAVE, so the level a tool
		// starts at is read from the same hints its risk is shown by.
		level: spec.Levels.Of(def.Name, ann),
	}
}

func newTool(s *server, def toolDef, spec Spec) *Tool {
	if len(def.InputSchema) > 0 {
		if kind, ok := def.InputSchema["type"]; !ok || kind != "object" {
			// The spec requires a tool's inputSchema to be an object
			// schema. Repaired at the render boundary rather than here
			// (see tools.paramsOrEmpty — one rule, one place), but said
			// out loud once here, where the wire value arrives: a model
			// offered a repaired schema for a tool whose server describes
			// its arguments wrongly will call it wrongly, and nothing
			// else would ever name the server responsible.
			s.b.log.Warn("mcp_tool_schema_not_an_object",
				"server", spec.Name, "tool", def.Name, "type", kind,
				"hint", "the MCP spec requires inputSchema.type == \"object\"; "+
					"the engine offers a repaired schema, so the model is not "+
					"told what this tool's arguments are")
		}
	}
	return &Tool{Definition: definitionOf(def, spec), srv: s}
}

// Name is the catalogue name, with the server's tool_prefix applied.
func (d *Definition) Name() string { return d.name }

// RawName is the name the server itself uses, which is what goes on the wire.
func (d *Definition) RawName() string { return d.raw }

// Description is what the model is told the tool does.
func (d *Definition) Description() string { return d.description }

// Parameters is the tool's JSON Schema, as the server published it.
func (d *Definition) Parameters() map[string]any { return d.parameters }

// Annotations are the server's behavioural hints with operator overrides
// applied. Tri-state: see annotations.go.
func (d *Definition) Annotations() Annotations { return d.annotations }

// Title is the tool's own display name, else the one its annotations carry,
// else "". Never shown to a model: a person reads it in the listing.
func (d *Definition) Title() string {
	if d.title != "" {
		return d.title
	}
	return d.annotations.Title
}

// OwnTitle is the tool's own title alone, "" where the server gives none: with
// [Definition.Declared]'s title, the words the digest's title part covers.
func (d *Definition) OwnTitle() string { return d.title }

// OutputSchema is the JSON Schema of the tool's structured result, as the
// server published it, or nil where it published none.
func (d *Definition) OutputSchema() map[string]any { return d.output }

// Digest names the server's definition of the tool (see digest.go): two tools
// with one digest are one definition, wherever and by whichever instance they
// were listed.
func (d *Definition) Digest() string { return d.digest }

// Declared are the hints as the server declared them, before any override the
// operator laid over them: the server's own claim about the tool, which is
// what its definition's digest covers and what a change to it is judged by.
func (d *Definition) Declared() Annotations { return d.declared }

// Parts names each part of the server's definition apart ([Parts]): which of
// them differ between two definitions of the tool is what the server changed.
func (d *Definition) Parts() Parts { return d.parts }

// Level is the level the tool runs at: its own in the entry's levels, else the
// entry's default, else the level its hints start it at ([Levels.Of]). A
// filed [Tool] is at [LevelAllow] or [LevelApprove]; only a blocked
// Definition reads [LevelBlocked].
func (d *Definition) Level() Level { return d.level }

// Unreviewed reports whether the definition the server lists for this tool is
// not the one its level was decided on (ADR-0078): one it changed, or added
// after its first listing, that nobody has accepted since. Such a tool asks a
// person before each call until somebody does, whatever level it is at. The
// bridge's owner decides it ([Supervision.Unreviewed]); false where it decides
// nothing.
//
// ASKED AT EACH CALL, never fixed when the tool is filed: the decision moves
// when a person accepts the change, which files nothing, and a tool filed
// once must not keep the answer it was filed with.
func (t *Tool) Unreviewed() bool {
	unreviewed := t.srv.b.sup.Unreviewed
	return unreviewed != nil && unreviewed(t.Server(), t.raw, t.digest)
}

// Instance is the full instance name of the server serving this tool —
// "github::Engineer" for a per-role child. It identifies the PROCESS.
func (t *Tool) Instance() string { return t.srv.spec.Name }

// Server is the bare server name, which is what the model is shown and what it
// types into list_mcp_server_tools. Two seats' children of one template are
// the same server to a reader.
func (t *Tool) Server() string { return ServerName(t.srv.spec.Name) }

// Origin is this tool's entry in the tool-origin grammar, ready for whatever
// registry the caller keeps. See origin.go.
func (t *Tool) Origin() string { return Origin(t.Server()) }

// Call runs the tool on its server.
//
// A failing tool is ORDINARY and comes back as a Result with Failed set: the
// server's own words go to the model, which is expected to react to them. The
// error return is reserved for the caller's context ending, because a turn
// that is being torn down has nobody left to show a tool message to — and
// because reporting a cancellation as a tool failure would teach the model
// that the tool is broken.
//
// A server that is NOT RUNNING — down and being restarted, or stopped by its
// owner — is refused by the engine in its own words rather than by whatever
// error a dead pipe produces, and so is a call whose session ended under it.
// See server.refusal.
func (t *Tool) Call(ctx context.Context, args map[string]any) (Result, error) {
	c := t.srv.live()
	if c == nil || c.gone() {
		return t.srv.refusal(t, c, false), nil
	}
	blocks, err := c.callTool(ctx, t.raw, args)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, err
		}
		// The session's end is recorded before the error that ends the call
		// reaches it (client.end), so this is an exact question rather than a
		// guess from the error's text.
		if c.gone() {
			return t.srv.refusal(t, c, true), nil
		}
		return Result{
			Output: fmt.Sprintf("MCP tool error (%s/%s): %v", t.srv.spec.Name, t.raw, err),
			Failed: true,
		}, nil
	}
	t.srv.noteAnswer()
	out := renderBlocks(blocks)
	if out == "" {
		// A server that answers with nothing at all is not an error, but a
		// blank tool message reads to a model as a dropped turn.
		out = "Tool returned no output."
	}
	return Result{Output: out}, nil
}

// Entry is one tool as the discovery meta-tools see it: enough to bucket it by
// server and describe it, and nothing that would let them call it.
type Entry struct {
	Name        string
	Description string
	// Server is the bare server name, or "" for a tool that is not an MCP
	// tool at all — a builtin in the merged catalogue.
	Server string
}

// EntriesOf projects bridged tools into catalogue entries.
func EntriesOf(tools []*Tool) []Entry {
	out := make([]Entry, len(tools))
	for i, t := range tools {
		out[i] = Entry{Name: t.Name(), Description: t.Description(), Server: t.Server()}
	}
	return out
}
