package code_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// writeOp is an operation id minted at at.
func writeOp(at time.Time) string { return statelog.NewOpID(at, "") }

// aWrite is a commit_files any party may act on: alice's put of one file onto
// a branch she creates from a commit.
func aWrite() code.EngineWrite {
	text := "hello\n"
	return code.EngineWrite{
		Kind: code.WriteCommitFiles, RepoID: "0190f3c4-0000-7000-8000-0000000000aa",
		Principal: code.Principal{Kind: code.PrincipalAgent, Seat: "alice",
			Run: code.RunProvenance{TurnID: "0192f00d-0000-7000-8000-000000000001"}},
		OpID: writeOp(time.Now()), Ref: "refs/heads/topic", Base: oidOf("B"), Message: "a change",
		Files: []code.FileOp{{Op: code.FileOpPut, Path: "docs/a.md", Content: &text}},
	}
}

// AN ENGINE WRITE IS HELD TO ONE RULE by every party to it — the tool that asks
// it, the router that carries it, the git node that builds it — and refused
// ErrInvalid naming the field: each kind takes the fields it needs and no
// other; a seat names its run and a person their human seat; the operation is
// one this engine mints; the payload, the operations and every path are
// bounded; one operation names a path; a link takes its target as text alone;
// and the message is what git writes as given.
//
// Mutations: admit a blob_oid for a link — its row passes, red; count a move's
// destination as no path — the colliding row passes, red.
func TestAnEngineWriteIsHeldToOneRule(t *testing.T) {
	t.Parallel()
	if err := aWrite().Validate(); err != nil {
		t.Fatalf("a well-formed write: %v", err)
	}
	str := func(s string) *string { return &s }
	edit := func(w code.EngineWrite, f func(*code.EngineWrite)) code.EngineWrite {
		f(&w)
		return w
	}
	big := strings.Repeat("x", code.MaxCommitPayloadBytes)
	many := make([]code.FileOp, code.MaxFileOps+1)
	for i := range many {
		many[i] = code.FileOp{Op: code.FileOpDelete, Path: fmt.Sprintf("f%d", i)}
	}
	for name, tc := range map[string]struct {
		w     code.EngineWrite
		names string
	}{
		"an unknown kind": {edit(aWrite(), func(w *code.EngineWrite) { w.Kind = "rebase" }), "kind:"},
		"no repository":   {edit(aWrite(), func(w *code.EngineWrite) { w.RepoID = "acme" }), "repo_id:"},
		"a seat with no run": {edit(aWrite(), func(w *code.EngineWrite) { w.Principal.Run = code.RunProvenance{} }),
			"principal.run:"},
		"a token bound to nobody": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Principal = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ci"}
		}), "principal.seat:"},
		"an operation id this engine never mints": {edit(aWrite(), func(w *code.EngineWrite) { w.OpID = "abc" }),
			"op_id"},
		"a floor on another log": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Fresh = statelog.Freshness{Level: statelog.ReadSession,
				Floors: []statelog.Position{{Stream: topics.TrackerLogStream, Seq: 1}}}
		}), "fresh:"},
		"a tag": {edit(aWrite(), func(w *code.EngineWrite) { w.Ref = "refs/tags/v1" }), "ref:"},
		"a short head": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Base, w.ExpectedHead = "", "abc123"
		}), "expected_head:"},
		"a base beside a head": {edit(aWrite(), func(w *code.EngineWrite) { w.ExpectedHead = oidOf("C") }),
			"base:"},
		"a commit with no files": {edit(aWrite(), func(w *code.EngineWrite) { w.Files = nil }), "files:"},
		"a commit with no message": {edit(aWrite(), func(w *code.EngineWrite) { w.Message = "" }),
			"message:"},
		"a commit naming a commit": {edit(aWrite(), func(w *code.EngineWrite) { w.Commit = oidOf("C") }),
			"commit:"},
		"too many operations": {edit(aWrite(), func(w *code.EngineWrite) { w.Files = many }), "files:"},
		"a payload past its bound": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0].Content = str(big)
		}), "files:"},
		"one path twice": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files = append(w.Files, code.FileOp{Op: code.FileOpDelete, Path: "docs/a.md"})
		}), "files[1]:"},
		"a move onto a path another names": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files = append(w.Files, code.FileOp{Op: code.FileOpMove, Path: "b.md", To: "docs/a.md"})
		}), "files[1]:"},
		"a put of text and a blob": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0].BlobOID = oidOf("blob")
		}), "files[0]:"},
		"a link from a blob": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0] = code.FileOp{Op: code.FileOpPut, Path: "l", BlobOID: oidOf("blob"), Mode: code.ModeLink}
		}), "files[0].blob_oid:"},
		"a link to nothing": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0] = code.FileOp{Op: code.FileOpPut, Path: "l", Content: str(""), Mode: code.ModeLink}
		}), "files[0].content:"},
		"a link holding a NUL": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0] = code.FileOp{Op: code.FileOpPut, Path: "l", Content: str("a\x00b"), Mode: code.ModeLink}
		}), "files[0].content:"},
		"a linked .gitmodules": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0] = code.FileOp{Op: code.FileOpPut, Path: "x/.GITMODULES", Content: str("a"),
				Mode: code.ModeLink}
		}), "files[0].path:"},
		"a mode making a link": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0] = code.FileOp{Op: code.FileOpMode, Path: "a", Mode: code.ModeLink}
		}), "files[0].mode:"},
		"an edit of nothing": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0] = code.FileOp{Op: code.FileOpEdit, Path: "a", New: "b"}
		}), "files[0].old:"},
		"an edit changing nothing": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0] = code.FileOp{Op: code.FileOpEdit, Path: "a", Old: "b", New: "b"}
		}), "files[0].new:"},
		"a delete carrying text": {edit(aWrite(), func(w *code.EngineWrite) {
			w.Files[0] = code.FileOp{Op: code.FileOpDelete, Path: "a", Content: str("b")}
		}), "files[0].content:"},
		"a message holding a NUL": {edit(aWrite(), func(w *code.EngineWrite) { w.Message = "a\x00b" }),
			"message:"},
		"a message git rewrites": {edit(aWrite(), func(w *code.EngineWrite) { w.Message = "a \uffff" }),
			"message:"},
		"a message that is no UTF-8": {edit(aWrite(), func(w *code.EngineWrite) { w.Message = "caf\xe9" }),
			"message:"},
		"a path into .git": {edit(aWrite(), func(w *code.EngineWrite) { w.Files[0].Path = "a/.Git/config" }),
			"files[0].path:"},
		"a revert of nothing": {code.EngineWrite{Kind: code.WriteRevert, RepoID: aWrite().RepoID,
			Principal: aWrite().Principal, OpID: aWrite().OpID, Ref: "refs/heads/main", ExpectedHead: oidOf("H")},
			"commit:"},
		"a create with a message": {code.EngineWrite{Kind: code.WriteCreateBranch, RepoID: aWrite().RepoID,
			Principal: aWrite().Principal, OpID: aWrite().OpID, Ref: "refs/heads/x", Commit: oidOf("C"),
			Message: "m"}, "message:"},
		"a delete of no head": {code.EngineWrite{Kind: code.WriteDeleteBranch, RepoID: aWrite().RepoID,
			Principal: aWrite().Principal, OpID: aWrite().OpID, Ref: "refs/heads/x"}, "expected_head:"},
	} {
		err := tc.w.Validate()
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), tc.names) {
			t.Errorf("%s: %v, want ErrInvalid naming %s", name, err, tc.names)
		}
	}
}

// AN ENGINE WRITE'S PUSH IS ITS OPERATION'S: an unnamed derivation at the
// operation's own instant — a canonical lowercase UUIDv7, which a scratch's
// name and a proposal's push id must be — the same for every ask of one
// operation and another for every other, and the commit's author date the
// operation's second.
//
// Mutation: a named derivation — the id carries a `.code_write` suffix, red.
func TestAnEngineWritesPushIsItsOperations(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 28, 9, 30, 15, 0, time.UTC).Add(700 * time.Millisecond)
	op := writeOp(at)
	id := code.WritePushID(op)
	parsed, err := uuid.Parse(id)
	switch {
	case err != nil || parsed.String() != id || parsed.Version() != 7:
		t.Fatalf("the push id %q is no canonical UUIDv7: %v", id, err)
	case code.WritePushID(op) != id:
		t.Error("two asks of one operation derive two push ids")
	case code.WritePushID(writeOp(at)) == id:
		t.Error("two operations at one instant derive one push id")
	}
	if minted, ok := statelog.OpMintedAt(id); !ok || !minted.Equal(at.Truncate(time.Millisecond)) {
		t.Errorf("the push id is minted at %v (%t), want the operation's instant %v", minted, ok, at)
	}
	w := aWrite()
	w.OpID = op
	if got := w.Instant(); !got.Equal(at.Truncate(time.Second)) {
		t.Errorf("the write's instant is %v, want its operation's second %v", got, at.Truncate(time.Second))
	}
}

// A TREE PATH GIT WOULD REFUSE IS REFUSED BY NAME, before anything is read:
// every name git's own check of a tree flags — fsck, which the build's
// index-pack --strict and the relay's receive.fsckObjects run — written into a
// tree with mktree and checked by `git fsck` on every run, is a name
// ValidTreePath refuses; and it refuses more (a control character, a
// backslash, a code point HFS+ ignores), which no tool needs in a name it
// writes. The ordinary names beside them pass both.
//
// Mutation: drop the `:` rule — `.GIT::$INDEX_ALLOCATION`, which fsck flags,
// passes, red.
func TestATreePathGitWouldRefuseIsRefused(t *testing.T) {
	t.Parallel()
	names := []string{
		".git", ".GIT", ".Git", ".git.", ".git ", ".git. .", "git~1", "GIT~1", "git~1.",
		".git::$INDEX_ALLOCATION", ".git:stream", ".g\u200cit", ".gi\u200dt", "\ufeff.git",
		".", "..", "a\\b", ".git\\x", "a\x01b", "tab\there", "line\nfeed",
		"README.md", ".github", ".gitignore", ".gitx", "git", "a.git", "git~2", "x.y",
	}
	c := gittest.NewClient(t)
	for _, name := range names {
		dir := c.Init(t)
		blob := strings.TrimSpace(c.Run(t, dir, "hash-object", "-w", "--stdin"))
		in := fmt.Sprintf("100644 blob %s\t%s\x00", blob, name)
		tree, _, err := c.Try(t, dir, strings.NewReader(in), "mktree", "-z")
		flagged := err != nil
		if !flagged {
			out, stderr, ferr := c.Try(t, dir, nil, "fsck", "--strict", "--no-dangling")
			flagged = ferr != nil || strings.Contains(out+stderr, "in tree "+strings.TrimSpace(tree))
		}
		refused := code.ValidTreePath(name) != nil
		if flagged && !refused {
			t.Errorf("%q: git's check of a tree flags it, and ValidTreePath admits it", name)
		}
		if !refused && strings.ContainsAny(name, "\\\x01\t\n") {
			t.Errorf("%q: ValidTreePath admits a name with a backslash or a control character", name)
		}
	}
	for _, ok := range []string{"README.md", ".github/workflows/ci.yml", ".gitignore", "a.git/b", "x/y/z.txt"} {
		if err := code.ValidTreePath(ok); err != nil {
			t.Errorf("%q: %v, want it admitted", ok, err)
		}
	}
	for _, bad := range []string{"", "/a", "a/", "a//b", "a/./b", "a/../b",
		strings.Repeat("a", 256), strings.Repeat("a/", code.MaxTreePathBytes/2) + "a", "caf\xe9"} {
		if err := code.ValidTreePath(bad); err == nil {
			t.Errorf("%q: admitted", bad)
		}
	}
}
