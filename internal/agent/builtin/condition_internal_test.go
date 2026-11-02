package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// EVERY TOOL'S SENTENCE ABOUT A CONDITION NO WAIT CLEARS tells a model that
// trying again won't help and what to say instead, and only a condition that
// clears is offered the retry.
//
// A model neither chooses the node a call goes to nor has an administrator to
// ask, so the person's advice ("try another node", "an administrator needs
// to make room") is nothing it can act on, and a read that dropped the tool's
// own "say you could not check" for it lost the one instruction a model can
// follow. And "could not read X right now" promises a wait the advice then
// rules out. Driven through every failure helper a tool answers a condition
// through — the tracker's, the knowledge base's, the chat's (a direct
// message's included), the artifacts', the datastore's and the node's own —
// with a full log, and with a node behind as the control that clears.
//
// Mutation: answer any helper's stuck condition with its `soon` (or with the
// person's advice), put "right now" back in a read's sentence, or drop the
// next step from a direct message's sentence, and that helper's row goes red.
func TestEveryToolTellsAModelWhatAConditionNoWaitClearsLeavesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const detail = "the log is at 64 MiB of stream.tracker_max_bytes"
	readFull := fmt.Errorf("read: %w", &statelog.Refused{Code: statelog.RefuseLogFull,
		Level: statelog.ReadLinearizable, Detail: detail})
	readBehind := fmt.Errorf("read: %w", &statelog.Refused{Code: statelog.RefuseBehind,
		Level: statelog.ReadLinearizable, Detail: detail, RetryAfter: time.Second})
	writeFull := fmt.Errorf("write: %w", &statelog.Unavailable{
		Reason: statelog.ReasonLogFull, Detail: detail})
	writeBehind := fmt.Errorf("write: %w", &statelog.Unavailable{
		Reason: statelog.ReasonBehind, Detail: detail})

	type helper struct {
		name   string
		answer func(error) tools.Result
		stuck  string
		read   bool
		// retries is whether the helper offers a retry where waiting clears.
		retries bool
	}
	for _, h := range []helper{
		{"tracker read", func(err error) tools.Result {
			return readFailure(ctx, GetWorkItemTool, err)
		}, stuckRead, true, true},
		{"tracker write", func(err error) tools.Result {
			return writeFailure(ctx, Actor{}, UpdateWorkItemTool, err)
		}, stuckWrite, false, false},
		{"knowledge base read", func(err error) tools.Result {
			return pageReadFailure(ctx, GetPageTool, err)
		}, stuckRead, true, true},
		{"knowledge base write", func(err error) tools.Result {
			return pageWriteFailure(ctx, WritePageTool, err, "")
		}, stuckWrite, false, false},
		{"chat read", func(err error) tools.Result {
			return chatReadFailure(ctx, ReadChannelTool, err)
		}, stuckRead, true, true},
		{"chat write", func(err error) tools.Result {
			return chatWriteFailure(ctx, PostMessageTool, err, "")
		}, stuckWrite, false, false},
		{"direct message", func(err error) tools.Result {
			return directPostFailure(ctx, []string{"jane"}, chatlet.Written{}, err, "")
		}, stuckWrite, false, false},
		{"artifacts read", func(err error) tools.Result {
			return artifactReadFailure(ctx, GetArtifactTool, "launch-plan", err)
		}, stuckRead, true, true},
		{"artifact folders read", func(err error) tools.Result {
			return folderReadFailure(ctx, CreateArtifactFolderTool, err)
		}, stuckRead, true, true},
		{"company brand read", func(err error) tools.Result {
			return brandReadFailure(ctx, err)
		}, stuckRead, true, true},
		{"artifacts write", func(err error) tools.Result {
			return publishFailure(ctx, PublishArtifactTool, artifactCaller{}, err)
		}, stuckWrite, false, true},
		{"datastore", func(err error) tools.Result {
			return dataFailure(ctx, WriteArtifactDataTool, err)
		}, stuckWrite, false, true},
		{"the node's own", func(err error) tools.Result {
			return nodeFailure(ctx, "remember", err, "Could not save your note",
				"Nothing was changed.")
		}, stuckWrite, false, true},
	} {
		full, behind := writeFull, writeBehind
		if h.read {
			full, behind = readFull, readBehind
		}
		got := h.answer(full)
		if tools.RefusalOf(got) != tools.RefusalUnavailable {
			t.Errorf("%s: a full log is %q, want %q", h.name, tools.RefusalOf(got),
				tools.RefusalUnavailable)
		}
		if !strings.Contains(got.Output, h.stuck) {
			t.Errorf("%s: a full log is told %q, want %q in it", h.name, got.Output, h.stuck)
		}
		for _, wrong := range []string{"Try again", "another node", "administrator",
			"right now", detail} {
			if strings.Contains(got.Output, wrong) {
				t.Errorf("%s: a full log is told %q, with %q in it", h.name,
					got.Output, wrong)
			}
		}
		got = h.answer(behind)
		if strings.Contains(got.Output, "won't clear") ||
			h.retries != strings.Contains(got.Output, "Try again") {
			t.Errorf("%s: a node behind is told %q; want the retry: %v", h.name,
				got.Output, h.retries)
		}
	}
}
