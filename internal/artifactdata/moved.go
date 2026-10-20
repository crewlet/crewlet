package artifactdata

import (
	"encoding/json"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHAT A COMMITTED BATCH MOVED, said after the commit.
//
// Every data node applies every record, and every node that serves a socket is
// a data node, so each node's applier is what tells that node's sockets about
// a document that changed: built from the rows the apply wrote rather than
// re-read, handed to the engine after the commit ([Engine.OnArtifactDataMoved]
// in internal/engine), and shaped per reader by whoever pushes it, since a
// document in an `own_rows`, `editors` or `private` collection reaches only the
// readers its rule admits.
//
// OVER-REPORTING IS THE SAFE DIRECTION: a listener told twice re-reads once
// more. A batch that retained a record this build cannot read moves everything
// ([Moved.Reset]).

// DocDelta is one document a record changed, as it stands after the record.
type DocDelta struct {
	Collection string `json:"collection"`
	Doc        string `json:"doc"`

	// Version is the document's version after the record: what a client
	// catching up by version compares.
	Version int64 `json:"version"`

	// Deleted says the record tombstoned it; Body is then empty.
	Deleted bool            `json:"deleted,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`

	// Read is the collection's read rule and CreatedBy the row's creator:
	// the two facts a push is shaped by, so a listener decides who may see
	// the delta without reading a row.
	Read      artifacts.ReadRule `json:"read"`
	CreatedBy string             `json:"created_by"`

	UpdatedBy iam.Actor `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WriteSummary is one record applied on a store, whatever its verdict.
type WriteSummary struct {
	Seq      int64                 `json:"seq"`
	OpID     string                `json:"op_id"`
	Op       OpKind                `json:"op"`
	Verdict  artifacts.DataVerdict `json:"verdict"`
	Actor    iam.Actor             `json:"actor"`
	ClientID string                `json:"client_id,omitempty"`
	At       time.Time             `json:"at"`
}

// Moved is one store's movement in a committed batch.
//
// TAGGED FOR THE WIRE, for [artifactsdomain.Moved]'s reason: a node whose copy
// is out of service hears a sound peer's movements relayed.
type Moved struct {
	// Artifact is the store's artifact; zero with Reset.
	Artifact artifacts.ID `json:"artifact,omitzero"`

	// Docs are the documents the batch changed, in the order it changed
	// them; a later delta for one document supersedes an earlier one.
	Docs []DocDelta `json:"docs,omitempty"`

	// Writes are the records the batch applied on the store.
	Writes []WriteSummary `json:"writes,omitempty"`

	// Schema says a record changed the store's collections, so a listener
	// drops any shaping it decided from the old rules.
	Schema bool `json:"schema,omitempty"`

	// Purged says the store is gone for good.
	Purged bool `json:"purged,omitempty"`

	// Reset names NO store: every store on this node may have moved with no
	// committed record to say which (a retained record, an adoption).
	Reset bool `json:"reset,omitempty"`

	// Through is the position of the batch's last record on the store, and
	// At the broker's own instant for it: when the change was stored, which
	// every node reads alike and what the socket's push of it is timed from
	// (internal/objectives' change visibility).
	Through statelog.Position `json:"through,omitzero"`
	At      time.Time         `json:"at,omitzero"`
}

// ResetMove is the movement that names everything.
func ResetMove() Moved { return Moved{Reset: true} }
