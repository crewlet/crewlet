package types

import "github.com/crewlet/crewlet/internal/events"

// Shared-knowledge writes. ScopeKind names whose knowledge it is (agent, unit,
// org) — the same scope the knowledge search reads back.

func init() {
	events.Register[DocumentCreated]()
	events.Register[DocumentUpdated]()
}

// DocumentCreated marks a document landing in the shared knowledge base.
type DocumentCreated struct {
	DocumentID string `json:"document_id"`
	ScopeKind  string `json:"scope_kind"`
	Title      string `json:"title"`
}

// EventType is the "document_created" wire type.
func (DocumentCreated) EventType() string { return "document_created" }

// SummaryFor names the author, who rides on the envelope's source: neither
// document event carries a role, so the actor chain resolves to the publisher.
func (e DocumentCreated) SummaryFor(actor string) string {
	if e.Title != "" {
		return lead(actor, "created document '"+e.Title+"'")
	}
	return lead(actor, "created a document")
}

// DocumentUpdated marks an existing document changing.
type DocumentUpdated struct {
	DocumentID string `json:"document_id"`
	ScopeKind  string `json:"scope_kind"`
}

// EventType is the "document_updated" wire type.
func (DocumentUpdated) EventType() string { return "document_updated" }

// SummaryFor falls back to the document id, since an update carries no title —
// the title lives on the document, which may itself have just been renamed.
func (e DocumentUpdated) SummaryFor(actor string) string {
	if e.DocumentID != "" {
		return lead(actor, "updated document "+e.DocumentID)
	}
	return lead(actor, "updated a document")
}
