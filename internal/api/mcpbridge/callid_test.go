package mcpbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// How a call carries the id its harness gave it (the package doc's "A call
// carries the id its harness gave it").

// toolUseKey is the `_meta` key Claude Code hands each call's own id under, as
// the claude-code profile reads it: the key a runner of a box of Claude Code's
// declares (internal/sandbox/executor's Runner.CallIDMeta).
const toolUseKey = "claudecode/toolUseId"

// withCallIDKey readies a session to read each call's id under key, as the
// runner that starts its box declares it.
func withCallIDKey(key string) func(*mcpbridge.Session) {
	return func(s *mcpbridge.Session) { s.CallIDMeta = key }
}

// callWithMeta makes one call of name from sess, meta its request's `_meta`,
// and answers its result: a failed one's too, which is an answer.
func callWithMeta(t *testing.T, sess *mcp.ClientSession, name string, args any,
	meta mcp.Meta) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args, Meta: meta})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

// carriesID fails t unless l holds one record of a call, under id, and — where
// entry is set — the one entry written ahead of it, under id too, or no entry
// where it is not; and answers the records.
func carriesID(t *testing.T, l *ledger, id string, entry bool) []mcpbridge.Call {
	t.Helper()
	rows := l.rows()
	if len(rows) != 1 || rows[0].CallID != id {
		t.Errorf("the call is recorded %+v, want one record under the id its harness gave it, %q", rows, id)
	}
	switch entries := l.entries(); {
	case entry && (len(entries) != 1 || entries[0].CallID != id):
		t.Errorf("the call's entry is %+v, want one, under the id its harness gave it, %q", entries, id)
	case !entry && len(entries) != 0:
		t.Errorf("a call answered without being admitted wrote the entries %+v, want none", entries)
	}
	return rows
}

// EVERY RECORD OF A BOX'S CALL CARRIES THE ID ITS HARNESS GAVE IT. Claude Code
// names each call it makes by the model's tool_use id, which it hands the
// server in the request's `_meta` and its session's transcript files the call
// under. The bridge records that id, read under the key the box's runner
// declares, on the call's entry written ahead of it and on its one update, so
// the run's log names each call as its harness's session does — ON EVERY PATH
// THAT RECORDS A CALL: one that ran, one the surface refused before its tool
// ran, one the surface could not run, one held for a person, one whose
// arguments did not parse and one made once the run's tools had closed (the
// last two answered without being admitted, so with no entry). A held call
// keeps its id beside neither its arguments nor their digest: the id says
// nothing of what the seal hides. And a skill load its harness's hook reports,
// the bridge's own call of the loader, carries none, whatever id the hook's
// request sent.
//
// Mutation: record a finished call without the id it arrived with
// (bridge-record-carries-no-id), and the update of a call that ran, of one the
// surface refused and of a held one carries none.
func TestEveryRecordOfABoxsCallCarriesTheIDItsHarnessGaveIt(t *testing.T) {
	t.Parallel()
	const id = "toolu_01carriesitsid"
	for _, tc := range []struct {
		name string
		// ready readies the fixture before its session opens.
		ready  func(f *fixture)
		args   any
		failed bool
		// entry is whether the call is written ahead of its run: a call
		// answered without being admitted writes none.
		entry bool
	}{
		{name: "a call that ran", args: map[string]any{"text": "hello"}, entry: true},
		{name: "a call the surface refused before its tool ran", ready: func(f *fixture) {
			f.session.Surface = f.surface.WithGuard(denyGuard{tool: "post_message",
				reason: "load the posting skill first"})
		}, args: map[string]any{"text": "hello"}, failed: true, entry: true},
		{name: "a call the surface could not run", ready: func(f *fixture) {
			f.byName["post_message"].err = fmt.Errorf("toolloop: %w", context.Canceled)
		}, args: map[string]any{"text": "hello"}, failed: true, entry: true},
		{name: "a call whose arguments did not parse", args: json.RawMessage(`["not","an object"]`),
			failed: true},
		{name: "a call made once the run's tools had closed", ready: func(f *fixture) {
			f.session.Fence = func() error { return errors.New("this node no longer holds the seat") }
		}, args: map[string]any{"text": "hello"}, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.session.CallIDMeta = toolUseKey
			if tc.ready != nil {
				tc.ready(f)
			}
			res := callWithMeta(t, dial(t, f.open(t)), "post_message", tc.args, mcp.Meta{toolUseKey: id})
			if res.IsError != tc.failed {
				t.Fatalf("the call was answered %q, failed %v, want failed %v", text(res), res.IsError, tc.failed)
			}
			carriesID(t, f.ledger, id, tc.entry)
		})
	}
	t.Run("a call held for a person", func(t *testing.T) {
		t.Parallel()
		reg := tools.NewRegistry()
		if err := reg.RegisterWith(&stubTool{name: "close_issue"}, tools.Origin("tracker"), tools.Annotations{},
			func(e *tools.Entry) { e.NeedsApproval = true }); err != nil {
			t.Fatal(err)
		}
		f := openServers(t, tools.NewSurface("execute", reg.Snapshot(), []string{"close_issue"}).
			ForTurn(seatTurn()).WithHolder(holdingHolder{}), withCallIDKey(toolUseKey))
		callWithMeta(t, dial(t, mcpbridge.ServerURL(f.endpoint, "tracker")), "close_issue",
			map[string]any{"id": 7}, mcp.Meta{toolUseKey: id})
		if rows := carriesID(t, f.ledger, id, true); len(rows) == 1 &&
			(!rows[0].Held || rows[0].Args != nil || rows[0].Digest != "") {
			t.Errorf("the held call is recorded as held %v with arguments %v and identity %q, want it held "+
				"with neither, beside its id", rows[0].Held, rows[0].Args, rows[0].Digest)
		}
	})
	t.Run("a skill load its harness's hook reported", func(t *testing.T) {
		t.Parallel()
		f := newSkillsFixture(t, handed, withCallIDKey(toolUseKey))
		callWithMeta(t, dial(t, f.endpoint), mcpbridge.SkillTool, map[string]any{"skill": "crewlet:mcp-github"},
			mcp.Meta{toolUseKey: id})
		if rows := f.ledger.rows(); len(rows) != 1 || rows[0].Name != toolname.LoadToolSkill || rows[0].CallID != "" {
			t.Errorf("the reported load is recorded %+v, want one load of the skill with no id: the bridge's own "+
				"call carries none of a harness's", rows)
		}
	})
}

// A CALL'S ID IS READ ONLY UNDER ITS RUNNER'S KEY, AND ONLY WHOLE. The box's
// harness hands a call's id in the request's `_meta` under a key of its own,
// which the runner that starts the box declares, and the box can put anything
// anywhere in `_meta`: so the bridge reads the id under that one key and no
// other — a session whose runner declares none reads none, even a value under
// the empty key — and takes only an id the run's log keeps whole: at most
// sandbox.MaxBridgeCallIDBytes of letters, digits, `-` and `_`. A value that is
// not a string, an id past the bound and one outside those characters are
// recorded as none, on the call's entry and its update alike, never cut: two
// ids cut alike would name two calls as one.
//
// Mutation: read the id under a fixed key, whatever it is
// (bridge-takes-any-id), and a session that declares another key, or none,
// records Claude Code's, and an id past the bound is recorded whole.
func TestACallIDIsReadOnlyUnderItsRunnersKeyAndOnlyWhole(t *testing.T) {
	t.Parallel()
	whole := strings.Repeat("a", sandbox.MaxBridgeCallIDBytes)
	for _, tc := range []struct {
		name, key string
		meta      map[string]any
		want      string
	}{
		{"an id under the runner's key", toolUseKey, map[string]any{toolUseKey: "toolu_01whole"}, "toolu_01whole"},
		{"an id at the bound", toolUseKey, map[string]any{toolUseKey: whole}, whole},
		{"an id under another runner's key, beside Claude Code's", "box/callId",
			map[string]any{"box/callId": "call_7", toolUseKey: "toolu_01other"}, "call_7"},
		{"a session whose runner declares no key", "",
			map[string]any{toolUseKey: "toolu_01x", "": "toolu_01y"}, ""},
		{"an id under a key the runner does not declare", toolUseKey, map[string]any{"callId": "toolu_01x"}, ""},
		{"an id past the bound", toolUseKey, map[string]any{toolUseKey: whole + "b"}, ""},
		{"an id with a slash", toolUseKey, map[string]any{toolUseKey: "toolu_a/b"}, ""},
		{"an id with a space", toolUseKey, map[string]any{toolUseKey: "toolu a"}, ""},
		{"an empty id", toolUseKey, map[string]any{toolUseKey: ""}, ""},
		{"a number", toolUseKey, map[string]any{toolUseKey: 42}, ""},
		{"an object", toolUseKey, map[string]any{toolUseKey: map[string]any{"id": "toolu_01x"}}, ""},
		{"no _meta at all", toolUseKey, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.session.CallIDMeta = tc.key
			f.open(t)
			res := mcpbridge.Dispatcher(f.session).Call(toolsession.Call{Name: "read_page",
				Arguments: map[string]any{"text": "a"}, Meta: tc.meta})
			if res.Failed {
				t.Fatalf("the call failed: %q", res.Output)
			}
			entries, rows := f.ledger.entries(), f.ledger.rows()
			if len(entries) != 1 || len(rows) != 1 || entries[0].CallID != tc.want || rows[0].CallID != tc.want {
				t.Errorf("the call's entry is %+v and its record %+v, want each under the id %q", entries, rows,
					tc.want)
			}
		})
	}
}
