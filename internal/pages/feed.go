package pages

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/coord"
)

// Source is the notification source name the native knowledge base registers
// under. Bare, for the reason [work.Source] is.
const Source = "page"

// Translator turns a change record into the delivery the parser reads.
type Translator struct {
	// skills is the container whose pages wake nobody. A tool skill is
	// machinery, and an edit to one must not notify a team about a
	// procedure written for a phase of a turn.
	skills string
}

// NewTranslator builds the knowledge base's feed translator.
//
// skills is the reserved tool-skills container, empty for a company that has
// turned tool skills off.
func NewTranslator(skills string) *Translator { return &Translator{skills: skills} }

// Family is the family this translator serves.
func (t *Translator) Family() coord.Family { return coord.FamilyPages }

// Class is the key class the feed filters on: the change class, never a
// rewritable key.
func (t *Translator) Class() string { return ClassChange }

// Source is the notification source name.
func (t *Translator) Source() string { return Source }

// Translate decides whether a change wakes anybody.
func (t *Translator) Translate(ctx context.Context, change coord.Change) (changefeed.Delivery, bool, error) {
	if change.Op == coord.OpPurge {
		return changefeed.Delivery{}, false, nil
	}
	record, err := DecodeChange(change.Value)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf(
			"pages: read the change on %s: %w", change.Key, err)
	}
	if record.Quiet {
		log.DebugContext(ctx, "pages_change_quiet", "change", record.ID,
			"page", record.Snapshot.Title)
		return changefeed.Delivery{}, false, nil
	}
	if t.skills != "" && record.Snapshot.Container == t.skills {
		// A TOOL SKILL. The sync worker picks it up on its own walk;
		// waking a team about it would put a procedure written for one
		// phase of one turn in front of somebody as knowledge.
		log.DebugContext(ctx, "pages_change_in_skills_container",
			"change", record.ID, "container", record.Snapshot.Container)
		return changefeed.Delivery{}, false, nil
	}
	if record.Snapshot.Status == StatusDraft {
		// A DRAFT IS SOMEBODY'S UNFINISHED THOUGHT. Waking a watcher for
		// every save while it is being written is how a page gets
		// abandoned in draft to avoid the noise.
		return changefeed.Delivery{}, false, nil
	}

	body, err := changeBody(record)
	if err != nil {
		return changefeed.Delivery{}, false, err
	}
	return changefeed.Delivery{Body: body, ID: record.ID, Actor: record.Actor}, true, nil
}

// changeBody renders a change as the map a parser reads, through JSON so the
// body is exactly the record — carried unknown fields included.
func changeBody(record Change) (map[string]any, error) {
	data, err := EncodeChange(record)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("pages: render the change body: %w", err)
	}
	return body, nil
}
