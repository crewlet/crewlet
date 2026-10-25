package worklet_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// THE MAXIMAL COMMIT IS A TASK PATCH, NOT A BODY EDIT.
//
// Stating it as a body edit understates the real maximum by more than 3×, and
// three constants are sized from this number — the applier's fetch, its
// ack-pending window and the reorder buffer — so quoting the wrong class is
// how a batch ceiling ends up 2.4× its stated bound.
//
// The fixture is what the caps actually permit: a patch that touches every
// collection at every cap, carrying a body whose bytes escape six-fold,
// alongside a maximal notification. It is a MEASUREMENT rather than an
// assertion about one number, which is why it prints what it found.
func TestTheMaximalCommitFitsItsDesignMaximum(t *testing.T) {
	t.Parallel()

	// Control characters escape as \u00XX — six bytes for one — which is
	// the worst case a body can reach and the multiplier the bound is
	// derived from.
	escaping := strings.Repeat("\x01", worklet.MaxBody)

	fields := make(map[string]json.RawMessage, worklet.MaxFieldValues)
	for i := range worklet.MaxFieldValues {
		size := worklet.MaxFieldValueBytes
		if i < 4 {
			// The four textarea values, at their own larger ceiling.
			size = worklet.MaxTextareaBytes
		}
		fields[fmt.Sprintf("f%032d", i)] = json.RawMessage(
			`"` + strings.Repeat("x", size) + `"`)
	}

	checklists := make([]worklet.Checklist, 0, worklet.MaxChecklists)
	perList := worklet.MaxChecklistItemsTotal / worklet.MaxChecklists
	for l := range worklet.MaxChecklists {
		items := make([]worklet.ChecklistItem, 0, perList)
		for i := range perList {
			items = append(items, worklet.ChecklistItem{
				ID:       fmt.Sprintf("c%032d-%03d", l, i),
				Name:     strings.Repeat("n", worklet.MaxChecklistItemName),
				Assignee: "somebody-with-a-long-handle",
				Order:    i,
			})
		}
		checklists = append(checklists, worklet.Checklist{
			ID: fmt.Sprintf("l%032d", l), Name: strings.Repeat("t", worklet.MaxChecklistName),
			Items: items,
		})
	}

	relations := make([]worklet.Relation, 0,
		worklet.MaxWaitingOn+worklet.MaxOtherRelations)
	for i := range worklet.MaxWaitingOn + worklet.MaxOtherRelations {
		relations = append(relations, worklet.Relation{
			Kind:      worklet.RelationLinked,
			Other:     fmt.Sprintf("r%032d", i),
			Note:      strings.Repeat("s", 256),
			CreatedBy: "a-handle",
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		})
	}

	watchers := longHandles(worklet.MaxWatchers, 64)
	tags := longHandles(worklet.MaxTagsPerTask, 64)

	deltas := make(map[string]worklet.Delta, worklet.MaxDeltas)
	for i := range worklet.MaxDeltas {
		deltas[fmt.Sprintf("field_%02d", i)] = worklet.Delta{
			From: strings.Repeat("a", 1024), To: strings.Repeat("b", 1024),
		}
	}

	rec := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V:         worklet.RecordVersion,
			OpID:      "0193f0a0-0000-7000-8000-000000000001",
			Subject:   worklet.TaskSubject("b1b2b3b4-0000-4000-8000-000000000001"),
			Op:        worklet.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Gen:       1,
			Writer:    "node-with-a-long-name",
			Scope:     worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Expect:    918_280_001,
		Actor:     "an-agent-handle",
		ActorKind: worklet.AuthorAgent,
		TurnID:    "0193f0a0-0000-7000-8000-000000000002",
		Chain:     []string{"pm", "eng", "eng-2"},
		Notify: &worklet.Notify{
			Kind:     worklet.ChangeFields,
			Fields:   deltas,
			Excerpt:  strings.Repeat("e", worklet.MaxExcerpt),
			Mentions: longHandles(worklet.MaxMentions, 64),
			Snapshot: worklet.Snapshot{
				Key: "ENG-99999", Project: "ENG",
				Title:         strings.Repeat("T", worklet.MaxTitle),
				Status:        worklet.StatusInProgress,
				StatusGroup:   worklet.GroupActive,
				Assignee:      "an-assignee-handle",
				Reporter:      "a-reporter-handle",
				Watchers:      watchers,
				Collaborators: longHandles(worklet.MaxCollaborators, 64),
				ProjectLead:   "a-project-lead-handle",
			},
		},
	}
	patch := worklet.TaskPatch{
		Body:       &escaping,
		Fields:     &fields,
		Checklists: &checklists,
		Relations:  &relations,
		Watchers:   &watchers,
		Tags:       &tags,
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		t.Fatalf("encode the patch: %v", err)
	}
	rec.Mutation = payload

	body, err := rec.Encode()
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	t.Logf("the maximal task patch encodes to %d bytes (%.2f MiB) against a "+
		"design maximum of %d (%.2f MiB)",
		len(body), float64(len(body))/(1<<20),
		worklet.MaxCommitBytes, float64(worklet.MaxCommitBytes)/(1<<20))
	if len(body) > worklet.MaxCommitBytes {
		t.Fatalf("the maximal commit is %d bytes and the design maximum is %d — "+
			"three constants are sized from that number, so a caps change that "+
			"moves it moves them", len(body), worklet.MaxCommitBytes)
	}

	// AND IT IS NOT ABSURDLY UNDER IT EITHER. A bound the real maximum
	// cannot approach is a bound nobody is checking: it would pass just as
	// well if the patch stopped carrying its collections whole, which is
	// the property that makes a record able to rebuild the row.
	if len(body) < worklet.MaxCommitBytes/2 {
		t.Fatalf("the maximal commit is only %d bytes against a %d maximum — "+
			"either a collection stopped travelling whole, or the maximum is "+
			"describing a record class that no longer exists",
			len(body), worklet.MaxCommitBytes)
	}
}

// longHandles is n distinct handles of about width bytes each.
func longHandles(n, width int) []string {
	out := make([]string, 0, n)
	for i := range n {
		h := fmt.Sprintf("handle-%04d-", i)
		out = append(out, h+strings.Repeat("x", max(0, width-len(h))))
	}
	return out
}
