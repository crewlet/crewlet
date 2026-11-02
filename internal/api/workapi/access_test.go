package workapi_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// THE COMMENT ROUTE LETS THE TOOL DECIDE.
//
// Who may post on an item's thread is a row: a tracker holder may say anything
// there, and the person an OPEN ask on the item names may post the answer to it
// and nothing else — and which ask is open is only known once the item is read.
// Decided at the route as `comment_on_work_item` on the bare kind, a person
// asked a question without Worklet was refused at the door to the one
// thread they were asked to answer on. So the route admits everybody and the
// tool decides, in the same words as everywhere else: the asked person's answer
// lands, the same person's remark on an item nobody asked them on is the
// table's refusal naming the tracker, and a tracker holder comments as ever.
func TestTheCommentRouteLetsTheToolDecide(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	r.reader.asks = map[string]map[string]string{"t-1": {"legal": "c-ask"}}
	legal := person("legal", iam.GrantDoclet)

	got := r.do(as(legal), http.MethodPost, "/worklet/items/ENG-1/comments",
		map[string]any{"body": "Yes — the clause stands."})
	if got.status != http.StatusOK {
		t.Fatalf("the person asked on ENG-1 could not answer it: %d %v", got.status, got.body)
	}
	if len(r.writes.updates) != 1 || r.writes.updates[0].Comment == nil ||
		r.writes.updates[0].Comment.Answers == nil ||
		*r.writes.updates[0].Comment.Answers != "c-ask" {
		t.Fatalf("the answer wrote %+v, want one comment answering c-ask", r.writes.updates)
	}

	got = r.do(as(legal), http.MethodPost, "/worklet/items/ENG-2/comments",
		map[string]any{"body": "fyi"})
	grants, _ := got.body["grants"].([]any)
	if got.status != http.StatusForbidden ||
		!slices.Equal(grants, []any{string(iam.GrantWorklet)}) {
		t.Errorf("a remark on an item nobody asked them on answered %d %v, want "+
			"403 naming the tracker", got.status, got.body)
	}
	if got := r.do(as(colleague("ana")), http.MethodPost, "/worklet/items/ENG-2/comments",
		map[string]any{"body": "fyi"}); got.status != http.StatusOK {
		t.Errorf("a tracker holder's comment answered %d %v", got.status, got.body)
	}
	if len(r.writes.updates) != 2 {
		t.Errorf("the tracker took %d comments, want the answer and the holder's",
			len(r.writes.updates))
	}
}

// THE TIDY ROUTES PASS THE AUTHOR.
//
// Whoever created a page may rename, trash and restore it, and whoever filed an
// item may remove and restore it — rules decided on the stored row's author
// beside its container, so a route that formed the object from the container
// alone would refuse every author who leads nothing. Each route is asked by the
// author, who leads nothing here, and by a colleague who neither created the
// row nor leads its container.
func TestTheTidyRoutesPassTheAuthor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, method, target string
		body                 any
	}{
		{"a page's rename", http.MethodPost, "/doclet/pages/p-1/rename",
			map[string]any{"title": "Runbook v2"}},
		{"a page's trash", http.MethodDelete, "/doclet/pages/p-1", nil},
		{"a page's restore", http.MethodPost, "/doclet/pages/p-1/restore", nil},
		{"an item's removal", http.MethodDelete, "/worklet/items/ENG-1", nil},
		{"an item's restore", http.MethodPost, "/worklet/items/ENG-1/restore", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, chart{})
			r.kb.page.Page.Author = "ana"
			item := r.reader.tasks["t-1"]
			item.Task.Reporter = "ana"
			r.reader.tasks["t-1"], r.reader.tasks["ENG-1"] = item, item

			if got := r.do(as(colleague("bo")), c.method, c.target, c.body); got.status != http.StatusForbidden {
				t.Errorf("a colleague who neither created it nor leads its "+
					"container answered %d %v", got.status, got.body)
			}
			if got := r.do(as(colleague("ana")), c.method, c.target, c.body); got.status != http.StatusOK {
				t.Errorf("its author answered %d %v", got.status, got.body)
			}
		})
	}
}

// A PURGE ASKS FOR A RECENT SIGN-IN.
//
// A purge destroys a row on every data node and nothing undoes it, so beside
// admin it asks for the step-up every other irreversible gesture asks for: an
// administrator whose proof is older than the window is refused
// `step_up_required` naming it, with nothing destroyed, and the same
// administrator a moment after proving who they are is served.
func TestAPurgeAsksForAStepUp(t *testing.T) {
	t.Parallel()
	for _, target := range []string{
		"/worklet/items/ENG-1/purge?confirm=ENG-1&reason=why",
		"/doclet/pages/p-1/purge?confirm=runbook&reason=why",
	} {
		r := newRig(t, chart{})
		stale := admin("root")
		stale.ReauthAt = time.Now().Add(-time.Minute)
		got := r.do(as(stale), http.MethodPost, target, nil)
		if got.status != http.StatusForbidden || got.body["error"] != "step_up_required" {
			t.Errorf("%s by an admin whose proof is stale answered %d %v, want 403 "+
				"step_up_required", target, got.status, got.body)
		}
		if len(r.writes.purged) != 0 || len(r.kb.did) != 0 {
			t.Fatalf("%s destroyed something for a stale proof: %v %v", target,
				r.writes.purged, r.kb.did)
		}
		if got := r.do(as(admin("root")), http.MethodPost, target, nil); got.status != http.StatusOK {
			t.Errorf("%s by an admin who proved who they are answered %d %v",
				target, got.status, got.body)
		}
	}
}
