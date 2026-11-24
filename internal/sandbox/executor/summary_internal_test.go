package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// summaryBox is a box holding a session for a harness's main-thread read.
type summaryBox struct {
	h                mainThreadHarness
	box              *sandbox.FakeSandbox
	session, release string
}

// read is the box's read asking the newest summary alone.
func (b summaryBox) read(t *testing.T) sandbox.ThreadSummary {
	t.Helper()
	return b.h.readMainThread(t.Context(), b.box, PathsFor(b.box), sandbox.MainThreadAsk{Session: b.session,
		Release: b.release, Summary: true}).Summary
}

// tailHead is the command a Claude Code summary's line is read by.
var tailHead = regexp.MustCompile(`^tail -c \+(\d+) '([^']*)' \| head -c (\d+)$`)

// answering makes box answer a command holding a key of answers with its
// value, and a `tail -c +N file | head -c M` with those bytes of its file, as
// a box's shell does; any other command fails.
func answering(box *sandbox.FakeSandbox, answers map[string]string) {
	box.ExecFunc = func(ctx context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if m := tailHead.FindStringSubmatch(cmd); m != nil {
			raw, err := box.ReadFile(ctx, m[2])
			if err != nil {
				return sandbox.ExecResult{}, err
			}
			from, _ := strconv.Atoi(m[1])
			n, _ := strconv.Atoi(m[3])
			from = min(from-1, len(raw))
			return sandbox.ExecResult{Stdout: string(raw[from:min(from+n, len(raw))])}, nil
		}
		for key, answer := range answers {
			if strings.Contains(cmd, key) {
				return sandbox.ExecResult{Stdout: answer}, nil
			}
		}
		return sandbox.ExecResult{ExitCode: 1, Stderr: "no such command"}, nil
	}
}

// fixtureLines are raw's lines, each with its break.
func fixtureLines(raw []byte) [][]byte {
	lines := bytes.SplitAfter(raw, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineWith is the last of lines holding marker, and its place; t fails where
// none does.
func lineWith(t *testing.T, lines [][]byte, marker string) (int, []byte) {
	t.Helper()
	for i := len(lines) - 1; i >= 0; i-- {
		if bytes.Contains(lines[i], []byte(marker)) {
			return i, lines[i]
		}
	}
	t.Fatalf("no line holds %q", marker)
	return 0, nil
}

// objectOf is line decoded as an object.
func objectOf(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// lineOf is v as one line, its break included.
func lineOf(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

// valueAt is the value m holds at path, each step a key or an index.
func valueAt(m any, path ...any) any {
	for _, step := range path {
		switch s := step.(type) {
		case string:
			m = m.(map[string]any)[s]
		case int:
			m = m.([]any)[s]
		}
	}
	return m
}

// edited is line with edit applied to it as an object.
func edited(t *testing.T, line []byte, edit func(m map[string]any)) []byte {
	t.Helper()
	m := objectOf(t, line)
	edit(m)
	return lineOf(t, m)
}

func readTestdata(t *testing.T, parts ...string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// THE NEWEST SUMMARY IS THE MAIN THREAD'S LAST, by each harness's own rule,
// read off the recorded sessions (testdata/<harness>/<release>): the last
// compaction's on the main thread, never a subagent's later one, never a
// snapshot Gemini CLI patched away, never a compaction off Pi's active branch,
// only Hermes's active summary row; none on a thread that never compacted, and
// unread on a release the summary was not read on. A case marked "built from
// recorded lines" adds a compaction copied from the fixture's own.
//
// Mutation: pick each harness's first summary instead of its last, and the
// cases of two compactions fail.
func TestTheNewestSummaryIsTheMainThreadsLast(t *testing.T) {
	t.Parallel()
	same := func(lines [][]byte) [][]byte { return lines }
	for _, tc := range []struct {
		name   string
		box    func(t *testing.T) summaryBox
		want   func(t *testing.T) string
		unread string
	}{
		{name: "claude code, one compaction",
			box: func(t *testing.T) summaryBox { return claudeSummaryBox(t, same) },
			want: func(t *testing.T) string {
				_, l := lineWith(t, claudeSummaryFixture(t), `"isCompactSummary":true`)
				return valueAt(objectOf(t, l), "message", "content").(string)
			}},
		{name: "claude code, two compactions (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return claudeSummaryBox(t, func(lines [][]byte) [][]byte {
					_, boundary := lineWith(t, lines, `"compact_boundary"`)
					_, summary := lineWith(t, lines, `"isCompactSummary":true`)
					return append(lines, boundary, edited(t, summary, func(m map[string]any) {
						m["message"].(map[string]any)["content"] = "NEWEST: the second compaction's summary"
					}))
				})
			},
			want: func(*testing.T) string { return "NEWEST: the second compaction's summary" }},
		{name: "claude code, another release",
			box: func(t *testing.T) summaryBox {
				b := claudeSummaryBox(t, same)
				b.release = "2.1.330"
				return b
			},
			unread: `the job ran release "2.1.330", and this build reads the summary of release 2.1.310 and 2.1.322 alone`},
		{name: "codex, two compactions (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return codexSummaryBox(t, func(lines [][]byte) [][]byte {
					i, compacted := lineWith(t, lines, `"type":"compacted"`)
					older := edited(t, compacted, func(m map[string]any) {
						m["payload"].(map[string]any)["message"] = "OLDER: the first compaction's summary"
					})
					return append(append(append([][]byte{}, lines[:i]...), older), lines[i:]...)
				})
			},
			want: func(t *testing.T) string {
				_, l := lineWith(t, fixtureLines(codexFixture(t, "compaction-note-after.jsonl")), `"type":"compacted"`)
				return valueAt(objectOf(t, l), "payload", "message").(string)
			}},
		{name: "codex, no compaction",
			box: func(t *testing.T) summaryBox {
				b := codexSummaryBox(t, same)
				b.box.Put(PathsFor(b.box).SessionCapture(), string(codexArchive(t, codexFixture(t, "rollout.jsonl"))))
				return b
			},
			want: func(*testing.T) string { return "" }},
		{name: "codex, another release",
			box: func(t *testing.T) summaryBox {
				b := codexSummaryBox(t, same)
				b.release = "0.170.0"
				return b
			},
			unread: "this build reads the summary of release 0.171.0 alone"},
		{name: "gemini cli, the first snapshot patched away",
			box: func(t *testing.T) summaryBox {
				return geminiSummaryBox(t, func(lines [][]byte) [][]byte { return geminiSnapshotsNamed(t, lines) })
			},
			want: func(*testing.T) string { return "<state_snapshot> NEWEST snapshot" }},
		{name: "gemini cli, the newest snapshot patched away (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return geminiSummaryBox(t, func(lines [][]byte) [][]byte {
					lines = geminiSnapshotsNamed(t, lines)
					i, _ := lineWith(t, lines, `"removeIds":["c0cf4893`)
					_, newest := lineWith(t, lines, "NEWEST snapshot")
					id := objectOf(t, newest)["id"]
					return append(append([][]byte{}, lines[:i]...), lineOf(t, map[string]any{
						"$patch": map[string]any{"removeIds": []any{id}}}))
				})
			},
			want: func(*testing.T) string { return "<state_snapshot> OLDER snapshot" }},
		{name: "qwen code, two compactions (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return qwenSummaryBox(t, func(lines [][]byte) [][]byte {
					_, compression := lineWith(t, lines, `"chat_compression"`)
					return append(lines, []byte(strings.ReplaceAll(string(compression), "SUMMARY-BY-STUB",
						"NEWEST-BY-STUB")))
				})
			},
			want: func(t *testing.T) string {
				_, l := lineWith(t, fixtureLines([]byte(gunzipped(t, qwenSummaryFixture))), `"chat_compression"`)
				text := valueAt(objectOf(t, l), "systemPayload", "compressedHistory", 0, "parts", 0, "text").(string)
				return strings.ReplaceAll(strings.TrimSpace(text), "SUMMARY-BY-STUB", "NEWEST-BY-STUB")
			}},
		{name: "opencode, two compactions (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return openCodeSummaryBox(t, func(doc map[string]any) {
					messages := doc["messages"].([]any)
					for _, m := range messages {
						if m.(map[string]any)["type"] == "compaction" {
							newest := map[string]any{}
							for k, v := range m.(map[string]any) {
								newest[k] = v
							}
							newest["id"], newest["summary"] = "msg_newest", "NEWEST: the second compaction's summary"
							doc["messages"] = append(messages, newest)
							return
						}
					}
					t.Fatal("the export holds no compaction")
				})
			},
			want: func(*testing.T) string { return "NEWEST: the second compaction's summary" }},
		{name: "grok build, two compactions (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return grokSummaryBox(t, func(lines [][]byte) [][]byte {
					_, summary := lineWith(t, lines, "This session is being continued")
					return append(lines, edited(t, summary, func(m map[string]any) {
						m["content"].([]any)[0].(map[string]any)["text"] = "NEWEST: the second compaction's summary"
					}))
				})
			},
			want: func(*testing.T) string { return "NEWEST: the second compaction's summary" }},
		{name: "grok build, one compaction beside its context",
			box: func(t *testing.T) summaryBox { return grokSummaryBox(t, same) },
			want: func(t *testing.T) string {
				_, l := lineWith(t, fixtureLines(grokFixture(t, "compacted.chat.jsonl")), "This session is being continued")
				return valueAt(objectOf(t, l), "content", 0, "text").(string)
			}},
		{name: "muse code, three compactions",
			box: func(t *testing.T) summaryBox { return museSummaryBox(t, same) },
			want: func(t *testing.T) string {
				_, l := lineWith(t, fixtureLines(museFixture(t, "compacted.session.jsonl")), "context_compaction_installed")
				return valueAt(objectOf(t, l), "payload", "event", "replacement", "text").(string)
			}},
		{name: "copilot, a subagent's later compaction",
			box: func(t *testing.T) summaryBox {
				return copilotSummaryBox(t, func(lines [][]byte) [][]byte {
					_, summary := lineWith(t, lines, "session.compaction_complete")
					return append(lines, edited(t, summary, func(m map[string]any) {
						m["agentId"] = "a-subagent"
						m["data"].(map[string]any)["summaryContent"] = "SUBAGENT: never the main thread's"
					}))
				})
			},
			want: func(t *testing.T) string {
				_, l := lineWith(t, fixtureLines(copilotFixture(t, "compacted-after-tools.events.jsonl")),
					"session.compaction_complete")
				return valueAt(objectOf(t, l), "data", "summaryContent").(string)
			}},
		{name: "copilot, two compactions (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return copilotSummaryBox(t, func(lines [][]byte) [][]byte {
					_, summary := lineWith(t, lines, "session.compaction_complete")
					return append(lines, edited(t, summary, func(m map[string]any) {
						m["data"].(map[string]any)["summaryContent"] = "NEWEST: the second compaction's summary"
					}))
				})
			},
			want: func(*testing.T) string { return "NEWEST: the second compaction's summary" }},
		{name: "kimi code, two compactions (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return kimiSummaryBox(t, "compacted.wire.jsonl", func(lines [][]byte) [][]byte {
					_, summary := lineWith(t, lines, "context.apply_compaction")
					return append(lines, edited(t, summary, func(m map[string]any) {
						m["summary"] = "NEWEST-OF-WORK"
					}))
				})
			},
			want: func(*testing.T) string { return "NEWEST-OF-WORK" }},
		{name: "kimi code, its summary and never its context summary",
			box:  func(t *testing.T) summaryBox { return kimiSummaryBox(t, "compacted.wire.jsonl", same) },
			want: func(*testing.T) string { return "SUMMARY-OF-WORK" }},
		{name: "kimi code, no compaction",
			box:  func(t *testing.T) summaryBox { return kimiSummaryBox(t, "launched.wire.jsonl", same) },
			want: func(*testing.T) string { return "" }},
		{name: "hermes agent, an active summary and a later one folded (built from recorded rows)",
			box: func(t *testing.T) summaryBox {
				return hermesSummaryBox(t, func(rows [][]byte) [][]byte {
					_, summary := lineWith(t, rows, `"_compressed_summary": 1`)
					return append(rows, edited(t, summary, func(m map[string]any) {
						r := m["r"].(map[string]any)
						r["id"], r["active"], r["content"] = 99, 0, "FOLDED: no longer in the conversation"
					}))
				})
			},
			want: func(t *testing.T) string {
				_, l := lineWith(t, fixtureLines(hermesFixture(t, "0.23.3", "session-compacted.jsonl")),
					`"_compressed_summary": 1`)
				return valueAt(objectOf(t, l), "r", "content").(string)
			}},
		{name: "hermes agent, two active summaries (built from recorded rows)",
			box: func(t *testing.T) summaryBox {
				return hermesSummaryBox(t, func(rows [][]byte) [][]byte {
					_, summary := lineWith(t, rows, `"_compressed_summary": 1`)
					return append(rows, edited(t, summary, func(m map[string]any) {
						r := m["r"].(map[string]any)
						r["id"], r["content"] = 99, "NEWEST: the second compaction's summary"
					}))
				})
			},
			want: func(*testing.T) string { return "NEWEST: the second compaction's summary" }},
		{name: "pi, a compaction off its active branch (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return piSummaryBox(t, func(lines [][]byte) [][]byte {
					_, summary := lineWith(t, lines, `"type":"compaction"`)
					leaf := objectOf(t, lines[len(lines)-1])
					off := edited(t, summary, func(m map[string]any) {
						m["id"], m["parentId"], m["summary"] = "0ff0b0a0", "1412e7e9", "OFF-BRANCH: never the newest"
					})
					back := edited(t, lines[len(lines)-1], func(m map[string]any) {
						m["id"], m["parentId"] = "1eaf1eaf", leaf["id"]
					})
					return append(lines, off, back)
				})
			},
			want: func(t *testing.T) string {
				_, l := lineWith(t, fixtureLines(piFixture(t, "compacted.session.jsonl")), `"type":"compaction"`)
				return objectOf(t, l)["summary"].(string)
			}},
		{name: "pi, two compactions on its branch (built from recorded lines)",
			box: func(t *testing.T) summaryBox {
				return piSummaryBox(t, func(lines [][]byte) [][]byte {
					_, summary := lineWith(t, lines, `"type":"compaction"`)
					leaf := objectOf(t, lines[len(lines)-1])
					return append(lines, edited(t, summary, func(m map[string]any) {
						m["id"], m["parentId"], m["summary"] = "2e2e2e2e", leaf["id"], "NEWEST: on the branch"
					}))
				})
			},
			want: func(*testing.T) string { return "NEWEST: on the branch" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.box(t).read(t)
			if tc.unread != "" {
				if !strings.Contains(got.Unread, tc.unread) || got.Text != "" {
					t.Fatalf("read %+v, want unread saying %q", got, tc.unread)
				}
				return
			}
			want := tc.want(t)
			switch {
			case got.Unread != "":
				t.Fatalf("read unread: %s", got.Unread)
			case want == "" && got != (sandbox.ThreadSummary{}):
				t.Fatalf("a thread that never compacted reads %+v, want none", got)
			case got.Text != want || got.Bytes != int64(len(want)) || got.Cut:
				t.Fatalf("read %.120q (%d bytes, cut %v), want %.120q", got.Text, got.Bytes, got.Cut, want)
			}
		})
	}
}

// claudeSummarySession is the Claude Code fixture session that compacted.
const claudeSummarySession = "5e55100a-0000-4000-8000-000000000113"

func claudeSummaryFixture(t *testing.T) [][]byte {
	t.Helper()
	return fixtureLines(readTestdata(t, "claude", "2.1.322", "holdings", "s1", "projects", "crewlet",
		claudeSummarySession+".jsonl"))
}

func claudeSummaryBox(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	box := sandbox.NewFakeSandbox("b")
	thread, _ := claudeMainThread(claudeSummarySession)
	box.Put(PathsFor(box).ClaudeConfigDir()+"/"+thread, string(bytes.Join(edit(claudeSummaryFixture(t)), nil)))
	answering(box, nil)
	return summaryBox{ClaudeCode{}, box, claudeSummarySession, "2.1.322"}
}

func codexSummaryBox(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	box := sandbox.NewFakeSandbox("b")
	rollout := bytes.Join(edit(fixtureLines(codexFixture(t, "compaction-note-after.jsonl"))), nil)
	box.Put(PathsFor(box).SessionCapture(), string(codexArchive(t, rollout)))
	return summaryBox{Codex{}, box, codexThread, codexCarryRelease}
}

// geminiSnapshotsNamed is the compressed session's lines, its two snapshots'
// words told apart: the first OLDER, the second NEWEST.
func geminiSnapshotsNamed(t *testing.T, lines [][]byte) [][]byte {
	t.Helper()
	out := make([][]byte, 0, len(lines))
	names := []string{"OLDER", "NEWEST"}
	for _, l := range lines {
		if bytes.Contains(l, []byte("<state_snapshot>")) && len(names) > 0 {
			name := names[0]
			l = edited(t, l, func(m map[string]any) {
				valueAt(m, "content", 0).(map[string]any)["text"] = "<state_snapshot> " + name + " snapshot"
			})
			names = names[1:]
		}
		out = append(out, l)
	}
	return out
}

func geminiSummaryBox(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	lines := edit(geminiFixture(t, "compressed.run2"))
	session := objectOf(t, lines[0])["sessionId"].(string)
	box := sandbox.NewFakeSandbox("b")
	name := "session-2026-11-24T00-00-" + session[:8] + ".jsonl"
	box.Put(PathsFor(box).GeminiChats()+"/"+name, string(bytes.Join(lines, nil)))
	answering(box, map[string]string{"ls -1 ": name + "\n"})
	return summaryBox{GeminiCLI{}, box, session, geminiRelease}
}

const qwenSummaryFixture = "testdata/qwen/0.28.4/holdings/compacted.chat.jsonl.gz"

func qwenSummaryBox(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	const session = "11111111-2222-4333-8444-000000000201"
	box := sandbox.NewFakeSandbox("b")
	path := PathsFor(box).QwenDir() + "/projects/-work/chats/" + session + ".jsonl"
	box.Put(path, string(bytes.Join(edit(fixtureLines([]byte(gunzipped(t, qwenSummaryFixture)))), nil)))
	answering(box, map[string]string{"/chats/": path + "\n"})
	return summaryBox{QwenCode{}, box, session, qwenRelease}
}

func openCodeSummaryBox(t *testing.T, edit func(doc map[string]any)) summaryBox {
	t.Helper()
	doc := objectOf(t, []byte(gunzipped(t, "testdata/opencode/2.3.2/holdings/compaction.json.gz")))
	edit(doc)
	box := sandbox.NewFakeSandbox("b")
	box.Put(PathsFor(box).SessionCapture(), string(gzipOf(t, string(lineOf(t, doc)))))
	return summaryBox{OpenCode{}, box, "ses_summary", openCodeRelease}
}

func grokSummaryBox(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	const session = "11111111-2222-4333-8444-000000000202"
	box, _ := grokThreadBox(session, bytes.Join(edit(fixtureLines(grokFixture(t, "compacted.chat.jsonl"))), nil))
	return summaryBox{Grok{}, box, session, grokSupported()}
}

func museSummaryBox(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	const session = "0b5e0000-0000-4000-8000-0000000000f8"
	box := sandbox.NewFakeSandbox("b")
	path := PathsFor(box).MuseSessions() + "/2026/11/22/" + session + "/session.jsonl"
	box.Put(path, string(bytes.Join(edit(fixtureLines(museFixture(t, "compacted.session.jsonl"))), nil)))
	answering(box, map[string]string{"session.jsonl": path + "\n"})
	return summaryBox{Muse{}, box, session, museSupported()}
}

func copilotSummaryBox(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	const session = "11111111-2222-4333-8444-000000000203"
	box := sandbox.NewFakeSandbox("b")
	box.Put(PathsFor(box).CopilotSessionDir(session)+"/events.jsonl", string(bytes.Join(
		edit(fixtureLines(copilotFixture(t, "compacted-after-tools.events.jsonl"))), nil)))
	return summaryBox{Copilot{}, box, session, copilotFloor()}
}

func kimiSummaryBox(t *testing.T, fixture string, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	const session = "session_0f0e0d0c0b0a"
	box := sandbox.NewFakeSandbox("b")
	path := PathsFor(box).KimiSessions() + "/work/" + session + "/agents/main/wire.jsonl"
	box.Put(path, string(bytes.Join(edit(fixtureLines(kimiFixture(t, kimiSupported, fixture))), nil)))
	answering(box, map[string]string{"wire.jsonl": path + "\n"})
	return summaryBox{Kimi{}, box, session, kimiSupported}
}

func hermesSummaryBox(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	const session = "11111111-2222-4333-8444-000000000204"
	box := sandbox.NewFakeSandbox("b")
	box.Put(PathsFor(box).HermesRecordRead(), string(bytes.Join(
		edit(fixtureLines(hermesFixture(t, "0.23.3", "session-compacted.jsonl"))), nil)))
	answering(box, map[string]string{"hcarry dump": "crewlet-carry ok\n"})
	return summaryBox{Hermes{}, box, session, hermesFloor()}
}

func piSummaryBox(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
	t.Helper()
	const session = "77777777-7777-4777-8777-777777777777"
	box := sandbox.NewFakeSandbox("b")
	box.Put(PathsFor(box).PiSessionFile(session), string(bytes.Join(
		edit(fixtureLines(piFixture(t, "compacted.session.jsonl"))), nil)))
	return summaryBox{Pi{}, box, session, piSupported}
}

// A SUMMARY IS KEPT SCRUBBED AND REDACTED: a value the job's capture is
// scrubbed of and a credential's shape, each inside the newest summary a
// harness's read finds as written, never reach the summary its runner keeps.
//
// Mutation: keep the summary's text as read, or drop the scrub, and this fails.
func TestTheSummaryIsScrubbedAndRedacted(t *testing.T) {
	t.Parallel()
	const scrubbed, credential = "the-scrubbed-value-1234", "AKIAQWERTYUIOPASDFGH"
	b := kimiSummaryBox(t, "compacted.wire.jsonl", func(lines [][]byte) [][]byte {
		_, summary := lineWith(t, lines, "context.apply_compaction")
		return append(lines, edited(t, summary, func(m map[string]any) {
			m["summary"] = "the work so far used " + scrubbed + " and the key " + credential + " to deploy"
		}))
	})
	found := b.h.readMainThread(t.Context(), b.box, PathsFor(b.box), sandbox.MainThreadAsk{Session: b.session,
		Release: b.release, Summary: true}).Summary
	if found.Unread != "" || !strings.Contains(found.Text, scrubbed) {
		t.Fatalf("read %+v, want the newest summary as its harness wrote it", found)
	}
	got := (&Runner{}).KeepSummary(found, []string{scrubbed})
	switch {
	case got.Unread != "" || !strings.Contains(got.Text, " to deploy"):
		t.Fatalf("kept %+v, want the newest summary", got)
	case strings.Contains(got.Text, scrubbed):
		t.Errorf("the summary keeps the value its capture is scrubbed of: %q", got.Text)
	case strings.Contains(got.Text, credential):
		t.Errorf("the summary keeps a credential's shape: %q", got.Text)
	case got.Bytes != int64(len(got.Text)):
		t.Errorf("the summary is %d bytes, and its kept words %d", got.Bytes, len(got.Text))
	}
}

// A SUMMARY PAST ITS BOUND KEEPS ITS END: its last part, within
// executorkeep.SummaryMaxBytes, marked at its start, Cut, Bytes the whole
// summary's size as kept, the cut never splitting a character or a marker the
// redaction wrote.
//
// Mutation: keep the summary's first part, or cut inside a rune or a marker,
// and a case fails.
func TestASummaryPastItsBoundKeepsItsEnd(t *testing.T) {
	t.Parallel()
	const credential = "AKIAQWERTYUIOPASDFGH"
	marker := redact.Secrets(credential)
	if marker == credential || !strings.HasPrefix(marker, redact.Marker) {
		t.Fatalf("the credential redacts to %q, want a marker", marker)
	}
	// keep is what the cut keeps of the whole, its mark aside: each tail puts
	// the cut one byte into its first character, or halfway into the marker
	// before it, so the cut moves on to the next character or past the marker.
	keep := executorkeep.SummaryMaxBytes - len("…")
	end := " what to do next"
	if (keep+1-len(end))%2 != 0 {
		end = " " + end
	}
	words := func(n int) string { return strings.Repeat("b ", n/2) + strings.Repeat("b", n%2) }
	head := "the opening " + words(8192)
	for _, tc := range []struct {
		name, text, kept string
	}{
		{name: "a character split", text: head + strings.Repeat("é", (keep+1-len(end))/2) + end,
			kept: strings.Repeat("é", (keep+1-len(end))/2-1) + end},
		{name: "a marker split", text: head + " " + credential + " " + words(keep-len(marker)/2-1-len(end)) + end,
			kept: " " + words(keep-len(marker)/2-1-len(end)) + end},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			whole := redact.Secrets(tc.text)
			got := keptSummary(tc.text, nil)
			switch {
			case !got.Cut || got.Bytes != int64(len(whole)):
				t.Fatalf("kept %d bytes of %d (cut %v), want the whole's %d, cut", len(got.Text), got.Bytes,
					got.Cut, len(whole))
			case len(got.Text) > executorkeep.SummaryMaxBytes || !utf8.ValidString(got.Text):
				t.Fatalf("kept %d bytes, valid UTF-8 %v, want at most %d and valid", len(got.Text),
					utf8.ValidString(got.Text), executorkeep.SummaryMaxBytes)
			case got.Text != "…"+tc.kept:
				t.Fatalf("kept %.60q (%d bytes), want the marked end %.60q (%d bytes)", got.Text, len(got.Text),
					"…"+tc.kept, len(tc.kept)+len("…"))
			}
		})
	}
}

// ASKING THE SUMMARY LEAVES THE COUNT AND THE RECEIPTS AS THEY WERE: a Claude
// Code or OpenCode read asking the newest summary beside the count, the next
// request and receipts answers each of those exactly as the read asking none
// of it, and a read asking the summary alone answers none of them.
//
// Mutation: let the summary's reading end the walk, or answer the count only
// where no summary was asked, and a case fails.
func TestAskingTheSummaryLeavesTheCountAsItWas(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		box  func(t *testing.T) summaryBox
		ask  sandbox.MainThreadAsk
	}{
		{name: "claude code, its count and its next request",
			box: func(t *testing.T) summaryBox { return claudeSummaryBox(t, func(l [][]byte) [][]byte { return l }) },
			ask: sandbox.MainThreadAsk{Count: true, Next: true}},
		{name: "claude code, receipts",
			box: func(t *testing.T) summaryBox { return claudeSummaryBox(t, func(l [][]byte) [][]byte { return l }) },
			ask: sandbox.MainThreadAsk{Ended: true, Takes: []sandbox.AskedTake{{Take: "0f0e0d0c-0000-4000-8000-000000000001"}}}},
		{name: "opencode, its count and its next request",
			box: func(t *testing.T) summaryBox { return openCodeSummaryBox(t, func(map[string]any) {}) },
			ask: sandbox.MainThreadAsk{Count: true, Next: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.box(t)
			read := func(ask sandbox.MainThreadAsk) sandbox.MainThreadReading {
				ask.Session, ask.Release = b.session, b.release
				return b.h.readMainThread(t.Context(), b.box, PathsFor(b.box), ask)
			}
			without := read(tc.ask)
			asked := tc.ask
			asked.Summary = true
			with := read(asked)
			if with.Summary.Text == "" {
				t.Fatalf("the read asking the summary read none: %+v", with.Summary)
			}
			with.Summary = sandbox.ThreadSummary{}
			if !reflect.DeepEqual(with, without) {
				t.Errorf("asking the summary answers %+v, and asking none %+v", with, without)
			}
			alone := read(sandbox.MainThreadAsk{Summary: true})
			if alone.Count != nil || alone.Next != (sandbox.NextRequest{}) || !reflect.DeepEqual(alone.Receipts,
				sandbox.TakeReceipts{}) {
				t.Errorf("a read asking the summary alone answers %+v", alone)
			}
		})
	}
}

// oneWalkBox is a box whose session record confirms a take and holds a
// summary: the runner, the box, the ask, the record's path and the take held.
type oneWalkBox struct {
	runner *Runner
	box    *sandbox.FakeSandbox
	ask    sandbox.MainThreadAsk
	record string
	held   []string
}

// A READ ASKING RECEIPTS AND THE SUMMARY WALKS THE RECORD ONCE: on a large
// record that confirms a take and holds a summary, every runner that confirms
// off its record answers the take held and the summary as written, the record
// streamed out of its box once (no count asked, as at a stop or a loss).
//
// Mutation: read the summary in a walk of its own (recordSummary false), and
// the record is streamed twice.
func TestReceiptsAndTheSummaryTakeOneWalkOfTheRecord(t *testing.T) {
	t.Parallel()
	words := strings.Repeat("the work so far.", executorkeep.SummaryMaxBytes/2/16)
	for _, tc := range []struct {
		name string
		box  func(t *testing.T) oneWalkBox
	}{
		{name: "copilot", box: func(t *testing.T) oneWalkBox {
			const session = "55555555-5555-4555-8555-555555555555"
			_, compaction := lineWith(t, fixtureLines(copilotFixture(t, "compacted-after-tools.events.jsonl")),
				"session.compaction_complete")
			record := append(copilotFixture(t, "steered.events.jsonl"), edited(t, compaction, func(m map[string]any) {
				m["data"].(map[string]any)["summaryContent"] = words
			})...)
			box := sandbox.NewFakeSandbox("b")
			file := PathsFor(box).CopilotSessionDir(session) + "/events.jsonl"
			box.Put(file, string(record))
			return oneWalkBox{NewCopilot(), box, sandbox.MainThreadAsk{Session: session, Release: copilotFloor(),
				Takes: []sandbox.AskedTake{{Take: "msg_inbox1", Digest: "sha256:not-read"}}}, file,
				[]string{"msg_inbox1"}}
		}},
		{name: "kimi code", box: func(t *testing.T) oneWalkBox {
			const session = "session_6752b286-da65-4fa6-b177-7c428c2282fe"
			_, compaction := lineWith(t, fixtureLines(kimiFixture(t, kimiSupported, "compacted.wire.jsonl")),
				"context.apply_compaction")
			record := append(kimiFixture(t, kimiSupported, "steered.wire.jsonl"), edited(t, compaction,
				func(m map[string]any) { m["summary"] = words })...)
			box := sandbox.NewFakeSandbox("b")
			file := PathsFor(box).KimiSessions() + "/wd_wk_221ce7ca232a/" + session + "/agents/main/wire.jsonl"
			box.Put(file, string(record))
			answering(box, map[string]string{"wire.jsonl": file + "\n"})
			return oneWalkBox{NewKimi(), box, sandbox.MainThreadAsk{Session: session, Release: kimiSupported,
				Takes: []sandbox.AskedTake{{Take: kimiSteeredInbox}}}, file, []string{kimiSteeredInbox}}
		}},
		{name: "pi", box: func(t *testing.T) oneWalkBox {
			const session = "22222222-2222-4222-8222-222222222222"
			steered := piFixture(t, "steered.session.jsonl")
			lines := fixtureLines(steered)
			_, compaction := lineWith(t, fixtureLines(piFixture(t, "compacted.session.jsonl")), `"type":"compaction"`)
			leaf := objectOf(t, lines[len(lines)-1])["id"]
			record := append(steered, edited(t, compaction, func(m map[string]any) {
				m["id"], m["parentId"], m["summary"] = "c0c0c0c0", leaf, words
			})...)
			box := sandbox.NewFakeSandbox("b")
			file := PathsFor(box).PiSessionFile(session)
			box.Put(file, string(record))
			return oneWalkBox{NewPi(), box, sandbox.MainThreadAsk{Session: session, Release: piSupported,
				Takes: []sandbox.AskedTake{{Take: "inbox-steer-1", Digest: "sha256:not-read"}}}, file,
				[]string{"inbox-steer-1"}}
		}},
		{name: "hermes agent", box: func(t *testing.T) oneWalkBox {
			_, summary := lineWith(t, fixtureLines(hermesFixture(t, "0.23.3", "session-compacted.jsonl")),
				`"_compressed_summary": 1`)
			record := append(hermesFixture(t, "0.23.3", "session-steered.jsonl"), edited(t, summary,
				func(m map[string]any) {
					r := m["r"].(map[string]any)
					r["id"], r["content"] = 999, words
				})...)
			box := sandbox.NewFakeSandbox("b")
			file := PathsFor(box).HermesRecordRead()
			box.Put(PathsFor(box).HermesRunStart(), hermesFixtureTitle+" 0\n")
			box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
				if !strings.Contains(cmd, "hcarry dump ") {
					return sandbox.ExecResult{ExitCode: 1}, nil
				}
				box.Put(file, string(record))
				return sandbox.ExecResult{Stdout: "crewlet-carry ok\n"}, nil
			}
			return oneWalkBox{NewHermes(), box, sandbox.MainThreadAsk{Session: hermesFixtureTitle,
				Release: hermesFloor(), Takes: []sandbox.AskedTake{{Take: "steered",
					Digest: ItemDigest(hermesSteeredNote)}}}, file, []string{"steered"}}
		}},
		{name: "codex", box: func(t *testing.T) oneWalkBox {
			const note = "a note the run was handed"
			compacted := lineOf(t, map[string]any{"timestamp": "2026-11-23T09:00:02.000Z", "type": "compacted",
				"payload": map[string]any{"message": words}})
			record := codexUserLine(t, note) + "\n" + codexReplyLine(t) + "\n" + string(compacted)
			box := sandbox.NewFakeSandbox("b")
			p := PathsFor(box)
			file := p.CodexSessions() + "/2026/11/23/rollout-2026-11-23T09-00-00-" + codexConfirmThread + ".jsonl"
			box.Put(file, record)
			box.Put(p.CodexRunStart(), codexConfirmThread+" 0\n")
			answering(box, map[string]string{"rollout": file + "\n"})
			return oneWalkBox{New(Codex{}), box, sandbox.MainThreadAsk{Session: codexConfirmThread,
				Release: codexCarryRelease, Takes: []sandbox.AskedTake{codexTake("held", note)}}, file,
				[]string{"held"}}
		}},
		{name: "qwen code", box: func(t *testing.T) oneWalkBox {
			const session = "11111111-2222-4333-8444-000000000001"
			_, compression := lineWith(t, fixtureLines([]byte(gunzipped(t, qwenSummaryFixture))), `"chat_compression"`)
			record := append(readTestdata(t, "qwen", "0.28.4", "chat-note.jsonl"), edited(t, compression,
				func(m map[string]any) {
					valueAt(m, "systemPayload", "compressedHistory", 0, "parts", 0).(map[string]any)["text"] = words
				})...)
			box := sandbox.NewFakeSandbox("b")
			file := PathsFor(box).QwenDir() + "/projects/-work-proj/chats/" + session + ".jsonl"
			box.Put(file, string(record))
			box.Put(PathsFor(box).QwenRunStart(), session+" 0\n")
			answering(box, map[string]string{"/chats/": file + "\n"})
			takes := qwenTakes("MARK-R6-START drained note MARK-R6-END")
			return oneWalkBox{New(QwenCode{}), box, sandbox.MainThreadAsk{Session: session, Release: qwenRelease,
				Takes: []sandbox.AskedTake{{Take: takes[0].InboxID, Digest: takes[0].Digest}}}, file, []string{"i-1"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.box(t)
			var mu sync.Mutex
			streams := 0
			b.box.Streams = func(p string, r io.Reader) io.Reader {
				if p == path.Clean(b.record) {
					mu.Lock()
					streams++
					mu.Unlock()
				}
				return r
			}
			ask := b.ask
			ask.Summary = true
			got := b.runner.ReadMainThread(t.Context(), b.box, ask)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case !slices.Equal(got.Receipts.Held, b.held):
				t.Errorf("receipts = %+v, want %q held", got.Receipts, b.held)
			case got.Summary.Unread != "" || got.Summary.Text != words:
				t.Errorf("the summary read %.80q (unread %q), want the record's %d bytes", got.Summary.Text,
					got.Summary.Unread, len(words))
			case streams != 1:
				t.Errorf("the record was streamed %d times, want once", streams)
			}
		})
	}
}
