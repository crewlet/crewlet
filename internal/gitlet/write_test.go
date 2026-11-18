package gitlet_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// writeOp is an operation id minted at at.
func writeOp(at time.Time) string { return statelog.NewOpID(at, "") }

// aWrite is a commit_files any party may act on: alice's put of one file onto
// a branch she creates from a commit, made for nobody but alice.
func aWrite() gitlet.EngineWrite {
	text := "hello\n"
	return gitlet.EngineWrite{
		Kind: gitlet.WriteCommitFiles, RepoID: "0190f3c4-0000-7000-8000-0000000000aa",
		Principal: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "alice",
			Run: gitlet.RunProvenance{TurnID: "0192f00d-0000-7000-8000-000000000001"}},
		OpID: writeOp(time.Now()), WrittenFor: types.ForNobody(), Ref: "refs/heads/topic", Base: oidOf("B"),
		Message: "a change", Files: []gitlet.FileOp{{Op: gitlet.FileOpPut, Path: "docs/a.md", Content: &text}},
	}
}

// tagWrite is a tag write of kind any party may act on but for what a case
// changes: at commit, or annotated with message.
func tagWrite(kind gitlet.EngineWriteKind, ref, commit, message string) gitlet.EngineWrite {
	return gitlet.EngineWrite{Kind: kind, RepoID: aWrite().RepoID, Principal: aWrite().Principal,
		WrittenFor: aWrite().WrittenFor, OpID: aWrite().OpID, Ref: ref, Commit: commit, Message: message}
}

// A TAG WRITE NAMES A TAG: create_tag at a commit, annotated by a message; delete_tag the value
// read. Mutation: read Annotates off the kind alone, red.
func TestATagWriteNamesATag(t *testing.T) {
	t.Parallel()
	light := tagWrite(gitlet.WriteCreateTag, "refs/tags/release/1.0", oidOf("C"), "")
	annotated := tagWrite(gitlet.WriteCreateTag, "refs/tags/v1", oidOf("C"), "The first release")
	deleted := tagWrite(gitlet.WriteDeleteTag, "refs/tags/v1", "", "")
	deleted.ExpectedHead = oidOf("T")
	for _, w := range []gitlet.EngineWrite{light, annotated, deleted} {
		if err := w.Validate(); err != nil {
			t.Errorf("%s %s: %v", w.Kind, w.Ref, err)
		}
	}
	if light.Annotates() || !annotated.Annotates() || deleted.Annotates() {
		t.Errorf("Annotates: light %t, annotated %t, delete %t; want only the annotated create",
			light.Annotates(), annotated.Annotates(), deleted.Annotates())
	}
	if !gitlet.WriteCreateTag.Tags() || !gitlet.WriteDeleteTag.Tags() || gitlet.WriteCreateBranch.Tags() {
		t.Error("Tags is not exactly the two tag kinds")
	}
}

// AN ENGINE WRITE IS HELD TO ONE RULE by the tool, the router and the building
// Gitlet node alike, refused ErrInvalid naming the field: each kind takes only
// its own fields; a seat names its turn (never an executor, whose box pushes
// through git), a person their human seat; the engine mints the operation;
// payload, operations, paths and path depth are bounded; one operation names a
// path; a link's target is text alone; git writes the message as given.
//
// Mutations, each red: admit a blob_oid for a link, count a move's destination
// as no path, or let a seat's run name an executor (it would land as a proposal
// its committer refuses) — its row passes; drop the depth bound, or hold a
// move's destination to none — a path one component past it passes; refuse a
// path at the bound — the write at it is refused.
func TestAnEngineWriteIsHeldToOneRule(t *testing.T) {
	t.Parallel()
	if err := aWrite().Validate(); err != nil {
		t.Fatalf("a well-formed write: %v", err)
	}
	str := func(s string) *string { return &s }
	edit := func(w gitlet.EngineWrite, f func(*gitlet.EngineWrite)) gitlet.EngineWrite {
		f(&w)
		return w
	}
	// deep is a path exactly as deep as a write may name: admitted, and one
	// component more refused, whichever field names it.
	deep := strings.TrimSuffix(strings.Repeat("d/", gitlet.MaxTreePathDepth), "/")
	for _, w := range []gitlet.EngineWrite{
		edit(aWrite(), func(w *gitlet.EngineWrite) { w.Files[0].Path = deep }),
		edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpMove, Path: deep, To: "e/" + deep[2:]}
		}),
	} {
		if err := w.Validate(); err != nil {
			t.Errorf("a write naming paths %d components deep, the bound: %v", gitlet.MaxTreePathDepth, err)
		}
	}
	pastDepth := fmt.Sprintf("%d components deep", gitlet.MaxTreePathDepth+1)
	big := strings.Repeat("x", gitlet.MaxCommitPayloadBytes)
	many := make([]gitlet.FileOp, gitlet.MaxFileOps+1)
	for i := range many {
		many[i] = gitlet.FileOp{Op: gitlet.FileOpDelete, Path: fmt.Sprintf("f%d", i)}
	}
	for name, tc := range map[string]struct {
		w     gitlet.EngineWrite
		names string
	}{
		"an unknown kind": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Kind = "rebase" }), "kind:"},
		"no repository":   {edit(aWrite(), func(w *gitlet.EngineWrite) { w.RepoID = "acme" }), "repo_id:"},
		"a seat with no run": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Principal.Run = gitlet.RunProvenance{} }),
			"principal.run:"},
		"a seat naming an executor": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Principal.Run.RunID, w.Principal.Run.Harness = "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", "claude-code"
		}), "principal.run.run_id:"},
		"a token bound to nobody": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Principal = gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
		}), "principal.seat:"},
		"an operation id this engine never mints": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.OpID = "abc" }),
			"op_id"},
		"a floor on another log": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Fresh = statelog.Freshness{Level: statelog.ReadSession,
				Floors: []statelog.Position{{Stream: topics.TrackerLogStream, Seq: 1}}}
		}), "fresh:"},
		"a tag": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Ref = "refs/tags/v1" }), "ref:"},
		"a short head": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Base, w.ExpectedHead = "", "abc123"
		}), "expected_head:"},
		"a base beside a head": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.ExpectedHead = oidOf("C") }),
			"base:"},
		"a commit with no files": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Files = nil }), "files:"},
		"a commit with no message": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Message = "" }),
			"message:"},
		"a commit naming a commit": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Commit = oidOf("C") }),
			"commit:"},
		"too many operations": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Files = many }), "files:"},
		"a payload past its bound": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0].Content = str(big)
		}), "files:"},
		"one path twice": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files = append(w.Files, gitlet.FileOp{Op: gitlet.FileOpDelete, Path: "docs/a.md"})
		}), "files[1]:"},
		"a move onto a path another names": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files = append(w.Files, gitlet.FileOp{Op: gitlet.FileOpMove, Path: "b.md", To: "docs/a.md"})
		}), "files[1]:"},
		"a put of text and a blob": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0].BlobOID = oidOf("blob")
		}), "files[0]:"},
		"a link from a blob": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpPut, Path: "l", BlobOID: oidOf("blob"), Mode: gitlet.ModeLink}
		}), "files[0].blob_oid:"},
		"a link to nothing": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpPut, Path: "l", Content: str(""), Mode: gitlet.ModeLink}
		}), "files[0].content:"},
		"a link holding a NUL": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpPut, Path: "l", Content: str("a\x00b"), Mode: gitlet.ModeLink}
		}), "files[0].content:"},
		"a linked .gitmodules": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpPut, Path: "x/.GITMODULES", Content: str("a"),
				Mode: gitlet.ModeLink}
		}), "files[0].path:"},
		"a mode making a link": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpMode, Path: "a", Mode: gitlet.ModeLink}
		}), "files[0].mode:"},
		"an edit of nothing": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpEdit, Path: "a", New: "b"}
		}), "files[0].old:"},
		"an edit changing nothing": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpEdit, Path: "a", Old: "b", New: "b"}
		}), "files[0].new:"},
		"a delete carrying text": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpDelete, Path: "a", Content: str("b")}
		}), "files[0].content:"},
		"a message holding a NUL": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Message = "a\x00b" }),
			"message:"},
		"a message git rewrites": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Message = "a \uffff" }),
			"message:"},
		"a message that is no UTF-8": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Message = "caf\xe9" }),
			"message:"},
		"a path into .git": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Files[0].Path = "a/.Git/config" }),
			"files[0].path:"},
		"a path past the depth bound": {edit(aWrite(), func(w *gitlet.EngineWrite) { w.Files[0].Path = deep + "/x" }),
			"files[0].path: " + pastDepth},
		"a move past the depth bound": {edit(aWrite(), func(w *gitlet.EngineWrite) {
			w.Files[0] = gitlet.FileOp{Op: gitlet.FileOpMove, Path: "a", To: deep + "/x"}
		}), "files[0].to: " + pastDepth},
		"a revert of nothing": {gitlet.EngineWrite{Kind: gitlet.WriteRevert, RepoID: aWrite().RepoID,
			Principal: aWrite().Principal, WrittenFor: aWrite().WrittenFor, OpID: aWrite().OpID,
			Ref: "refs/heads/main", ExpectedHead: oidOf("H")}, "commit:"},
		"a create with a message": {gitlet.EngineWrite{Kind: gitlet.WriteCreateBranch, RepoID: aWrite().RepoID,
			Principal: aWrite().Principal, WrittenFor: aWrite().WrittenFor, OpID: aWrite().OpID,
			Ref: "refs/heads/x", Commit: oidOf("C"), Message: "m"}, "message:"},
		"a delete of no head": {gitlet.EngineWrite{Kind: gitlet.WriteDeleteBranch, RepoID: aWrite().RepoID,
			Principal: aWrite().Principal, WrittenFor: aWrite().WrittenFor, OpID: aWrite().OpID,
			Ref: "refs/heads/x"}, "expected_head:"},
		"a tag create on a branch": {tagWrite(gitlet.WriteCreateTag, "refs/heads/v1", oidOf("C"), ""), "ref:"},
		"a branch create on a tag": {edit(tagWrite(gitlet.WriteCreateTag, "refs/tags/v1", oidOf("C"), ""),
			func(w *gitlet.EngineWrite) { w.Kind = gitlet.WriteCreateBranch }), "ref:"},
		"a tag named HEAD": {tagWrite(gitlet.WriteCreateTag, "refs/tags/HEAD", oidOf("C"), ""), "ref:"},
		"a tag of nothing": {tagWrite(gitlet.WriteCreateTag, "refs/tags/v1", "", ""), "commit:"},
		"a tag create on a head": {edit(tagWrite(gitlet.WriteCreateTag, "refs/tags/v1", oidOf("C"), ""),
			func(w *gitlet.EngineWrite) { w.ExpectedHead = oidOf("H") }), "expected_head:"},
		"a tag delete of no value": {tagWrite(gitlet.WriteDeleteTag, "refs/tags/v1", "", ""), "expected_head:"},
		"a tag delete with a message": {edit(tagWrite(gitlet.WriteDeleteTag, "refs/tags/v1", "", ""),
			func(w *gitlet.EngineWrite) { w.ExpectedHead, w.Message = oidOf("T"), "m" }), "message:"},
		"an annotation git rewrites": {tagWrite(gitlet.WriteCreateTag, "refs/tags/v1", oidOf("C"), "a \uffff"),
			"message:"},
	} {
		err := tc.w.Validate()
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.names) {
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
	id := gitlet.WritePushID(op)
	parsed, err := uuid.Parse(id)
	switch {
	case err != nil || parsed.String() != id || parsed.Version() != 7:
		t.Fatalf("the push id %q is no canonical UUIDv7: %v", id, err)
	case gitlet.WritePushID(op) != id:
		t.Error("two asks of one operation derive two push ids")
	case gitlet.WritePushID(writeOp(at)) == id:
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

// AN ENGINE WRITE'S PAYLOAD IS MEASURED AS THE WIRE SPELLS IT: its operations
// or suggestions and its message, each as encoding/json encodes it (a '<' in
// six bytes), against gitlet.MaxCommitPayloadBytes for every kind carrying any
// — by length, a mebibyte of '<' crossed as six. Half the bound of '<' as text
// (commit_files, apply_suggestions) or one character past it as JSON spells it
// (revert, cherry-pick) is refused; exactly at the bound is admitted.
//
// Mutations: measure the message by its length — the half-bound '<' messages
// and the one-past ones pass, red; bound only a commit_files' payload — the
// revert's, the cherry-pick's and the apply's one-past writes pass, red;
// refuse at the bound — the writes at it are refused, red.
func TestAnEngineWritesPayloadIsMeasuredAsTheWireSpellsIt(t *testing.T) {
	t.Parallel()
	spelled := func(v any) int {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return len(encoded)
	}
	// lt is n '<', each six bytes as JSON spells it.
	lt := func(n int) string { return strings.Repeat("<", n) }
	pick := func(kind gitlet.EngineWriteKind, message string) gitlet.EngineWrite {
		return gitlet.EngineWrite{Kind: kind, RepoID: aWrite().RepoID, Principal: aWrite().Principal,
			WrittenFor: aWrite().WrittenFor, OpID: aWrite().OpID, Ref: "refs/heads/main", ExpectedHead: oidOf("H"),
			Commit: oidOf("C"), Message: message}
	}
	commit := func(message string) gitlet.EngineWrite {
		w := aWrite()
		w.Message = message
		return w
	}
	apply := func(message string) gitlet.EngineWrite {
		w := anApply()
		w.Message = message
		return w
	}
	// payload is what the write carries as JSON spells it: the files or the
	// suggestions and the message the write sets.
	payload := func(w gitlet.EngineWrite) int {
		n := 0
		if len(w.Files) > 0 {
			n += spelled(w.Files)
		}
		if len(w.Suggestions) > 0 {
			n += spelled(w.Suggestions)
		}
		if w.Message != "" {
			n += spelled(w.Message)
		}
		return n
	}
	// fill is a message JSON spells in exactly room bytes: as many '<' as
	// fit beside its quotes, and the rest in plain bytes.
	fill := func(room int) string {
		n := (room - 2) / 6
		return lt(n) + strings.Repeat("x", room-2-6*n)
	}
	files, suggestions := spelled(aWrite().Files), spelled(anApply().Suggestions)
	sixth := (gitlet.MaxCommitPayloadBytes - 2) / 6
	for name, tc := range map[string]struct {
		w       gitlet.EngineWrite
		atBound bool
	}{
		"a revert at the bound":                    {pick(gitlet.WriteRevert, fill(gitlet.MaxCommitPayloadBytes)), true},
		"a cherry-pick at the bound":               {pick(gitlet.WriteCherryPick, fill(gitlet.MaxCommitPayloadBytes)), true},
		"a commit whose files and message fill it": {commit(fill(gitlet.MaxCommitPayloadBytes - files)), true},
		"an apply whose suggestions and message fill it": {apply(fill(gitlet.MaxCommitPayloadBytes - suggestions)),
			true},
		"a revert whose message of '<' fits":     {pick(gitlet.WriteRevert, lt(sixth)), false},
		"a revert taking git's own message":      {pick(gitlet.WriteRevert, ""), false},
		"a cherry-pick taking git's own message": {pick(gitlet.WriteCherryPick, ""), false},
	} {
		if n := payload(tc.w); n > gitlet.MaxCommitPayloadBytes || tc.atBound && n != gitlet.MaxCommitPayloadBytes {
			t.Fatalf("%s: the case's premise does not hold — it carries %d bytes as JSON spells them", name, n)
		}
		if err := tc.w.Validate(); err != nil {
			t.Errorf("%s: %v, want it admitted", name, err)
		}
	}
	for name, tc := range map[string]struct {
		w     gitlet.EngineWrite
		names string
	}{
		"a commit whose message of '<' is half the bound as text": {commit(lt(gitlet.MaxCommitPayloadBytes / 2)),
			"files:"},
		"a commit one byte past the bound": {commit(fill(gitlet.MaxCommitPayloadBytes-files) + "x"), "files:"},
		"an apply whose message of '<' is half the bound as text": {apply(lt(gitlet.MaxCommitPayloadBytes / 2)),
			"suggestions:"},
		"an apply one byte past the bound": {apply(fill(gitlet.MaxCommitPayloadBytes-suggestions) + "x"),
			"suggestions:"},
		"a revert one '<' past the bound":      {pick(gitlet.WriteRevert, lt(sixth+1)), "message:"},
		"a cherry-pick one '<' past the bound": {pick(gitlet.WriteCherryPick, lt(sixth+1)), "message:"},
		"a revert one byte past the bound": {pick(gitlet.WriteRevert, fill(gitlet.MaxCommitPayloadBytes)+"x"),
			"message:"},
	} {
		err := tc.w.Validate()
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.names) {
			t.Errorf("%s: %v, want ErrInvalid naming %s", name, err, tc.names)
		}
	}
}

// A TREE PATH GIT WOULD REFUSE IS REFUSED BY NAME, before anything is read:
// every name fsck flags (run by the build's index-pack --strict and the
// relay's receive.fsckObjects), written into a tree with mktree and checked by
// `git fsck` each run, ValidTreePath refuses — and more (a control character,
// a backslash, a code point HFS+ ignores), which no tool writes. Ordinary
// names pass both, as does a path deeper than a write may name: depth bounds
// one write (gitlet.MaxTreePathDepth), never a name: the name rule is what a
// check's report path is held to as well.
//
// Mutations: drop the `:` rule — `.GIT::$INDEX_ALLOCATION`, which fsck flags,
// passes, red; hold a name to the write's depth bound — the deep path is
// refused, red.
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
		refused := gitlet.ValidTreePath(name) != nil
		if flagged && !refused {
			t.Errorf("%q: git's check of a tree flags it, and ValidTreePath admits it", name)
		}
		if !refused && strings.ContainsAny(name, "\\\x01\t\n") {
			t.Errorf("%q: ValidTreePath admits a name with a backslash or a control character", name)
		}
	}
	for _, ok := range []string{"README.md", ".github/workflows/ci.yml", ".gitignore", "a.git/b", "x/y/z.txt",
		strings.Repeat("d/", gitlet.MaxTreePathDepth) + "x"} {
		if err := gitlet.ValidTreePath(ok); err != nil {
			t.Errorf("%q: %v, want it admitted", ok, err)
		}
	}
	for _, bad := range []string{"", "/a", "a/", "a//b", "a/./b", "a/../b",
		strings.Repeat("a", 256), strings.Repeat("a/", gitlet.MaxTreePathBytes/2) + "a", "caf\xe9"} {
		if err := gitlet.ValidTreePath(bad); err == nil {
			t.Errorf("%q: admitted", bad)
		}
	}
}

// THE ENGINE'S COMMITTER IS A CONVENTION, read exactly: `Crewlet` at `engine@` on any
// domain; another case, more name, no domain, a second `@` or padding marks nothing.
// Mutation: EqualFold the name, split at the last `@`, or TrimSpace.
func TestEngineCommittedIsTheConvention(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, email string
		marked      bool
	}{
		{"Crewlet", "engine@seats.example", true},
		{"Crewlet", "engine@other.example", true},
		{gitlet.EngineCommitterName, gitlet.EngineCommitterLocal + "@x", true},
		{"crewlet", "engine@x.example", false},
		{"CREWLET", "engine@x.example", false},
		{"Crewlet", "Engine@x.example", false},
		{"Crewlet Bot", "engine@x.example", false},
		{" Crewlet", "engine@x.example", false},
		{"Crewlet", " engine@x.example", false},
		{"Crewlet", "engine@x.example ", false},
		{"Crewlet", "engine@", false},
		{"Crewlet", "engine", false},
		{"Crewlet", "@x.example", false},
		{"Crewlet", "engine@a@x.example", false},
		{"Crewlet", "engine+ci@x.example", false},
		{"Crewlet", "engine@x .example", false},
		{"Crewlet", "engine@x.example\x7f", false},
		{"Crewlet", "", false},
		{"", "engine@x.example", false},
	} {
		if got := gitlet.EngineCommitted(tc.name, tc.email); got != tc.marked {
			t.Errorf("EngineCommitted(%q, %q) = %t, want %t", tc.name, tc.email, got, tc.marked)
		}
	}
}
