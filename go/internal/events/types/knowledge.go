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

func (DocumentCreated) EventType() string { return "document_created" }

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

func (DocumentUpdated) EventType() string { return "document_updated" }

func (e DocumentUpdated) SummaryFor(actor string) string {
	if e.DocumentID != "" {
		return lead(actor, "updated document "+e.DocumentID)
	}
	return lead(actor, "updated a document")
}
