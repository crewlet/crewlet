package coord

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// AN MCP RESTART is a person's request to restart a server now: every instance
// on every node, or one seat's on the node holding it. One record per server
// or restarted seat, watched by every node (internal/engine's mcprestart.go),
// shaped as the model retry (modelretry.go).
//
// A STAMP, NOT A COMMAND: a node restarts the instances whose current state (a
// session, or an outage) BEGAN BEFORE the record's instant, so a record heard
// twice, late or after a restart does exactly what it would have on time, and
// needs no expiry: the apply that removes the server deletes it. Only the
// seat's holder acts on a seat's record, and a seat that moved runs a child
// started after the stamp. A write keeps the LATER stamp, which restarts a
// superset ([MCPRestarts.PutMCPRestart]).

// MCPRestart is one restart request, as the coordination store holds it.
type MCPRestart struct {
	// Server is the configured server's name.
	Server string `json:"server"`

	// Seat is the agent id of the seat whose instance is restarted
	// (ADR-0013), or uuid.Nil for every instance of the server on every
	// node.
	Seat uuid.UUID `json:"seat"`

	// At is when it was asked for: every node restarts the instances whose
	// state began before it.
	At time.Time `json:"at"`

	// By, ByKind and OperatorID are who asked, as [ModelRetry] records them.
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Version is the store's version of the record as it was read. Opaque,
	// as [ModelRetry.Version] is. Not on the wire.
	Version uint64 `json:"-"`
}

// Validate reports why a restart cannot be written.
func (r MCPRestart) Validate() error {
	switch {
	case r.Server == "" || strings.Contains(r.Server, mcpRestartSeatMark):
		return errors.New("coord: an MCP restart needs the configured name of the server it restarts")
	case r.At.IsZero():
		return errors.New("coord: an MCP restart needs the instant it was asked for: " +
			"every node restarts what began before it")
	case r.By == "":
		return errors.New("coord: an MCP restart needs who asked for it")
	case !r.ByKind.Valid():
		return errors.New("coord: an MCP restart needs what sort of author asked for it " +
			"(iam.ActorFor's kind): agent, human, operator or system")
	case len(r.Server) > MaxMCPServerNameBytes:
		return errors.New("coord: an MCP restart names a server past its bound")
	}
	return checkActor("an MCP restart", r.By, r.OperatorID)
}

// MaxMCPServerNameBytes bounds the server a record names: a configured
// server's name (internal/config's MaxMCPServerName), with room.
const MaxMCPServerNameBytes = 64

// Entry is the record's name within its class ([MCPRestartEntry]).
func (r MCPRestart) Entry() string { return MCPRestartEntry(r.Server, r.Seat) }

// MCPRestartClass is the key class a restart is filed under in the positions
// register.
const MCPRestartClass = "mcp_restart"

// mcpRestartSeatMark joins a server's name and a seat's id in an entry. A
// server's name holds lowercase letters, digits and hyphens alone
// (internal/config's name rule), so it never contains the mark.
const mcpRestartSeatMark = "@"

// MCPRestartEntry names one restart within its class: the server alone for
// every instance of it, or the server and the seat's id for one seat's.
func MCPRestartEntry(server string, seat uuid.UUID) string {
	if seat == uuid.Nil {
		return server
	}
	return server + mcpRestartSeatMark + seat.String()
}

// MCPRestartTarget reads an entry back into its server and seat, reporting
// false for one [MCPRestartEntry] did not write.
func MCPRestartTarget(entry string) (string, uuid.UUID, bool) {
	server, seat, scoped := strings.Cut(entry, mcpRestartSeatMark)
	if server == "" {
		return "", uuid.Nil, false
	}
	if !scoped {
		return server, uuid.Nil, true
	}
	id, err := uuid.Parse(seat)
	if err != nil || id == uuid.Nil {
		return "", uuid.Nil, false
	}
	return server, id, true
}

// MCPRestartKey is an entry's key in the register.
func MCPRestartKey(entry string) string { return DocumentKey(MCPRestartClass, entry) }

// MCPRestartEntryOf recovers the entry from a key of the restart class,
// reporting false for a key of another class or one this grammar did not
// write.
func MCPRestartEntryOf(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != MCPRestartClass {
		return "", false
	}
	if _, _, ok := MCPRestartTarget(segments[1]); !ok {
		return "", false
	}
	return segments[1], true
}

// MCPRestartUpdate is one change a restart watch delivers.
type MCPRestartUpdate struct {
	// Restart is the record the entry now holds, nil when it was deleted.
	Restart *MCPRestart

	// Entry names the record the update is about ([MCPRestartEntry]).
	// Empty on the marker.
	Entry string

	// Current marks the end of the records that existed when the watch
	// started, as [ModelRetryUpdate.Current] does.
	Current bool
}

// MCPRestarts is the fleet's record of the restarts people asked for.
//
// Every read RAISES rather than answering empty when the store cannot be
// read: a restart a node missed is a server left as it was, which is what
// happens with no request at all, and raising keeps a reader from confusing
// the two.
type MCPRestarts interface {
	// MCPRestart reads one restart, by its entry ([MCPRestartEntry]).
	MCPRestart(ctx context.Context, entry string) (MCPRestart, bool, error)

	// ListMCPRestarts returns every restart, ordered by entry.
	ListMCPRestarts(ctx context.Context) ([]MCPRestart, error)

	// PutMCPRestart records a restart, keeping whichever of it and the
	// stored one is LATER, and returns the record the entry holds after the
	// call. A write that loses to a later stamp changes nothing.
	PutMCPRestart(ctx context.Context, r MCPRestart) (MCPRestart, error)

	// DeleteMCPRestart removes a restart at a version, reporting whether
	// that version still held. A version of zero never holds.
	DeleteMCPRestart(ctx context.Context, entry string, version uint64) (bool, error)

	// WatchMCPRestarts streams every restart, then the
	// [MCPRestartUpdate.Current] marker, then every change after it. A
	// closed channel is a watcher that stopped hearing, as
	// [SeatPauses.WatchSeatPauses] says.
	WatchMCPRestarts(ctx context.Context) (<-chan MCPRestartUpdate, error)
}
