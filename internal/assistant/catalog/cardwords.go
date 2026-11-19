package catalog

import (
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// The card vocabulary the curated writes and previews draw with, and the rules
// a read's answer is fenced by where it carries text from outside.

// The column types, short.
const (
	tText     = wire.ColumnText
	tNumber   = wire.ColumnNumber
	tTokens   = wire.ColumnTokens
	tDuration = wire.ColumnDuration
	tDatetime = wire.ColumnDatetime
	tLabel    = wire.ColumnLabel
)

// prose is a table column a person or an agent wrote; fact is one fact of an
// entity card.
func prose(key, label, path string) cards.Column {
	return cards.Column{Key: key, Label: label, Path: path, Type: tText, Prose: true}
}

func fact(label, path string, typ wire.ColumnType) cards.Fact {
	return cards.Fact{Label: label, Path: path, Type: typ}
}

// one is a tool's single card spec.
func one(spec cards.Spec) map[string]cards.Spec { return map[string]cards.Spec{"": spec} }

var adminOnly = []iam.Grant{iam.GrantAdmin}

// statuses and priorities are Worklet's own closed sets (internal/worklet),
// which Worklet refuses anything outside.
var (
	statuses   = []string{"todo", "in_progress", "in_review", "done", "cancelled", "closed"}
	priorities = []string{"none", "low", "normal", "high", "urgent"}
)

// carriesRunAsk reports whether any row of answer at rows carries an executor's
// question at field.
func carriesRunAsk(answer any, rows, field string) bool {
	list, _ := cards.Lookup(answer, rows).([]any)
	for _, row := range list {
		if strings.TrimSpace(cards.Text(cards.Lookup(row, field))) != "" {
			return true
		}
	}
	return false
}

// knowledgeTrust is a knowledge search's trust, as each backend labels its own
// hits: Doclet's (whose answers carry the stored backend name "native",
// doclet.Backend) by the one rule for a surface the engine hosts, a
// colleague's with nobody named; a connected knowledge base's external, since
// people outside the company may write its pages and the answer names nobody.
func knowledgeTrust(answer any) Trusted {
	switch cards.Text(cards.Lookup(answer, "backend")) {
	case "", "native":
		return Trusted{Trust: provenance.TrustForHosted("", nil, false)}
	}
	return Trusted{Trust: provenance.TrustExternal, Source: provenance.SourcePageBody}
}
