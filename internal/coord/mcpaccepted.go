package coord

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

// THE DEFINITION EACH MCP TOOL'S LEVEL WAS DECIDED ON (ADR-0078).
//
// A tool's level (Always allow, Needs approval, Blocked) is a person's
// decision about a tool as its server defined it then. A server controls its
// own definitions and can change one at any moment, a description reworded or
// a hint dropped, so a level that silently carried over would let a server
// turn a tool somebody allowed into one nobody looked at. Each tool's level
// therefore holds only for the definition it was decided on, named by its
// digest (internal/mcp's digest.go), and a tool the server lists under any
// other definition asks a person first until somebody accepts the change.
//
// # Who has to agree on it
//
// The whole company, now: every node holding a seat that may call the tool
// enforces the same answer, and a person accepting a change on any node's
// screen releases the tool on every node. It is machine state, decided by
// people, not operator configuration: it names digests no operator writes and
// changes whenever a server does, so it is not a field of the company
// document.
//
// # One record per tool, compare-and-set
//
// A record per (server, the server's own name for the tool), [MCPAcceptedKey],
// in the positions register, the bucket with no age: an accepted definition
// that aged out would put every tool of a quiet server back in front of a
// person. Three writers race for it (the node that lists a server first, and
// people accepting or blocking changes on any node), so every write is a
// compare-and-set on the version the writer read ([MCPAccepted.PutMCPAccepted]).
//
// # A server's first listing, and a removal, both keep a record
//
// A server's first listing is accepted as it is, by the node that lists it
// first: the operator who added the server chose it with that listing in front
// of them. A server is BASELINED once any record names it, and from then on a
// tool no record names is one it added, which waits like a changed one. So a
// removal a person accepts keeps its record, marked removed, rather than
// deleting it: a server that dropped every tool must not become a server
// listing for the first time again, whose next listing would be accepted
// unseen.
//
// # Bounded, and somebody else's words
//
// The record keeps the words of the definition it accepts (title,
// description, the hints the server declared), bounded as a listing bounds
// them, so a list can name what was accepted. A change is shown beside the
// definition whole, which the record's digest names in [MCPToolDefinition] and
// the sweep keeps while the record stands, months after the server stopped
// listing it. They are the server's words, and a reader treats them as such.

// MCPAcceptedClass is the key class accepted definitions are filed under in the
// positions register.
const MCPAcceptedClass = "mcp_accepted"

// MCPAcceptedKey is the accepted definition of one tool of one server, by the
// server's configured name and the server's own name for the tool.
func MCPAcceptedKey(server, tool string) string {
	return DocumentKey(MCPAcceptedClass, server, tool)
}

// MCPAcceptedOf recovers the server and tool from an accepted definition's key,
// false for a key of another class or one this grammar did not write.
func MCPAcceptedOf(key string) (server, tool string, ok bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 3 || segments[0] != MCPAcceptedClass ||
		segments[1] == "" || segments[2] == "" {
		return "", "", false
	}
	return segments[1], segments[2], true
}

// MaxMCPAcceptedByBytes bounds who accepted a definition: a person's or an
// agent's name, as an audit record names them.
const MaxMCPAcceptedByBytes = 256

// MCPToolPartLength is how many hex digits name one part of a definition
// ([MCPToolParts]): internal/mcp's PartLength.
const MCPToolPartLength = 16

// MCPToolParts names each part of a tool's definition apart (internal/mcp's
// Parts): two definitions of one tool differ in exactly the parts whose names
// differ, which says what a server changed without holding either whole.
type MCPToolParts struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Schema      string `json:"schema"`
	Hints       string `json:"hints"`
}

// Valid reports whether every part is named as internal/mcp names one.
func (p MCPToolParts) Valid() bool {
	for _, part := range []string{p.Title, p.Description, p.Schema, p.Hints} {
		if len(part) != MCPToolPartLength || !isLowerHex(part) {
			return false
		}
	}
	return true
}

// MCPAcceptedTool is the definition one tool's level was decided on.
type MCPAcceptedTool struct {
	// Server is the server's configured name and Tool the server's own
	// name for the tool, before any prefix: the record's key, stamped from
	// it by the store.
	Server string `json:"server"`
	Tool   string `json:"tool"`

	// Digest names the definition accepted (internal/mcp's digest.go). ""
	// for a removal accepted ([MCPAcceptedTool.Removed]).
	Digest string `json:"digest,omitempty"`

	// Removed is a removal accepted: the server stopped listing the tool,
	// and somebody agreed. A tool it lists again is one it added.
	Removed bool `json:"removed,omitempty"`

	// First is a definition accepted as part of the server's first
	// listing, by the node that listed it first rather than by a person.
	First bool `json:"first,omitempty"`

	// By is who accepted it, as the audit names them; "" for a first
	// listing. At is when.
	By string    `json:"by,omitempty"`
	At time.Time `json:"at"`

	// Title, Description and Hints are the definition's words as a listing
	// names them, Hints the server's own declared hints, before any
	// override: what a change is shown beside. Parts names each part of
	// the definition ([MCPToolParts]). All empty for a removal.
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Hints       MCPToolHints `json:"hints"`
	Parts       MCPToolParts `json:"parts"`

	// Version is the store's version of the record as it was read. Not on
	// the wire.
	Version uint64 `json:"-"`
}

// Bounded is the record held to its bounds: every text cut as a listing cuts
// it.
func (a MCPAcceptedTool) Bounded() MCPAcceptedTool {
	out := a
	out.At = a.At.UTC()
	out.Tool = textcut.Within(a.Tool, MaxMCPToolNameBytes)
	out.By = textcut.Within(a.By, MaxMCPAcceptedByBytes)
	out.Title = textcut.Within(a.Title, MaxMCPToolTitleBytes)
	out.Description = textcut.Within(a.Description, MaxMCPToolDescriptionBytes)
	return out
}

// Validate reports why an accepted definition cannot be written.
func (a MCPAcceptedTool) Validate() error {
	switch {
	case a.Server == "" || a.Tool == "":
		return errors.New("coord: an accepted MCP tool definition needs the server and the " +
			"server's own name for the tool")
	case a.At.IsZero():
		return fmt.Errorf("coord: the accepted definition of %s on %s needs when it was accepted",
			a.Tool, a.Server)
	case a.First && a.By != "":
		return fmt.Errorf("coord: the accepted definition of %s on %s is a first listing's, "+
			"which nobody accepted by name, and names %q", a.Tool, a.Server, a.By)
	case !a.First && a.By == "":
		return fmt.Errorf("coord: the accepted definition of %s on %s needs who accepted it",
			a.Tool, a.Server)
	}
	if a.Removed {
		if a.Digest != "" || a.First || a.Title != "" || a.Description != "" ||
			a.Hints != (MCPToolHints{}) || a.Parts != (MCPToolParts{}) {
			return fmt.Errorf("coord: the accepted removal of %s on %s names a definition: a "+
				"removal accepts that the server lists none", a.Tool, a.Server)
		}
		return nil
	}
	switch {
	case !IsMCPDigest(a.Digest):
		return fmt.Errorf("coord: the accepted definition of %s on %s is named by %q, which is "+
			"not a definition digest", a.Tool, a.Server, a.Digest)
	case !validHints(a.Hints):
		return fmt.Errorf("coord: the accepted definition of %s on %s gives a hint that is not "+
			"yes, no or unknown", a.Tool, a.Server)
	case !a.Parts.Valid():
		return fmt.Errorf("coord: the accepted definition of %s on %s does not name each of its "+
			"parts by %d hex digits", a.Tool, a.Server, MCPToolPartLength)
	}
	return nil
}

// isLowerHex reports whether s is lowercase hexadecimal digits only.
func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// MCPAcceptedUpdate is one change an accepted-definitions watch delivers.
type MCPAcceptedUpdate struct {
	// Accepted is the record now held, nil when it was removed.
	Accepted *MCPAcceptedTool

	// Server and Tool name the record the update is about. Empty on the
	// marker.
	Server string
	Tool   string

	// Current marks the end of the records that existed when the watch
	// started, as [MCPListingUpdate.Current] does.
	Current bool
}

// MCPAccepted is the fleet's record of the definition each MCP tool's level
// was decided on.
//
// Every read RAISES rather than answering empty when the store cannot be
// read: an empty answer is a fleet that has accepted nothing, which a reader
// would take for every server listing for the first time.
type MCPAccepted interface {
	// PutMCPAccepted writes one tool's accepted definition, bounded
	// ([MCPAcceptedTool.Bounded]) and validated, AT A VERSION: zero creates
	// it only where no record is held, any other replaces the record only
	// while it holds that version. False, and no record, where the version
	// did not hold: the caller reads the tool's record again.
	PutMCPAccepted(ctx context.Context, a MCPAcceptedTool, version uint64) (MCPAcceptedTool, bool, error)

	// DeleteMCPAccepted removes one tool's accepted definition at a
	// version, reporting whether that version still held. A version of
	// zero never holds.
	DeleteMCPAccepted(ctx context.Context, server, tool string, version uint64) (bool, error)

	// ListMCPAccepted returns every accepted definition, ordered by server
	// and then tool.
	ListMCPAccepted(ctx context.Context) ([]MCPAcceptedTool, error)

	// WatchMCPAccepted streams every accepted definition, then the
	// [MCPAcceptedUpdate.Current] marker, then every change after it. A
	// closed channel is a watcher that stopped hearing.
	WatchMCPAccepted(ctx context.Context) (<-chan MCPAcceptedUpdate, error)
}

// SortMCPAccepted orders accepted definitions by server and then tool, the
// order every listing of them answers in.
func SortMCPAccepted(accepted []MCPAcceptedTool) {
	slices.SortFunc(accepted, func(a, b MCPAcceptedTool) int {
		return cmp.Or(cmp.Compare(a.Server, b.Server), cmp.Compare(a.Tool, b.Tool))
	})
}
