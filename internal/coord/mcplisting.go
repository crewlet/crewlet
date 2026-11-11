package coord

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
// definition, its schemas and its words whole, is one record per definition
// digest, [MCPDefinitionKey], written create-only, so a definition every
// seat's child of a server lists is stored once however many nodes and
// instances list it. Both live in the positions
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
// # Every tool, bounded by bytes, and somebody else's words
//
// A listing names EVERY tool its node's instances list, because what it does
// not name is never reviewed, so a call to it would ask a person for ever
// (ADR-0078). It is bounded by the bytes it is stored as rather than by a
// count of tools: each tool's identity (its names, digest, parts and hints) is
// always on it, and its words give way first ([MCPListing.Bounded]).
// Every field of a listing but its counts came from a remote MCP server, so
// every one is bounded where the record is built and again by the store that
// writes it, and a reader treats them as the server's words. A record's key names its server and node, and a store stamps both
// from the key it files the record under, so a node can describe only its
// own listing of the server the key names.

// MCPListingClass and MCPDefinitionClass are the key classes the records are
// filed under in the positions register.
const (
	MCPListingClass    = "mcp_listing"
	MCPDefinitionClass = "mcp_definition"
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

// MCPDefinitionKey is one definition, by its digest.
func MCPDefinitionKey(digest string) string { return DocumentKey(MCPDefinitionClass, digest) }

// MCPDefinitionDigest recovers the digest from a definition key, false for a key of
// another class or one naming no digest.
func MCPDefinitionDigest(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != MCPDefinitionClass || !IsMCPDigest(segments[1]) {
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

// The bounds a listing is held to, each measured as the record is stored:
// encoded as JSON, where `<`, `>`, `&`, a control character or U+2028 takes six
// bytes and an invalid byte three, so a bound on the raw text would let a
// record grow several times past it.
const (
	// MaxMCPListingBytes bounds one listing's record, encoded. 4 MiB is half
	// of the 8 MiB message every broker the engine runs on carries
	// (internal/queue's MaxPayloadBytes): it holds the identity of the most
	// tools the engine takes from one server (internal/mcp's MaxServerTools,
	// 4,096) at under 600 bytes each, with room for their words, and the
	// largest server seen in use, at 461 tools, writes about 300 KiB. A
	// listing whose identities alone are past it is refused, never cut.
	MaxMCPListingBytes = 4 << 20

	// MaxMCPToolTitleBytes bounds a tool's title on a listing, and
	// MaxMCPToolDescriptionBytes its description: what a person reading a
	// list needs. The digest covers every byte, and the definition's record
	// keeps its words whole ([MCPToolDefinition]). Raw bytes, as a cut is
	// made; the record holding them is bounded encoded.
	MaxMCPToolTitleBytes       = 256
	MaxMCPToolDescriptionBytes = 1024

	// MaxMCPInstructionsBytes bounds the instructions a server offers in
	// its handshake.
	MaxMCPInstructionsBytes = 4096

	// MaxMCPServerInfoBytes bounds each field of the server's identity.
	MaxMCPServerInfoBytes = 256

	// MaxMCPToolSchemaBytes bounds one definition's schemas, input and
	// output together, as stored. A definition whose schemas are larger is
	// listed without its record ([MCPListedTool.DefinitionKept] false): 64
	// KiB is past any schema a model is usefully offered.
	MaxMCPToolSchemaBytes = 64 << 10

	// MaxMCPToolWordsBytes bounds one definition's words whole, as stored:
	// its title, the title its hints carry and its description, together. The engine
	// bounds nothing a server sends and hands a model every byte of a
	// description on every request, so 64 KiB, some 16,000 tokens of one
	// tool, is past any description a server usefully sends (real ones run
	// to a few KiB). A definition whose words are longer is listed without
	// its record, and a change to it is said rather than shown.
	MaxMCPToolWordsBytes = 64 << 10

	// MaxMCPListingDefinitionBytes bounds the records one listing keeps,
	// every kept definition's schemas and words together, as stored: a tool whose
	// definition waits for a review is kept first, so its change can be
	// shown, then the rest in name order while they fit, and a tool past
	// the budget is listed without its record. Held by the node that builds
	// the listing, since a listing names its records by digest and a store
	// writing it cannot weigh them. 1 MiB is eight of the largest
	// definitions, and hundreds of the definitions real servers publish.
	MaxMCPListingDefinitionBytes = 1 << 20
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

	// Tools are every tool the node's instances list, offered and blocked
	// ([MCPListedTool.Blocked]) alike, sorted by name.
	Tools []MCPListedTool `json:"tools"`

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

// MCPListedTool is one tool a node's instances of a server list.
type MCPListedTool struct {
	// Name is the name it is offered under, the server's tool_prefix
	// applied; Raw is the server's own name for it.
	Name string `json:"name"`
	Raw  string `json:"raw"`

	// Title and Description are its words, cut to
	// [MaxMCPToolTitleBytes] and [MaxMCPToolDescriptionBytes], and shorter
	// where the listing's bytes would not hold them ([MCPListing.Bounded]).
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`

	// Hints are its behavioural hints with the company's overrides applied,
	// each [HintYes], [HintNo] or [HintUnknown]; Declared the hints as the
	// server declared them, no override applied, which is the server's own
	// claim and what a change to the tool is judged by ([MCPAcceptedTool]).
	Hints    MCPToolHints `json:"hints"`
	Declared MCPToolHints `json:"declared"`

	// Digest names the server's definition (internal/mcp's digest.go),
	// and DefinitionKept whether its record is stored under it
	// ([MCPToolDefinition]): false for a definition past its bounds
	// ([MCPToolDefinition.Fits]), and for a tool past its listing's
	// [MaxMCPListingDefinitionBytes].
	Digest         string `json:"digest"`
	DefinitionKept bool   `json:"definition_kept"`

	// Parts names each part of the definition apart ([MCPToolParts]), so
	// a reader comparing it with the definition its level was decided on
	// can say which part changed.
	Parts MCPToolParts `json:"parts"`

	// Instances is how many of the node's instances list it: a per-agent
	// server's children list what each seat's credentials reach.
	Instances int `json:"instances"`

	// Blocked is a tool the server's entry blocks (its tool levels): the
	// node's instances list it and never file it, so no agent is offered,
	// lists or calls it. Listed all the same, so a reader of what the
	// server lists sees what was held back and can let it through again.
	Blocked bool `json:"blocked,omitempty"`
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

// validHints reports whether each of the four hints is a hint's word.
func validHints(h MCPToolHints) bool {
	return validHint(h.ReadOnly) && validHint(h.Destructive) && validHint(h.Idempotent) &&
		validHint(h.OpenWorld)
}

// Bounded is the listing held to its bounds: every text cut, the tools sorted
// by name, and, while the record is past [MaxMCPListingBytes] as stored,
// every description cut to half as many bytes again, then every title, each
// cut marked. EVERY TOOL STAYS: a listing whose identities alone are past the
// bound is refused by [MCPListing.Validate].
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
	// THE WORDS GIVE WAY, never a tool, all alike: the definition's record
	// keeps each tool's words whole.
	description, title := MaxMCPToolDescriptionBytes, MaxMCPToolTitleBytes
	for out.EncodedSize() > MaxMCPListingBytes && title > 0 {
		if description > 0 {
			description /= 2
		} else {
			title /= 2
		}
		out.Tools = slices.Clone(tools)
		for i := range out.Tools {
			t := &out.Tools[i]
			t.Description = textcut.Within(t.Description, description)
			t.Title = textcut.Within(t.Title, title)
		}
	}
	return out
}

// EncodedSize is how many bytes the listing is stored as.
func (l MCPListing) EncodedSize() int { return encodedLen(l) }

// encodedLen is how many bytes v encodes to as the store writes it, or one
// past every bound for a value that does not encode.
func encodedLen(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return math.MaxInt32
	}
	return len(raw)
}

// encodedText is how many bytes s takes inside a stored record, its quotes
// left out.
func encodedText(s string) int {
	if s == "" {
		return 0
	}
	return encodedLen(s) - 2
}

// encodedJSON is how many bytes an encoded JSON value takes inside a stored
// record, which compacts it and escapes it as it does a string.
func encodedJSON(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	return encodedLen(raw)
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
	case l.Instances < 0:
		return fmt.Errorf("coord: %s's listing of %s counts below zero", l.Node, l.Server)
	}
	if size := l.EncodedSize(); size > MaxMCPListingBytes {
		return fmt.Errorf("coord: %s's listing of %s names %d tools in %d bytes as stored, past the %d "+
			"one listing holds even with their words left off: every tool a listing does not name "+
			"asks a person before each call, so the listing is refused rather than cut. Narrow what "+
			"the server lists (its own toolsets or filters), or split it into several entries",
			l.Node, l.Server, len(l.Tools), size, MaxMCPListingBytes)
	}
	for _, t := range l.Tools {
		switch {
		case t.Name == "" || t.Raw == "":
			return fmt.Errorf("coord: %s's listing of %s names a tool with no name", l.Node, l.Server)
		case !IsMCPDigest(t.Digest):
			return fmt.Errorf("coord: %s's listing of %s names %s by %q, which is not a "+
				"definition digest", l.Node, l.Server, t.Name, t.Digest)
		case !validHints(t.Hints) || !validHints(t.Declared):
			return fmt.Errorf("coord: %s's listing of %s gives %s a hint that is not "+
				"yes, no or unknown", l.Node, l.Server, t.Name)
		case !t.Parts.Valid():
			return fmt.Errorf("coord: %s's listing of %s does not name each part of %s's "+
				"definition by %d hex digits", l.Node, l.Server, t.Name, MCPToolPartLength)
		case t.Instances < 0:
			return fmt.Errorf("coord: %s's listing of %s counts %s's instances below zero",
				l.Node, l.Server, t.Name)
		}
	}
	return nil
}

// MCPToolDefinition is one definition whole, stored once under its digest: its
// schemas, which a listing names rather than carries, and its words, which a
// listing cuts. What a change to the tool is shown by, part by part.
type MCPToolDefinition struct {
	Digest string `json:"digest"`
	// Title is the tool's own title, HintsTitle the title its hints carry,
	// and Description what the model is told it does, each as the server
	// sent it: the words the digest's title and description parts cover.
	Title       string `json:"title,omitempty"`
	HintsTitle  string `json:"hints_title,omitempty"`
	Description string `json:"description"`
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

// Fits reports whether the definition is within the bounds a record keeps, as
// it is stored: its schemas within [MaxMCPToolSchemaBytes] and its words
// within [MaxMCPToolWordsBytes].
func (d MCPToolDefinition) Fits() bool {
	return d.schemaBytes() <= MaxMCPToolSchemaBytes && d.wordBytes() <= MaxMCPToolWordsBytes
}

// Size is what the record weighs against its listing's
// [MaxMCPListingDefinitionBytes]: its schemas and its words, as stored.
func (d MCPToolDefinition) Size() int { return d.schemaBytes() + d.wordBytes() }

// schemaBytes is the definition's two schemas as stored.
func (d MCPToolDefinition) schemaBytes() int { return encodedJSON(d.Input) + encodedJSON(d.Output) }

// wordBytes is the definition's three texts as stored.
func (d MCPToolDefinition) wordBytes() int {
	return encodedText(d.Title) + encodedText(d.HintsTitle) + encodedText(d.Description)
}

// Validate reports why a definition record cannot be written.
func (d MCPToolDefinition) Validate() error {
	switch {
	case !IsMCPDigest(d.Digest):
		return fmt.Errorf("coord: an MCP tool definition is stored under its digest, and %q is "+
			"not one", d.Digest)
	case len(d.Input) == 0 || !json.Valid(d.Input):
		return fmt.Errorf("coord: the definition stored under %s needs its input schema as JSON", d.Digest)
	case len(d.Output) > 0 && !json.Valid(d.Output):
		return fmt.Errorf("coord: the output schema stored under %s is not JSON", d.Digest)
	case !d.Fits():
		return fmt.Errorf("coord: the definition stored under %s holds %d bytes of schemas and %d of "+
			"words as stored, past the %d and %d a record keeps", d.Digest, d.schemaBytes(),
			d.wordBytes(), MaxMCPToolSchemaBytes, MaxMCPToolWordsBytes)
	case d.At.IsZero():
		return fmt.Errorf("coord: the definition stored under %s needs when it was stored", d.Digest)
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

	// CreateMCPToolDefinition stores a definition unless its digest already
	// holds one, reporting whether this call stored it. A digest names one
	// definition, so what it holds is what this call would have written.
	CreateMCPToolDefinition(ctx context.Context, s MCPToolDefinition) (bool, error)

	// MCPToolDefinition reads one definition.
	MCPToolDefinition(ctx context.Context, digest string) (MCPToolDefinition, bool, error)

	// ListMCPToolDefinitions returns every stored definition, ordered by digest:
	// what the sweep weighs against the listings.
	ListMCPToolDefinitions(ctx context.Context) ([]MCPToolDefinition, error)

	// DeleteMCPToolDefinition removes a definition at a version,
	// reporting whether that version still held.
	DeleteMCPToolDefinition(ctx context.Context, digest string, version uint64) (bool, error)
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
