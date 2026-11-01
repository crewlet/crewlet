package builtin_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/caveat/caveattest"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// personAnswer is one answer that landed, as a person's surface answers it:
// the tool's answer for a person without its model-only keys
// ([builtin.ForPerson]) and its caveats in a person's words, decoded.
func personAnswer(t *testing.T, name string, res tools.Result) map[string]any {
	t.Helper()
	if res.Failed {
		t.Fatalf("%s was refused: %s", name, res.Output)
	}
	raw, ok := builtin.ForPerson(res)
	if !ok {
		t.Fatalf("%s answered something that is not a receipt: %s", name, res.Output)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	shown, err := json.Marshal(caveat.ForPeople(res.Caveats))
	if err != nil {
		t.Fatal(err)
	}
	var caveats []any
	if err := json.Unmarshal(shown, &caveats); err != nil {
		t.Fatal(err)
	}
	out["caveats"] = caveats
	return out
}

// everyLandedAnswer is one successful answer of every write a person's
// surface can reach (the operator catalogue's writes), and every partial
// outcome, keyed by a name for each.
func everyLandedAnswer(t *testing.T) map[string]tools.Result {
	t.Helper()
	out := map[string]tools.Result{}
	call := func(tool tools.Callable, w *ledgerFake, args map[string]any) tools.Result {
		res, err := tool.Call(w.asked(t, tool, request(1), args), args)
		if err != nil {
			t.Fatalf("%s: %v", tool.Name(), err)
		}
		return res
	}

	// THE TRACKER'S AND THE KNOWLEDGE BASE'S, through the fakes every
	// other walk over the writes uses.
	w := newLedgerFake(statelog.OutcomeApplied)
	for name, tool := range writeSurface(w) {
		out[name] = call(tool, w, everyWriteCall[name])
	}

	// THE PARTIAL OUTCOMES: a call that landed part of what it was asked.
	// THE THIRD WRITE: the create's labels, the item, then what it waits on.
	deps := newLedgerFake(statelog.OutcomeApplied)
	deps.failAt = 3
	out["create_work_item, its dependencies refused"] = call(
		writeSurface(deps)[worklet.CreateWorkItemTool], deps, everyWriteCall[worklet.CreateWorkItemTool])
	// THE SECOND: the patch, then what it waits on.
	deps = newLedgerFake(statelog.OutcomeApplied)
	deps.failAt = 2
	out["update_work_item, its dependencies refused"] = call(
		writeSurface(deps)[worklet.UpdateWorkItemTool], deps, everyWriteCall[worklet.UpdateWorkItemTool])
	for _, waiting := range []string{"", "ENG-4"} {
		moved := newLedgerFake(statelog.OutcomeApplied)
		moved.stop = &worklet.MoveStopped{Root: "id-1", Key: "OPS-3", Target: "OPS",
			Followed: 1, Of: 3, Waiting: waiting, Err: worklet.ErrStepUnresolved}
		out["move_work_item, its walk stopped "+waiting] = call(
			writeSurface(moved)[worklet.MoveWorkItemTool], moved, everyWriteCall[worklet.MoveWorkItemTool])
	}
	for _, name := range []string{worklet.RemoveWorkItemTool, worklet.RestoreWorkItemTool} {
		walked := newLedgerFake(statelog.OutcomeApplied)
		walked.stop = &worklet.SubtreeStopped{Root: "id-1", Followed: 1, Of: 3,
			Err: worklet.ErrStepUnresolved}
		out[name+", its walk stopped"] = call(writeSurface(walked)[name], walked, everyWriteCall[name])
	}

	// A PROJECT'S FILES.
	files, objects := newFakeFiles(), newFakeObjects()
	reg := workRegistry(t, fileDeps(files, objects))
	out[worklet.WriteProjectFileTool] = callWork(t, reg, worklet.WriteProjectFileTool,
		map[string]any{"path": "reports/q3.md", "content": "# Q3"})
	out[worklet.RemoveProjectFileTool] = callWork(t, reg, worklet.RemoveProjectFileTool,
		map[string]any{"path": "reports/q3.md"})

	// THE COMPANY'S CODE: each write landed on a scratch repository, with the
	// arguments the hold walk lands it with ([holdingCall]).
	for _, name := range builtin.CodeWrites() {
		args, _ := holdingCall(name)
		reg := codeRegistry(t, writeDeps(scratchHost(time.Hour), &codeWrites{}))
		out[name] = callWrite(t, everyGrant(), reg, writerTurn(t), name, args)
	}
	// AND THE PERSON'S OWN CODE MARK, which only the operator catalogue serves.
	marks := codeDeps(newCodeHost())
	marks.Writer = &codeWrites{}
	marked, err := readersCatalogueOver(marks, nil)[builtin.CodeMarkInboxTool].Call(asBoundPerson(),
		map[string]any{"read": []any{"r-1"}})
	if err != nil {
		t.Fatalf("%s: %v", builtin.CodeMarkInboxTool, err)
	}
	out[builtin.CodeMarkInboxTool] = marked

	// THE OPERATOR'S OWN VERBS.
	desk := &deskFake{runs: map[string]sandbox.PendingRun{"t1": parked(t, "t1", sandbox.StatusReseed)}}
	answered, err := answerRunTool(t, desk).Call(t.Context(),
		map[string]any{"turn_id": "t1", "launch_id": "launch-t1", "answer": "use main"})
	if err != nil {
		t.Fatal(err)
	}
	out[builtin.AnswerRunTool] = answered
	pauses := newPauseRig(t, coordmem.NewFleet(), boundTo("jane"))
	out[builtin.PauseSeatTool], _ = pauses.call(t, builtin.PauseSeatTool,
		map[string]any{"handle": "agent-cto", "reason": "looping"})
	out[builtin.ResumeSeatTool], _ = pauses.call(t, builtin.ResumeSeatTool,
		map[string]any{"handle": "agent-cto"})
	asker := &fleetAsker{replies: []steer.Reply{{Version: 1, TurnID: "run-1",
		AgentHandle: "swe", Status: steer.StatusAccepted}}}
	out[builtin.SteerTurnTool], _ = steerCall(t, steerTool(t, asker, requestKey()), "run-1", "use staging")
	out[builtin.AnswerKnowledgeTool], _, _ = ask(t, newAnswerRig().tool(t), "How do we deploy?")

	// THE ARTIFACTS AND THEIR DATASTORES.
	chart := artifactChart{}
	req := requesters{"founder": founderPrincipal(iam.Apps...)}
	art, _ := folderRig(t)
	turn, _ := readerTurn(t)
	h := art.f.seed("agent-cto", iam.ActorAgent, cto,
		[]artifacts.Share{shareWith("founder", artifacts.RoleEditor)},
		[2]string{"index.html", "<p>ok</p>"})
	for name, args := range map[string]map[string]any{
		builtin.CreateArtifactTool: {"kind": "html", "title": "Review sheet",
			"content": "<p>one</p>"},
		builtin.PublishArtifactTool: {"artifact": h.ID.String(), "base_version": 1,
			"content": "<p>two</p>"},
		builtin.UpdateArtifactTool:       {"artifact": h.ID.String(), "icon": "chart"},
		builtin.ShareArtifactTool:        {"artifact": h.ID.String(), "to": "agent-ceo", "role": "viewer"},
		builtin.UnshareArtifactTool:      {"artifact": h.ID.String(), "from": "agent-ceo"},
		builtin.CreateArtifactFolderTool: {"parent": "Marketing", "name": "Q3"},
		builtin.MoveArtifactTool:         {"artifacts": []any{h.ID.String()}, "folder": "Marketing"},
	} {
		out[name] = art.call(t, turn, name, args)
	}
	upload := newArtifactRig(t, chart, req, nil)
	held := upload.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	upload.f.mu.Lock()
	head := upload.f.heads[held.ID]
	head.Capabilities = []artifacts.Capability{artifacts.CapabilityAssets}
	upload.f.heads[held.ID] = head
	upload.f.mu.Unlock()
	out[builtin.UploadArtifactAssetTool] = upload.call(t, turnFor(t, "agent-cto"),
		builtin.UploadArtifactAssetTool, map[string]any{"artifact": held.ID.String(),
			"name": "logo.png", "content": "iVBORw0KGgo=", "encoding": "base64"})
	data := newDataRig(t, chart, req)
	data.data.verdict = artifacts.DataApplied
	sheet := data.art.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	data.data.collection("review", artifacts.ReadAll)
	dataTurn, _ := readerTurn(t)
	out[builtin.WriteArtifactDataTool] = data.call(t, dataTurn, builtin.WriteArtifactDataTool,
		map[string]any{"artifact": sheet.ID.String(), "ops": []any{map[string]any{"op": "set",
			"collection": "review", "doc": "row-1", "fields": map[string]any{"ok": true}}}})
	out[builtin.SetArtifactSchemaTool] = data.call(t, dataTurn, builtin.SetArtifactSchemaTool,
		map[string]any{"artifact": sheet.ID.String(), "collections": []any{map[string]any{
			"name": "review", "preset": "contribute", "columns": []any{
				map[string]any{"key": "status", "type": "select"}}}}})
	// A VERDICT THIS NODE HAS NOT APPLIED YET: the fake's zero verdict.
	pending := newDataRig(t, chart, req)
	pending.data.verdict = ""
	pendingSheet := pending.art.seed("agent-cto", iam.ActorAgent, cto, nil,
		[2]string{"index.html", "<p>x</p>"})
	pending.data.collection("review", artifacts.ReadAll)
	out["write_artifact_data, its verdict pending"] = pending.call(t, dataTurn,
		builtin.WriteArtifactDataTool, map[string]any{"artifact": pendingSheet.ID.String(),
			"ops": []any{map[string]any{"op": "set", "collection": "review", "doc": "row-1",
				"fields": map[string]any{"ok": true}}}})
	return out
}

// EVERY ANSWER A PERSON CAN BE GIVEN IS WORDED FOR A PERSON (ADR-0060), every
// write the operator catalogue serves and every partial outcome, not a sample.
//
// A tool's answer is written for a model, and a person's surface answers it
// without its model-only keys and with its caveats in a person's words. A
// partial outcome (the item landed, its dependencies did not; a move or a
// removal whose walk stopped) used to ride the receipt as the model's
// sentence under a key of its own, so a person was told "Created" while a
// dependency write had failed, and the next screen reading that key would
// have shown them a model's instructions. This walks every string of every
// such answer outside the value fields a screen reads as values, so a new key
// of prose on any tool fails here whatever it is called. A write the catalogue
// gains without an answer here fails the coverage half.
func TestEveryAnswerAPersonCanBeGivenIsWordedForAPerson(t *testing.T) {
	t.Parallel()
	answers := everyLandedAnswer(t)
	for name, res := range answers {
		for _, problem := range caveattest.AnswerProblems(name, personAnswer(t, name, res)) {
			t.Errorf("%s: %s", name, problem)
		}
	}

	// EVERY WRITE IS HERE.
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		if mcp.ReadOnlyProven(builtin.AnnotationsFor(tool.Name())) {
			continue
		}
		if _, covered := answers[tool.Name()]; !covered {
			t.Errorf("%s is a write a person's surface can reach, and no answer of it "+
				"is walked here: add one", tool.Name())
		}
	}

	// AND EVERY PARTIAL OUTCOME SAYS SO IN A PERSON'S WORDS, beside the
	// model's sentence the model still reads.
	for name, want := range map[string]struct {
		key  string
		code caveat.Code
	}{
		"create_work_item, its dependencies refused": {builtin.DependenciesFailedKey, caveat.CodeDependenciesNotRecorded},
		"update_work_item, its dependencies refused": {builtin.DependenciesFailedKey, caveat.CodeDependenciesNotRecorded},
		"move_work_item, its walk stopped ":          {builtin.MoveStoppedKey, caveat.CodeMoveUnfinished},
		"move_work_item, its walk stopped ENG-4":     {builtin.MoveStoppedKey, caveat.CodeMoveWaiting},
		"remove_work_item, its walk stopped":         {builtin.SubtreeStoppedKey, caveat.CodeSubtreeUnfinished},
		"restore_work_item, its walk stopped":        {builtin.SubtreeStoppedKey, caveat.CodeSubtreeUnfinished},
		"write_artifact_data, its verdict pending":   {builtin.NoteKey, caveat.CodeDatastorePending},
	} {
		res := answers[name]
		var model map[string]any
		if err := json.Unmarshal([]byte(res.Output), &model); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if said, _ := model[want.key].(string); said == "" {
			t.Errorf("%s no longer tells the model under %s: %v", name, want.key,
				slices.Sorted(maps.Keys(model)))
		}
		if !slices.ContainsFunc(res.Caveats, func(c caveat.Caveat) bool { return c.Code() == want.code }) {
			t.Errorf("%s answered caveats %v, want %s", name, res.Caveats, want.code)
		}
		if warnings, _ := model[builtin.WarningsKey].([]any); slices.ContainsFunc(warnings,
			func(w any) bool { return w == model[want.key] }) {
			t.Errorf("%s tells the model the same sentence twice", name)
		}
	}
}
