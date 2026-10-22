package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// WHAT A TOOL MAY HAND BACK OF AN ARTIFACT'S CONTENT: an administrator's
// override is recorded before a row or a file is shown (D09), an artifact in
// the trash serves nothing, and a write the requester does not bound (an
// agent's edit to its own work) hands back nothing the requester could not
// read (D11).

// operatorTool is one tool of the operator catalogue over the fakes.
func operatorTool(t *testing.T, f *fakeArtifacts, data *fakeData, name string) tools.Callable {
	t.Helper()
	chart := artifactChart{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Artifacts: artifactDeps(f, chart, requesters{}), ArtifactData: artifactDataDeps(data),
		Authorize: builtin.Decide(chart)}) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("the operator catalogue serves no %s", name)
	return nil
}

// administrator is a person holding every grant and no relation to the
// agents' artifacts.
func administrator() context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{Kind: iam.KindPerson,
		Login: "root@example.com", Seat: "founder", Stage: iam.StageActive, Grants: iam.AllGrants})
}

// AN ADMINISTRATOR'S DATASTORE READ IS RECORDED BEFORE A ROW IS SHOWN (D09):
// read_artifact_data and query_artifact_data on an artifact the administrator
// holds no relation to write the elevated read first, and show no row when it
// cannot be written. Mutation: drop the record from dataTools.admit and the
// administrator reads every row with no trace.
func TestAnElevatedDataReadIsRecordedBeforeARowIsShown(t *testing.T) {
	t.Parallel()
	f, data := newFakeArtifacts(), newFakeData()
	h := f.seed("agent-ceo", iam.ActorAgent, ceo, nil, [2]string{"index.html", "<p>x</p>"})
	data.collection("salaries", artifacts.ReadEditors)
	data.row("salaries", "row-1", ceo, `{"pay":"secret-pay"}`)
	for i, name := range []string{builtin.ReadArtifactDataTool, builtin.QueryArtifactDataTool} {
		got, _ := operatorTool(t, f, data, name).Call(administrator(),
			map[string]any{"artifact": h.ID.String(), "collection": "salaries"})
		if got.Failed || !strings.Contains(got.Output, "secret-pay") {
			t.Fatalf("%s as an administrator answered %q", name, got.Output)
		}
		if len(f.elevated) != i+1 || !f.calls[i].Authority.Elevated {
			t.Fatalf("%s as an administrator left %d records", name, len(f.elevated))
		}
	}
	f.elevatedErr = errors.New("log refused")
	reads := len(data.readings)
	for _, name := range []string{builtin.ReadArtifactDataTool, builtin.QueryArtifactDataTool} {
		got, _ := operatorTool(t, f, data, name).Call(administrator(),
			map[string]any{"artifact": h.ID.String(), "collection": "salaries"})
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable ||
			strings.Contains(got.Output, "secret-pay") {
			t.Fatalf("%s whose record failed answered %v %q", name, tools.RefusalOf(got), got.Output)
		}
	}
	if len(data.readings) != reads {
		t.Errorf("an unrecorded elevated read reached the datastore %d times", len(data.readings)-reads)
	}
}

// AN ADMINISTRATOR'S PUBLISH ON A STALE BASE IS A RECORDED READ OF THE LIVE
// VERSION (D09): its live text is handed back only once the elevated read is
// written, and withheld when it cannot be. Mutation: inline the live files
// without ArtifactDeps.discloses and the record is missing.
func TestAnElevatedStalePublishRecordsTheLiveRead(t *testing.T) {
	t.Parallel()
	f, data := newFakeArtifacts(), newFakeData()
	h := f.seed("agent-ceo", iam.ActorAgent, ceo, nil, [2]string{"index.html", "<p>v1</p>"})
	f.republish(h.ID, ceo, [2]string{"index.html", "<p>live-secret</p>"})
	publish := operatorTool(t, f, data, builtin.PublishArtifactTool)
	args := map[string]any{"artifact": h.ID.String(), "base_version": 1, "content": "<p>mine</p>"}
	got, _ := publish.Call(administrator(), args)
	if tools.RefusalOf(got) != tools.RefusalStaleVersion || !strings.Contains(got.Output, "live-secret") {
		t.Fatalf("an administrator's stale publish answered %v %q", tools.RefusalOf(got), got.Output)
	}
	if len(f.elevated) != 1 || f.elevated[0] != 2 {
		t.Fatalf("the live version's read was recorded %v, want version 2 once", f.elevated)
	}
	f.elevatedErr = errors.New("log refused")
	got, _ = publish.Call(administrator(), args)
	if tools.RefusalOf(got) != tools.RefusalStaleVersion || strings.Contains(got.Output, "live-secret") ||
		!strings.Contains(got.Output, "live_files_withheld") {
		t.Fatalf("a stale publish whose record failed answered %q", got.Output)
	}
}

// AN ARTIFACT IN THE TRASH SERVES NO CONTENT: get_artifact, read_artifact_data
// and query_artifact_data on a trashed artifact are refused, naming the trash,
// and read nothing. Mutation: drop the status check from dataTools.admit and
// the rows are read.
//
// THE REFUSAL NAMES THE ARTIFACT AS THE MODEL DID — here by its label — and
// never by a uuid it did not pass, which is what it said. Mutation: name h.ID
// in artifactTrashed again and every row goes red.
func TestATrashedArtifactsContentIsNotRead(t *testing.T) {
	t.Parallel()
	r := newDataRig(t, artifactChart{}, requesters{})
	h := r.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>binned-page</p>"})
	r.data.collection("review", artifacts.ReadAll)
	r.data.row("review", "row-1", cto, `{"note":"binned-row"}`)
	r.art.mu.Lock()
	held := r.art.heads[h.ID]
	held.Status = artifactsdomain.StatusTrashed
	r.art.heads[h.ID] = held
	r.art.mu.Unlock()
	turn, _ := readerTurn(t)
	for _, name := range []string{builtin.GetArtifactTool, builtin.ReadArtifactDataTool,
		builtin.QueryArtifactDataTool} {
		label := h.ID.Label().String()
		args := map[string]any{"artifact": label, "collection": "review"}
		if name == builtin.GetArtifactTool {
			args = map[string]any{"artifact": label}
		}
		got := r.call(t, turn, name, args)
		if !got.Failed || !errors.Is(got.Cause, artifactsdomain.ErrTrashed) ||
			strings.Contains(got.Output, "binned-") {
			t.Errorf("%s on a trashed artifact answered %v %q", name, got.Cause, got.Output)
		}
		if !strings.Contains(got.Output, label) || strings.Contains(got.Output, h.ID.String()) {
			t.Errorf("%s names the trashed artifact as %q, want the label it was "+
				"passed and never its uuid", name, got.Output)
		}
	}
	if len(r.data.readings) != 0 {
		t.Errorf("a trashed artifact's datastore was read %d times", len(r.data.readings))
	}
}

// A STALE PUBLISH TO THE AGENT'S OWN WORK HANDS BACK NO TEXT ITS REQUESTER
// COULD NOT READ (D11): the publish skips the requester, as an edit to the
// agent's own work does, but the live files it would inline are a read, so a
// requester the artifact is not shared with is answered the numbers, the
// author and the paths alone, while one who views it is handed the text.
// Mutation: inline the live files without ArtifactDeps.discloses and the
// stranger to the artifact reads it.
func TestAStaleOwnPublishHandsBackNoTextTheRequesterCouldNotRead(t *testing.T) {
	t.Parallel()
	req := requesters{"founder": founderPrincipal(iam.Apps...)}
	r := newArtifactRig(t, artifactChart{}, req, nil)
	for _, c := range []struct {
		name   string
		shares []artifacts.Share
		shown  bool
	}{
		{"not shared with the requester", nil, false},
		{"viewed by the requester", []artifacts.Share{shareWith("founder", artifacts.RoleViewer)}, true},
	} {
		h := r.f.seed("agent-cto", iam.ActorAgent, cto, c.shares, [2]string{"index.html", "<p>v1</p>"})
		r.f.republish(h.ID, cto, [2]string{"index.html", "<p>private-live</p>"})
		got := r.call(t, asked(t), builtin.PublishArtifactTool, map[string]any{
			"artifact": h.ID.String(), "base_version": 1, "content": "<p>x</p>"})
		if tools.RefusalOf(got) != tools.RefusalStaleVersion {
			t.Fatalf("%s: answered %v %q", c.name, tools.RefusalOf(got), got.Output)
		}
		out := decode(t, got)
		_, inlined := out["live_files"]
		_, withheld := out["live_files_withheld"]
		if inlined != c.shown || withheld == c.shown ||
			strings.Contains(got.Output, "private-live") != c.shown {
			t.Errorf("%s: live files inlined %v, withheld %v: %s", c.name, inlined, withheld, got.Output)
		}
		if changed, _ := out["changed_paths"].([]any); len(changed) != 1 {
			t.Errorf("%s: changed_paths = %v, want the entry", c.name, out["changed_paths"])
		}
	}
}

// A CONFLICTING DATA WRITE HANDS BACK ONLY THE ROWS THE REQUESTER COULD READ
// (D11): on the agent's own artifact, which a requester steers without being
// shared it, no row at all; on somebody else's, the agent's own row in a
// private collection is left out, exactly as the read path leaves it out,
// while a row the requester wrote is shown. Mutation: answer res.Conflict
// unfiltered and both cases hand the requester rows they could not read.
func TestAConflictingDataWriteHandsBackOnlyWhatTheRequesterCouldRead(t *testing.T) {
	t.Parallel()
	req := requesters{"founder": founderPrincipal(iam.Apps...)}
	r := newDataRig(t, artifactChart{}, req)
	mine := r.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>a</p>"})
	theirs := r.art.seed("agent-ceo", iam.ActorAgent, ceo, []artifacts.Share{
		shareWith("agent-cto", artifacts.RoleEditor), shareWith("founder", artifacts.RoleViewer)},
		[2]string{"index.html", "<p>b</p>"})
	r.data.collection("drafts", artifacts.ReadPrivate)
	r.data.verdict = artifacts.DataConflict
	r.data.conflict = []artifactdata.Doc{
		{Collection: "drafts", ID: "cto-draft", Version: 4, Fields: json.RawMessage(`{"t":"cto-secret"}`),
			CreatedBy: cto, UpdatedBy: cto},
		{Collection: "drafts", ID: "founder-draft", Version: 2, Fields: json.RawMessage(`{"t":"founders"}`),
			CreatedBy: founder, UpdatedBy: founder},
	}
	write := func(h artifactsdomain.Head) (tools.Result, []string) {
		got := r.call(t, asked(t), builtin.WriteArtifactDataTool, map[string]any{
			"artifact": h.ID.String(), "ops": []any{map[string]any{"op": "merge",
				"collection": "drafts", "doc": "founder-draft", "if_version": 1,
				"fields": map[string]any{"t": "x"}}}})
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalConflict {
			t.Fatalf("a conflicting write answered %v %q", tools.RefusalOf(got), got.Output)
		}
		var out struct {
			Current []struct {
				Doc string `json:"doc"`
			} `json:"current"`
		}
		if err := json.Unmarshal([]byte(got.Output), &out); err != nil {
			t.Fatal(err)
		}
		var docs []string
		for _, row := range out.Current {
			docs = append(docs, row.Doc)
		}
		return got, docs
	}
	if got, docs := write(mine); len(docs) != 0 || strings.Contains(got.Output, "secret") {
		t.Errorf("a conflict on the agent's own unshared artifact handed back %v: %s", docs, got.Output)
	}
	if got, docs := write(theirs); len(docs) != 1 || docs[0] != "founder-draft" ||
		strings.Contains(got.Output, "cto-secret") {
		t.Errorf("a conflict for a viewer handed back %v: %s", docs, got.Output)
	}
}

// A MODEL IS TOLD THE ARTIFACT AS IT NAMED IT, on every refusal about one: a
// base past the head, a base the artifact no longer keeps, an asset on an
// artifact with no asset store and a path a version does not hold each named
// the resolved uuid, which a model that passed a label or a link never typed
// and does not know the artifact by. Each call here names it by its label, and
// a path the version does not hold names no artifact at all. Mutation: name
// h.ID in any of the four sentences again and its row goes red.
func TestARefusalNamesTheArtifactAsTheModelDid(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	turn := turnFor(t, "agent-cto")
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>one</p>"})
	label := h.ID.Label().String()
	check := func(name string, got tools.Result, says string) {
		t.Helper()
		if !got.Failed || !strings.Contains(got.Output, says) {
			t.Errorf("%s answered %q, want a refusal saying %q", name, got.Output, says)
			return
		}
		if !strings.Contains(got.Output, label) || strings.Contains(got.Output, h.ID.String()) {
			t.Errorf("%s names the artifact as %q, want the label it was passed and "+
				"never its uuid", name, got.Output)
		}
	}
	// A PATH THE VERSION DOES NOT HOLD names no artifact at all: the call
	// that asked is the one reading the answer.
	got := r.call(t, turn, builtin.GetArtifactTool, map[string]any{
		"artifact": label, "path": "missing.html"})
	if !got.Failed || !strings.Contains(got.Output, "has no file") ||
		strings.Contains(got.Output, h.ID.String()) {
		t.Errorf("a path the version does not hold answered %q", got.Output)
	}
	r.f.mu.Lock()
	delete(r.f.versions[h.ID], 1)
	r.f.mu.Unlock()
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		says       string
	}{
		{"a base past the head", builtin.PublishArtifactTool, map[string]any{
			"artifact": label, "base_version": 5, "content": "<p>two</p>"}, "is past it"},
		{"a base no longer kept", builtin.PublishArtifactTool, map[string]any{
			"artifact": label, "base_version": 1, "content": "<p>two</p>"}, "no longer keeps"},
		{"an asset with no asset store", builtin.UploadArtifactAssetTool, map[string]any{
			"artifact": label, "name": "logo.png", "content": "iVBORw0KGgo=",
			"encoding": "base64"}, "does not declare"},
		{"a version past the head", builtin.GetArtifactTool, map[string]any{
			"artifact": label, "version": 5}, "has versions 1.."},
		{"a version no longer kept", builtin.GetArtifactTool, map[string]any{
			"artifact": label, "version": 1}, "no longer keeps version 1"},
	} {
		check(tc.name, r.call(t, turn, tc.tool, tc.args), tc.says)
	}
}
