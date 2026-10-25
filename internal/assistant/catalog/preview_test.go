package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// reader is a turn grant over answers, refusing anything it does not hold.
func reader(answers map[string]any) proposal.Read {
	return func(_ context.Context, r exec.Request) (exec.Response, error) {
		body, ok := answers[r.Path]
		if !ok {
			return exec.Response{Status: 404,
				Body: []byte(`{"error":"not_found","message":"No such record."}`)}, nil
		}
		b, err := json.Marshal(body)
		return exec.Response{Status: 200, Body: b}, err
	}
}

// EVERY WRITE IS PREVIEWED FROM THE OBJECT AS IT IS NOW, and its preview sends
// exactly the request the tool builds.
//
// Each proposing tool's preview reads what it changes through the turn's
// grant, answers a card the proposal package accepts (a title, an approve
// label, at least one change, an audience exactly where the tool declares
// ChangesVisibility), binds the
// revision it read, and carries the request byte for byte. A share to the whole
// company asks a step-up (D09). Mutation: build the preview's request from
// other arguments than the tool's, or drop the share's audience, and this
// fails, and so does unsetting share_artifact's ChangesVisibility.
func TestEveryWriteIsPreviewedFromTheObjectAsItIsNow(t *testing.T) {
	t.Parallel()
	read := reader(assistanttest.World())
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindWrite && tool.Kind != catalog.KindPageAction {
			continue
		}
		args, ok := assistanttest.ProposalArgs[tool.Name]
		if !ok {
			t.Errorf("%s proposes and this suite does not preview it", tool.Name)
			continue
		}
		p, err := tool.Preview(t.Context(), read, args)
		if err != nil {
			t.Errorf("%s: %v", tool.Name, err)
			continue
		}
		if err := p.Check(tool.ChangesVisibility); err != nil {
			t.Errorf("%s's preview: %v", tool.Name, err)
		}
		// A PREVIEW NAMING AN AUDIENCE IS A WRITE THAT DECLARES IT CHANGES
		// VISIBILITY, and the other way round, so the proposal is told to
		// hold the audience exactly where one is shown.
		if (len(p.Audience) > 0) != tool.ChangesVisibility {
			t.Errorf("%s previews the audience %v and declares ChangesVisibility %v",
				tool.Name, p.Audience, tool.ChangesVisibility)
		}
		want, err := tool.Request(args)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p.Request, want) {
			t.Errorf("%s previews %+v and the tool sends %+v", tool.Name, p.Request, want)
		}
		if p.SourceRevision != "" {
			if _, err := proposal.ParseRevision(p.SourceRevision); err != nil {
				t.Errorf("%s binds %q: %v", tool.Name, p.SourceRevision, err)
			}
			now, err := catalog.Revision(t.Context(), read, p.SourceRevision)
			if err != nil || now != p.SourceRevision {
				t.Errorf("%s's revision %q re-reads as %q, %v", tool.Name, p.SourceRevision, now, err)
			}
		}
	}
	share, _ := catalog.Lookup("share_artifact")
	p, err := share.Preview(t.Context(), read, assistanttest.ProposalArgs["share_artifact"])
	if err != nil || !p.StepUp || len(p.Audience) != 1 {
		t.Errorf("a company-wide share previews step-up %v, audience %v, %v", p.StepUp, p.Audience, err)
	}
	update, _ := catalog.Lookup("update_task")
	p, err = update.Preview(t.Context(), read, assistanttest.ProposalArgs["update_task"])
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]wire.ChangeKind{}
	for _, c := range p.Changes {
		kinds[c.Field] = c.Kind
	}
	if kinds["status"] != wire.ChangeChanged || kinds["body"] != wire.ChangeChanged ||
		kinds["task"] != wire.ChangeFact || p.SourceRevision != "task:ENG-42@3" || p.Note == "" {
		t.Errorf("the update previews %+v at %q", p.Changes, p.SourceRevision)
	}
}

// A CHANGE PREPARED AGAINST AN OLD VERSION IS STALE, and a revision that moved
// re-reads as another, so an approval never applies what the person did not
// see. Mutation: drop the if_match comparison and this fails.
func TestAChangePreparedAgainstAnOldVersionIsStale(t *testing.T) {
	t.Parallel()
	read := reader(assistanttest.World())
	update, _ := catalog.Lookup("update_task")
	args := map[string]any{"item": "ENG-42", "if_match": float64(2), "status": "done"}
	if _, err := update.Preview(t.Context(), read, args); !errors.Is(err, catalog.ErrStale) {
		t.Errorf("an update against version 2 of a task at 3 = %v, want ErrStale", err)
	}
	edit, _ := catalog.Lookup("edit_page")
	args = map[string]any{"page": assistanttest.RecordID, "base_version": float64(1), "body": "x"}
	if _, err := edit.Preview(t.Context(), read, args); !errors.Is(err, catalog.ErrStale) {
		t.Errorf("an edit against version 1 of a page at 2 = %v, want ErrStale", err)
	}
	moved := assistanttest.World()
	moved["/work/ENG-42"].(map[string]any)["task"].(map[string]any)["version"] = 4
	if now, err := catalog.Revision(t.Context(), reader(moved), "task:ENG-42@3"); err != nil ||
		now != "task:ENG-42@4" {
		t.Errorf("a moved task re-reads as %q, %v", now, err)
	}
	if _, err := catalog.Revision(t.Context(), read, "task:ENG-404@1"); !errors.Is(err,
		catalog.ErrPreviewRead) {
		t.Errorf("a task the person may not read = %v, want ErrPreviewRead", err)
	}
}
