package atlaswake

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
)

var log = logging.Get("atlaswake")

// The metadata keys a drift wake carries.
const (
	EntryField   = "entry"
	KindField    = "kind"
	ChangesField = "changes"
	DayField     = "day"
)

// Parser turns one request into the wake of the seat it names; a seat the
// roster does not hold as an agent wakes nobody.
type Parser struct{}

var _ notify.Parser = Parser{}

// Source implements [notify.Parser].
func (Parser) Source() string { return Source }

// Parse implements [notify.Parser]: one wake, its id from [WakeID] and its
// instant the claim's, the same on every copy. No sender and no body: the
// turn is the company's own (ADR-0109), and nothing somebody wrote rides it.
func (Parser) Parse(_ context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	r, err := Decode(w)
	if err != nil {
		return nil, err
	}
	if reg != nil {
		party, ok := reg.ByHandle(r.Handle)
		if !ok || party.Human {
			log.Info("atlas_drift_reaches_nobody", "agent", r.Handle, "entry", r.Entry.String(),
				"detail", "the company no longer employs this handle as an agent seat")
			return nil, nil
		}
	}
	changes := make([]string, 0, len(r.Changes))
	for _, c := range r.Changes {
		changes = append(changes, string(c))
	}
	return []notify.Routed{{
		Inbound: notify.Inbound{
			Source:    Source,
			EventType: EventType,
			Subject:   Subject,
			Metadata: map[string]string{
				EntryField:   r.Entry.String(),
				KindField:    string(r.Kind),
				ChangesField: strings.Join(changes, ","),
				DayField:     r.Day,
			},
		},
		To:     notify.Recipient{Handle: r.Handle},
		WakeID: WakeID(r),
		WakeAt: r.At,
	}}, nil
}

// Subject is the one line a drift wake is named by: the same words on every
// one, so a label rather than content ([Prompt.SubjectIsLabel]).
const Subject = "An atlas entry you own needs a look"

// Prompt is what a drift wake asks of its seat: to read the entry it names
// and act within its own role. Never addressed, so the turn may end having
// only looked.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: always; the wake says where to
// look and nothing of what is there.
func (Prompt) RequiresRecon(notify.Inbound) bool { return true }

// SubjectIsLabel implements [notify.Prompt]: always, the subject is [Subject].
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return true }

// Addressed implements [notify.Prompt]: never. See the type's doc.
func (Prompt) Addressed(notify.Inbound) bool { return false }

// PartitionKey implements [notify.Prompt]: the entry, so two entries' wakes
// are two turns. A merge keeps only its latest wake's pointers, and a wake
// here is nothing but its pointer.
func (Prompt) PartitionKey(metadata map[string]string, _ string) string { return metadata[EntryField] }

// ConversationIdentity implements [notify.Prompt]: the same.
func (Prompt) ConversationIdentity(metadata map[string]string, _ string) string {
	return metadata[EntryField]
}

// DigestBody implements [notify.Prompt]: nothing; a wake has no body.
func (Prompt) DigestBody(string, string) string { return "" }

// WakesActor implements [notify.Prompt]: never; a drift names no actor.
func (Prompt) WakesActor(notify.Inbound) bool { return false }

// Provenance implements [notify.Prompt]: the atlas. The parser writes no
// body, so nothing is fenced under it.
func (Prompt) Provenance(notify.Inbound, notify.Parties) notify.Provenance {
	return notify.Provenance{Source: provenance.SourceAtlas}
}

// said is a change in the engine's own words, false for a word outside the
// closed set. A switch, so nothing is built at package init.
func said(c Change) (string, bool) {
	switch c {
	case ChangeAlerting:
		return "its health turned to alerting", true
	case ChangeContested:
		return "a newer report differs from a value a person set on it", true
	case ChangeDisputed:
		return "two of its sources disagree, and nobody has settled it", true
	}
	return "", false
}

// Build implements [notify.Prompt]: the entry by id and kind, and each change
// by its class, read back through the closed sets so a delivery's metadata
// can put no other word in the prompt.
func (Prompt) Build(n notify.Inbound, _ notify.Parties) string {
	var b strings.Builder
	b.WriteString("**Atlas.** An entry you own in the company's atlas changed.\n\n")
	b.WriteString("- **Entry:** `" + id(n.Metadata[EntryField]) + "`")
	if kind := kindOf(n.Metadata[KindField]); kind != "" {
		b.WriteString(" (" + kind + ")")
	}
	b.WriteString("\n")
	for part := range strings.SplitSeq(n.Metadata[ChangesField], ",") {
		if words, ok := said(Change(part)); ok {
			b.WriteString("- **Changed:** " + words + " (`" + part + "`)\n")
		}
	}
	b.WriteString("\n" + Task)
	if n.Body != "" {
		b.WriteString("\n\n" + n.Body)
	}
	return b.String()
}

// Task is a drift turn's task: look first, then act only within the role.
const Task = "This wake carries no detail on purpose. Read the entry with `" + toolname.AtlasGet +
	"` by that id, and what relies on it with `" + toolname.AtlasTrace + "`, before you decide anything: it may have settled " +
	"since. Then act as your role does for a service you answer for. A disagreement between " +
	"sources is settled by a person, so say what you found where your team will read it rather " +
	"than choosing a side. You are told about this entry at most once a day, and the turn may " +
	"end having only looked."

// id is s where it is a UUID, and a placeholder otherwise: the prompt names
// an id or nothing.
func id(s string) string {
	if parsed, err := uuid.Parse(s); err == nil {
		return parsed.String()
	}
	return "unknown"
}

// kindOf is s where it is a kind the atlas holds, and empty otherwise.
func kindOf(s string) string {
	if atlas.Kind(s).Valid() {
		return s
	}
	return ""
}
