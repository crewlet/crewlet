package coord

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

// WHAT EACH NODE'S MCP SERVERS LIST, read by the whole fleet (ADR-0074).
//
// A node knows what the servers it runs list, and nothing else does: a shared
// server's tools are filed into every node's own registry, but a per-agent
// server's children run only on the node holding each seat, so no node's
// registry holds the company's whole catalogue. Every node therefore
// publishes, per server, the union of what its own instances list, and every
// reader takes the union of the live nodes' records.
//
// # Who has to agree on it
//
// The whole company, now: a screen on any node shows a server whose children
// run on another, and the tool catalogue a node pushes names tools no
// instance of its own serves. It is too large for the presence heartbeat,
// which is re-sent every beat (a listing is tens of kilobytes where a status
// row is a few hundred bytes), so it is a record of its own, written when it
// changes.
//
// # Two classes in the positions register
//
// A listing is one record per (server, node), [MCPListingKey]; each tool's
// schemas are one record per definition digest, [MCPSchemaKey], written
// create-only, so a schema every seat's child of a server lists is stored
// once however many nodes and instances list it. Both live in the positions
// register, the bucket with no age, for the reason its other classes do: an
// age would forget the listing of a server that has not changed in a week,
// which is a server whose listing is exactly right. Removal is a decision:
// the node removes its own listing when it stops running a server or shuts
// down, and the maintenance sweep removes what a node that died left behind.
//
// # Swept with the node's presence, and held to its incarnation
//
// A listing carries the presence epoch of the node that wrote it. A reader
// counts it only while that node holds its presence AT THAT EPOCH
// ([LiveMCPListings]): a node that died stops counting the moment its
// presence lapses, and a node that restarted, or lapsed and re-claimed its
// presence, is a new incarnation whose listings are not counted until it
// writes them again, which it does at once (a new epoch is its signal that a
// sweep may have removed them). The sweep removes exactly what readers
// already ignore.
//
// # Bounded, and somebody else's words
//
// Every field of a listing but its counts came from a remote MCP server, so
// every one is bounded where the record is built ([MCPListing.Bounded]) and
// again by the store that writes it, and a reader treats them as the server's
// words. A record's key names its server and node, and a store stamps both
// from the key it files the record under, so a node can describe only its
// own listing of the server the key names.

// MCPListingClass and MCPSchemaClass are the key classes the records are
// filed under in the positions register.
const (
	MCPListingClass = "mcp_listing"
	MCPSchemaClass  = "mcp_schema"
)

// MCPListingKey is one node's listing of one server, by the server's
// configured name and the node's id.
func MCPListingKey(server, node string) string {
	return DocumentKey(MCPListingClass, server, node)
}

// MCPListingOf recovers the server and node from a listing key, false for a
// key of another class or one this grammar did not write.
func MCPListingOf(key string) (server, node string, ok bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 3 || segments[0] != MCPListingClass ||
		segments[1] == "" || segments[2] == "" {
		return "", "", false
	}
	return segments[1], segments[2], true
}

// MCPSchemaKey is one definition's schemas, by its digest.
func MCPSchemaKey(digest string) string { return DocumentKey(MCPSchemaClass, digest) }

// MCPSchemaDigest recovers the digest from a schema key, false for a key of
// another class or one naming no digest.
func MCPSchemaDigest(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != MCPSchemaClass || !IsMCPDigest(segments[1]) {
		return "", false
	}
	return segments[1], true
}

// IsMCPDigest reports whether s is a definition digest as internal/mcp writes
// one: 64 lowercase hexadecimal digits.
func IsMCPDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// The bounds a listing is held to. Together they keep the largest listing a
// node can write under half a mebibyte (256 tools of at most about 1.7 KiB
// each, plus the instructions and the server's identity), half the 1 MiB
// payload an external NATS server allows by default, so a server listing
// thousands of tools with essays for descriptions costs the fleet one bounded
// record rather than a write the broker refuses.
const (
	// MaxMCPListedTools is the most tools one listing names, the first by
	// name; [MCPListing.ToolCount] travels whole beside them. 256 is past
	// every catalogue entry's server and well past the 100 tools an agent
	// is budgeted.
	MaxMCPListedTools = 256

	// MaxMCPToolTitleBytes bounds a tool's title, and MaxMCPToolDescriptionBytes
	// its description: a description is what a model reads, and the whole
	// of one is in the server's own listing; the record keeps what a person
	// reading a list needs, and the digest still covers every byte.
	MaxMCPToolTitleBytes       = 256
	MaxMCPToolDescriptionBytes = 1024

	// MaxMCPInstructionsBytes bounds the instructions a server offers in
	// its handshake.
	MaxMCPInstructionsBytes = 4096

	// MaxMCPServerInfoBytes bounds each field of the server's identity.
	MaxMCPServerInfoBytes = 256

	// MaxMCPToolSchemaBytes bounds one definition's schemas, input and
	// output together, as encoded. A definition whose schemas are larger is
	// listed without them ([MCPListedTool.SchemaKept] false) rather than
	// stored: 64 KiB is past any schema a model is usefully offered.
	MaxMCPToolSchemaBytes = 64 << 10
)

// The three words a hint is written in on a listing, as internal/mcp's Hint
// renders them.
const (
	HintYes     = "yes"
	HintNo      = "no"
	HintUnknown = "unknown"
)

// MCPListing is one node's listing of one server: the union of what every
// instance of it on that node lists.
type MCPListing struct {
	// Server is the server's configured name and Node the node's id: the
	// record's key, stamped from it by the store.
	Server string `json:"server"`
	Node   string `json:"node"`

	// Epoch is the node's presence epoch when it wrote the listing: a
	// reader counts it only while the node holds its presence at that epoch
	// ([LiveMCPListings]).
	Epoch int64 `json:"epoch"`

	// At is when the node last changed the listing.
	At time.Time `json:"at"`

	// Shared is one instance for the company rather than one per seat.
	Shared bool `json:"shared"`

	// Instances is how many instances on the node this listing is the
	// union of: every one that has listed, serving or down since.
	Instances int `json:"instances"`

	// Info is what the server said of itself in its handshake, nil where
	// no instance on the node gave a name. Instructions is what it offers
	// a client for using it. The first instance's by seat, a shared one
	// first.
	Info         *MCPServerInfo `json:"info,omitempty"`
	Instructions string         `json:"instructions,omitempty"`

	// Tools are the tools the node's instances offer, sorted by name, at
	// most [MaxMCPListedTools]; ToolCount is how many there are in all,
	// never fewer than are listed.
	Tools     []MCPListedTool `json:"tools"`
	ToolCount int             `json:"tool_count"`

	// Version is the store's version of the record as it was read. Not on
	// the wire.
	Version uint64 `json:"-"`
}

// MCPServerInfo is a server's identity as its handshake gave it.
type MCPServerInfo struct {
	Name     string `json:"name"`
	Title    string `json:"title,omitempty"`
	Version  string `json:"version,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

// MCPListedTool is one tool a node's instances of a server offer.
type MCPListedTool struct {
	// Name is the name it is offered under, the server's tool_prefix
	// applied; Raw is the server's own name for it.
	Name string `json:"name"`
	Raw  string `json:"raw"`

	Title       string `json:"title,omitempty"`
	Description string `json:"description"`

	// Hints are its behavioural hints with the company's overrides applied,
	// each [HintYes], [HintNo] or [HintUnknown].
	Hints MCPToolHints `json:"hints"`

	// Digest names the server's definition (internal/mcp's digest.go),
	// and SchemaKept whether its schemas are stored under it
	// ([MCPToolSchema]): false for schemas past [MaxMCPToolSchemaBytes].
	Digest     string `json:"digest"`
	SchemaKept bool   `json:"schema_kept"`

	// Instances is how many of the node's instances list it: a per-agent
	// server's children list what each seat's credentials reach.
	Instances int `json:"instances"`
}

// MCPToolHints are one tool's four hints as words.
type MCPToolHints struct {
	ReadOnly    string `json:"read_only"`
	Destructive string `json:"destructive"`
	Idempotent  string `json:"idempotent"`
	OpenWorld   string `json:"open_world"`
}

func validHint(word string) bool {
	return word == HintYes || word == HintNo || word == HintUnknown
}

// Bounded is the listing held to its bounds: every text cut, the tools sorted
// by name and cut to [MaxMCPListedTools], ToolCount never below what is
// listed.
func (l MCPListing) Bounded() MCPListing {
	out := l
	out.At = l.At.UTC()
	out.Instructions = textcut.Within(l.Instructions, MaxMCPInstructionsBytes)
	if l.Info != nil {
		out.Info = &MCPServerInfo{
			Name:     textcut.Within(l.Info.Name, MaxMCPServerInfoBytes),
			Title:    textcut.Within(l.Info.Title, MaxMCPServerInfoBytes),
			Version:  textcut.Within(l.Info.Version, MaxMCPServerInfoBytes),
			Protocol: textcut.Within(l.Info.Protocol, MaxMCPServerInfoBytes),
		}
	}
	tools := slices.Clone(l.Tools)
	slices.SortFunc(tools, func(a, b MCPListedTool) int { return cmp.Compare(a.Name, b.Name) })
	out.ToolCount = max(l.ToolCount, len(tools))
	if len(tools) > MaxMCPListedTools {
		tools = tools[:MaxMCPListedTools]
	}
	for i := range tools {
		t := &tools[i]
		t.Name = textcut.Within(t.Name, MaxMCPToolNameBytes)
		t.Raw = textcut.Within(t.Raw, MaxMCPToolNameBytes)
		t.Title = textcut.Within(t.Title, MaxMCPToolTitleBytes)
		t.Description = textcut.Within(t.Description, MaxMCPToolDescriptionBytes)
	}
	if tools == nil {
		tools = []MCPListedTool{}
	}
	out.Tools = tools
	return out
}

// Validate reports why a listing cannot be written.
func (l MCPListing) Validate() error {
	switch {
	case l.Server == "" || l.Node == "":
		return errors.New("coord: an MCP listing needs the server and the node it is " +
			"one node's listing of")
	case l.Epoch <= 0:
		return fmt.Errorf("coord: %s's listing of %s needs the presence epoch it was "+
			"written under: a reader counts it only while the node holds that epoch", l.Node, l.Server)
	case l.At.IsZero():
		return fmt.Errorf("coord: %s's listing of %s needs when it was written", l.Node, l.Server)
	case l.Instances < 0 || l.ToolCount < 0:
		return fmt.Errorf("coord: %s's listing of %s counts below zero", l.Node, l.Server)
	}
	for _, t := range l.Tools {
		switch {
		case t.Name == "" || t.Raw == "":
			return fmt.Errorf("coord: %s's listing of %s names a tool with no name", l.Node, l.Server)
		case !IsMCPDigest(t.Digest):
			return fmt.Errorf("coord: %s's listing of %s names %s by %q, which is not a "+
				"definition digest", l.Node, l.Server, t.Name, t.Digest)
		case !validHint(t.Hints.ReadOnly) || !validHint(t.Hints.Destructive) ||
			!validHint(t.Hints.Idempotent) || !validHint(t.Hints.OpenWorld):
			return fmt.Errorf("coord: %s's listing of %s gives %s a hint that is not "+
				"yes, no or unknown", l.Node, l.Server, t.Name)
		case t.Instances < 0:
			return fmt.Errorf("coord: %s's listing of %s counts %s's instances below zero",
				l.Node, l.Server, t.Name)
		}
	}
	return nil
}

// MCPToolSchema is one definition's schemas, stored once under its digest.
type MCPToolSchema struct {
	Digest string `json:"digest"`
	// Input is the tool's input schema, and Output its output schema or
	// empty where it publishes none, each as encoded JSON.
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output,omitempty"`
	// At is when it was first stored.
	At time.Time `json:"at"`

	// Version is the store's version of the record as it was read. Not on
	// the wire.
	Version uint64 `json:"-"`
}

// MCPToolSchemaFits reports whether schemas this large may be stored.
func MCPToolSchemaFits(input, output []byte) bool {
	return len(input)+len(output) <= MaxMCPToolSchemaBytes
}

// Validate reports why a schema record cannot be written.
func (s MCPToolSchema) Validate() error {
	switch {
	case !IsMCPDigest(s.Digest):
		return fmt.Errorf("coord: an MCP tool schema is stored under its definition "+
			"digest, and %q is not one", s.Digest)
	case len(s.Input) == 0 || !json.Valid(s.Input):
		return fmt.Errorf("coord: the schema stored under %s needs its input schema as JSON", s.Digest)
	case len(s.Output) > 0 && !json.Valid(s.Output):
		return fmt.Errorf("coord: the output schema stored under %s is not JSON", s.Digest)
	case !MCPToolSchemaFits(s.Input, s.Output):
		return fmt.Errorf("coord: the schemas stored under %s are %d bytes, past the %d "+
			"a definition keeps", s.Digest, len(s.Input)+len(s.Output), MaxMCPToolSchemaBytes)
	case s.At.IsZero():
		return fmt.Errorf("coord: the schema stored under %s needs when it was stored", s.Digest)
	}
	return nil
}

// MCPListingUpdate is one change a listing watch delivers.
type MCPListingUpdate struct {
	// Listing is the record now held, nil when it was removed.
	Listing *MCPListing

	// Server and Node name the record the update is about. Empty on the
	// marker.
	Server string
	Node   string

	// Current marks the end of the records that existed when the watch
	// started, as [SeatPauseUpdate.Current] does.
	Current bool
}

// MCPListings is the fleet's record of what each node's MCP servers list.
//
// Every read RAISES rather than answering empty when the store cannot be
// read: an empty answer is a fleet whose servers list nothing, and a catalogue
// built on it would drop every per-agent server's tools over a store blip.
type MCPListings interface {
	// PutMCPListing writes one node's listing of one server, replacing
	// it, bounded ([MCPListing.Bounded]) and validated. LAST WRITER WINS,
	// as a node's positions row does: the only writer of a (server, node)
	// key is that node.
	PutMCPListing(ctx context.Context, l MCPListing) (MCPListing, error)

	// DeleteMCPListing removes one node's listing at a version, reporting
	// whether that version still held. A version of zero never holds.
	DeleteMCPListing(ctx context.Context, server, node string, version uint64) (bool, error)

	// ListMCPListings returns every listing, every node's, ordered by
	// server and then node.
	ListMCPListings(ctx context.Context) ([]MCPListing, error)

	// WatchMCPListings streams every listing, then the
	// [MCPListingUpdate.Current] marker, then every change after it. A
	// closed channel is a watcher that stopped hearing, as
	// [SeatPauses.WatchSeatPauses] says.
	WatchMCPListings(ctx context.Context) (<-chan MCPListingUpdate, error)

	// CreateMCPToolSchema stores a definition's schemas unless its digest
	// already holds some, reporting whether this call stored them. A digest
	// names one definition, so what it holds is what this call would have
	// written.
	CreateMCPToolSchema(ctx context.Context, s MCPToolSchema) (bool, error)

	// MCPToolSchema reads one definition's schemas.
	MCPToolSchema(ctx context.Context, digest string) (MCPToolSchema, bool, error)

	// ListMCPToolSchemas returns every stored schema, ordered by digest:
	// what the sweep weighs against the listings.
	ListMCPToolSchemas(ctx context.Context) ([]MCPToolSchema, error)

	// DeleteMCPToolSchema removes a definition's schemas at a version,
	// reporting whether that version still held.
	DeleteMCPToolSchema(ctx context.Context, digest string, version uint64) (bool, error)
}

// SortMCPListings orders listings by server and then node, the order every
// listing of them answers in.
func SortMCPListings(listings []MCPListing) {
	slices.SortFunc(listings, func(a, b MCPListing) int {
		return cmp.Or(cmp.Compare(a.Server, b.Server), cmp.Compare(a.Node, b.Node))
	})
}

// LiveMCPListings is the listings readers count: each one whose node holds its
// presence now AT THE EPOCH the listing was written under, presence being
// [Backend.ListLive] of [ClassNode]. In the order given.
//
// THE ONE FILTER, for every reader: a node that died, drained or restarted
// stops counting the moment its presence lapses or moves to a new epoch, and
// a reader that filtered on the node alone would count a restarted node's
// previous incarnation until the new one wrote over it.
func LiveMCPListings(listings []MCPListing, presence []Lease) []MCPListing {
	live := make(map[string]int64, len(presence))
	for _, lease := range presence {
		if node, ok := NodeID(lease.Resource); ok {
			live[node] = lease.Epoch
		}
	}
	out := make([]MCPListing, 0, len(listings))
	for _, l := range listings {
		if epoch, ok := live[l.Node]; ok && epoch == l.Epoch {
			out = append(out, l)
		}
	}
	return out
}
