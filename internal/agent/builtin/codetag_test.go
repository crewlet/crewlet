package builtin_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/tools"
)

// A TAG WRITE ASKS THE TAG IT NAMES: code_create_tag sends refs/tags/<tag> at
// the commit `at` resolves, its message making it annotated; code_delete_tag
// sends the value its caller read. Neither moves a branch.
//
// Mutations: send refs/heads/<tag>, red; drop the message, red; send no
// expected value with the delete, red.
func TestATagWriteAsksTheTagItNames(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(newGitletHost(), writer))
	turn := writerTurn(t)
	for _, args := range []map[string]any{
		{"repo": "site", "tag": "v1.0.0", "at": "main", "message": "The first release"},
		{"repo": "site", "tag": "refs/tags/nightly", "at": mainOID},
	} {
		if got := callWrite(everyGrant(), t, reg, turn, builtin.CodeCreateTagTool, args); got.Failed {
			t.Fatalf("code_create_tag %v answered %q", args, got.Output)
		}
	}
	got := callWrite(everyGrant(), t, reg, turn, builtin.CodeDeleteTagTool,
		map[string]any{"repo": "site", "tag": "v1.0.0", "expected_target": featureOID})
	if got.Failed {
		t.Fatalf("code_delete_tag answered %q", got.Output)
	}
	var answer struct {
		Tag      string `json:"tag"`
		Previous string `json:"previous"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil || answer.Tag != "v1.0.0" ||
		answer.Previous != featureOID {
		t.Errorf("the delete answered %q (%v), want tag v1.0.0 and its previous value", got.Output, err)
	}
	writes, _ := writer.asked()
	want := []gitlet.EngineWrite{
		{Kind: gitlet.WriteCreateTag, Ref: "refs/tags/v1.0.0", Commit: mainOID, Message: "The first release"},
		{Kind: gitlet.WriteCreateTag, Ref: "refs/tags/nightly", Commit: mainOID},
		{Kind: gitlet.WriteDeleteTag, Ref: "refs/tags/v1.0.0", ExpectedHead: featureOID},
	}
	if len(writes) != len(want) {
		t.Fatalf("%d writes were sent, want %d", len(writes), len(want))
	}
	for i, w := range writes {
		if w.Kind != want[i].Kind || w.Ref != want[i].Ref || w.Commit != want[i].Commit ||
			w.Message != want[i].Message || w.ExpectedHead != want[i].ExpectedHead {
			t.Errorf("write %d is %s %s commit %q message %q expected %q, want %+v", i, w.Kind, w.Ref, w.Commit,
				w.Message, w.ExpectedHead, want[i])
		}
		if err := w.Validate(); err != nil {
			t.Errorf("write %d is not a write any party takes: %v", i, err)
		}
	}
}

// A TAG NAME GIT REFUSES IS REFUSED BEFORE ANYTHING IS SENT, and a tag that
// already exists is answered stale with the way out: a tag never moves.
//
// Mutation: skip ValidTag in the tool's arguments: the write is sent, red.
func TestATagWriteIsRefusedWhereGitWouldRefuseIt(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{}
	reg := codeRegistry(t, writeDeps(newGitletHost(), writer))
	for _, tag := range []string{"HEAD", "-v1", "v1..2", "v 1", ""} {
		got := callWrite(everyGrant(), t, reg, writerTurn(t), builtin.CodeCreateTagTool,
			map[string]any{"repo": "site", "tag": tag, "at": mainOID})
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid {
			t.Errorf("tag %q answered %q, want an invalid refusal", tag, got.Output)
		}
	}
	if writes, _ := writer.asked(); len(writes) != 0 {
		t.Fatalf("%d writes were sent for names git refuses", len(writes))
	}
	stale := &codeWrites{write: func(w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
		return gitlet.EngineWriteResult{Command: gitlet.Command{Ref: w.Ref, Old: gitlet.ZeroOID, New: w.Commit,
			Result: gitlet.ResultNG, Reason: gitlet.RefusalStale, Detail: "refs/tags/v1 already exists"}}, nil
	}}
	reg = codeRegistry(t, writeDeps(newGitletHost(), stale))
	got := callWrite(everyGrant(), t, reg, writerTurn(t), builtin.CodeCreateTagTool,
		map[string]any{"repo": "site", "tag": "v1", "at": mainOID})
	if tools.RefusalOf(got) != tools.RefusalStaleVersion || !strings.Contains(got.Output, "A tag never moves") {
		t.Errorf("a tag that exists answered %q (%s), want stale naming that a tag never moves", got.Output,
			tools.RefusalOf(got))
	}
}
