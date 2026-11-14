package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A project's file copied into an artifact (`from_project_file`) is
// read_project_file's read: decided by that tool's verb, its path held to the
// tracker's rule, and every refusal in that tool's words. Every refusal case
// below runs through each of the three calls that copy one, and so does the
// seat's half of the authority case; the person's half runs through
// create_artifact, which holds the operator surface's wiring, and what a copy
// reserves and moves is held through create_artifact, with publish_artifact
// for a file the artifact already holds.

// newCopyRig is the artifacts tools registered for a seat beside a project's
// files, over fakes, behind the real authority decision: the artifact rig
// ([newArtifactRig]) on a node whose company runs Worklet. read_project_file
// is registered beside them, so a case can ask it the same question.
func newCopyRig(t *testing.T, req requesters) (artifactRig, *fakeFiles, *fakeObjects) {
	t.Helper()
	f, files, objects := newFakeArtifacts(), newFakeFiles(), newFakeObjects()
	chart := artifactChart{}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{Artifacts: artifactDeps(f, chart, req),
		Work: fileDeps(files, objects), Requesters: req,
		Authorize: builtin.Decide(chart)}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return artifactRig{f: f, reg: reg}, files, objects
}

// keepFile keeps content as the ENG project's file at filePath, as
// write_project_file leaves one: its bytes an object of their own, and a row
// naming that object at the content's digest and size.
func keepFile(t *testing.T, files *fakeFiles, objects *fakeObjects, filePath, content string) worklet.File {
	t.Helper()
	data := []byte(content)
	key := objstore.KeyAt(fakeAt)
	objects.mu.Lock()
	objects.objects[key] = data
	objects.mu.Unlock()
	file := worklet.File{Project: "ENG", Path: filePath, ContentType: "text/markdown; charset=utf-8",
		Hash: objstore.HashOf(data), Size: int64(len(data)), Object: key, Version: 1,
		CreatedBy: "founder", CreatedAt: fakeAt, UpdatedBy: "founder", UpdatedAt: fakeAt}
	files.mu.Lock()
	files.files[files.key("ENG", filePath)] = file
	files.mu.Unlock()
	return file
}

// copyDoor is one of the three calls that copy a project's file into an
// artifact: its tool, and its arguments naming ref as `from_project_file`,
// over an artifact of agent-cto's the rig is seeded with where the call needs
// one.
type copyDoor struct {
	tool string
	args func(r artifactRig, ref map[string]any) map[string]any
}

// copyDoors are the three: a file of a new artifact, a file of a new version,
// and an asset.
var copyDoors = []copyDoor{
	{builtin.CreateArtifactTool, func(_ artifactRig, ref map[string]any) map[string]any {
		return map[string]any{"kind": "html", "title": "Quarterly report", "content": "<p>Q3</p>",
			"files": map[string]any{"report.md": map[string]any{"from_project_file": ref}}}
	}},
	{builtin.PublishArtifactTool, func(r artifactRig, ref map[string]any) map[string]any {
		h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>Q3</p>"})
		return map[string]any{"artifact": h.ID.String(), "base_version": 1,
			"files": map[string]any{"report.md": map[string]any{"from_project_file": ref}}}
	}},
	{builtin.UploadArtifactAssetTool, func(r artifactRig, ref map[string]any) map[string]any {
		h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>Q3</p>"})
		r.f.mu.Lock()
		held := r.f.heads[h.ID]
		held.Capabilities = []artifacts.Capability{artifacts.CapabilityAssets}
		r.f.heads[h.ID] = held
		r.f.mu.Unlock()
		return map[string]any{"artifact": h.ID.String(), "name": "logo.png", "from_project_file": ref}
	}},
}

// tally is what a rig's calls reserved, moved and wrote: upload intents,
// transferred files, and creates, publishes and assets together.
type tally struct{ intents, transfers, writes int }

func tallyOf(r artifactRig) tally {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return tally{intents: len(r.f.intents), transfers: len(r.f.transfers),
		writes: len(r.f.creates) + len(r.f.publishes) + len(r.f.assets)}
}

// wordsAfter is a result's sentence after the "<tool>: " it opens with, and
// false for one that does not open with it.
func wordsAfter(res tools.Result, tool string) (string, bool) {
	return strings.CutPrefix(res.Output, tool+": ")
}

// A COPY NAMING A PROJECT OR A FILE THAT IS NOT THERE IS THE ARGUMENT TO
// CHANGE: refused `invalid`, in read_project_file's own sentence, with the
// tracker's sentinel beneath it, and nothing reserved, published or written.
// read_project_file answers the same words `not_found`, because its call is
// ABOUT the file; a copy only names its source, and `not_found` there is a
// 404 about the artifact the call is about, which a person's screen reads as
// gone. Neither is ever an internal fault: the missing project was one, with
// an error in the node's log, and the missing file was told in words of the
// copy's own. The project and the path are spelled as read_project_file takes
// them, so the words hold the copy's own reading of both.
//
// Mutation: tell the copy's read through readFailure again and the project
// rows are internal faults; pass not_found for a copy and every row fails on
// the class; drop the copy's ProjectKey or its normalizePath and the words
// stop being read_project_file's.
func TestACopyNamingWhatIsNotThereIsTheArgumentToChange(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		ref      map[string]any
		sentinel error
	}{
		{"a project that is not there", map[string]any{"project": "ops", "path": "reports/q3.md"},
			worklet.ErrNoProject},
		{"a file that is not there", map[string]any{"project": "eng", "path": "/reports/q4.md"},
			worklet.ErrNoFile},
	} {
		for _, door := range copyDoors {
			t.Run(c.name+"/"+door.tool, func(t *testing.T) {
				t.Parallel()
				r, files, objects := newCopyRig(t, requesters{})
				keepFile(t, files, objects, "reports/q3.md", "# Q3")
				turn := turnFor(t, "agent-cto")
				got := r.call(t, turn, door.tool, door.args(r, c.ref))
				read := r.call(t, turn, worklet.ReadProjectFileTool, c.ref)
				copied, copiedOK := wordsAfter(got, door.tool)
				said, saidOK := wordsAfter(read, worklet.ReadProjectFileTool)
				switch {
				case !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid:
					t.Errorf("answered %v %q, want invalid", tools.RefusalOf(got), got.Output)
				case !errors.Is(got.Cause, c.sentinel):
					t.Errorf("the refusal carries %v, not the tracker's %v", got.Cause, c.sentinel)
				case tools.RefusalOf(read) != tools.RefusalNotFound:
					t.Errorf("read_project_file answered %v %q, want not_found",
						tools.RefusalOf(read), read.Output)
				case !copiedOK || !saidOK || copied != said:
					t.Errorf("the copy said %q and read_project_file %q: want the same words",
						got.Output, read.Output)
				}
				if n := tallyOf(r); n != (tally{}) {
					t.Errorf("a copy naming what is not there moved %+v", n)
				}
			})
		}
	}
}

// A PATH THE TRACKER REFUSES IS REFUSED IN A COPY IN THE TRACKER'S WORDS:
// `invalid`, in the sentence read_project_file gives it and never with the
// "worklet:" the tracker's error opens with, before the tracker is read and
// before anything is reserved. It reached the reader as it was typed, and the
// reader's refusal was told as a fault of this node.
//
// Mutation: drop normalizePath from the copy and every row is an internal
// fault.
func TestAPathTheTrackerRefusesInACopyIsToldInItsWords(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, path, says string }{
		{"an empty folder", "reports//q3.md", "empty folder"},
		{"a trailing slash", "reports/", "ends in a slash"},
	} {
		for _, door := range copyDoors {
			t.Run(c.name+"/"+door.tool, func(t *testing.T) {
				t.Parallel()
				r, files, objects := newCopyRig(t, requesters{})
				keepFile(t, files, objects, "reports/q3.md", "# Q3")
				turn := turnFor(t, "agent-cto")
				ref := map[string]any{"project": "ENG", "path": c.path}
				got := r.call(t, turn, door.tool, door.args(r, ref))
				read := r.call(t, turn, worklet.ReadProjectFileTool, ref)
				copied, copiedOK := wordsAfter(got, door.tool)
				said, saidOK := wordsAfter(read, worklet.ReadProjectFileTool)
				switch {
				case !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid:
					t.Errorf("answered %v %q, want invalid", tools.RefusalOf(got), got.Output)
				case !strings.Contains(got.Output, c.says) || strings.Contains(got.Output, "worklet:"):
					t.Errorf("%q does not say %q in the tracker's words", got.Output, c.says)
				case !copiedOK || !saidOK || copied != said:
					t.Errorf("the copy said %q and read_project_file %q: want the same words",
						got.Output, read.Output)
				}
				if n := tallyOf(r); n != (tally{}) {
					t.Errorf("a copy of a path the tracker refuses moved %+v", n)
				}
			})
		}
	}
}

// A COPY IS DECIDED AS read_project_file IS, by that tool's own verb: a caller
// who may not read a project's files copies none of them into an artifact,
// refused before anything is reserved — a seat in a turn somebody woke who
// does not use Worklet (the turn is held to the apps whoever woke it uses,
// ADR-0109), and a person who does not use it, calling through the operator's
// catalogue — and each copies once they use it. The same create with inline
// content lands either way: the refusal is the copy's, not the call's.
//
// Mutation: drop the copy's authority question and the refused rows land;
// leave the authority the copy asks unwired on either surface and the landing
// rows are refused as a build that decided nothing.
func TestACopyIsDecidedAsReadProjectFileIs(t *testing.T) {
	t.Parallel()
	ref := map[string]any{"project": "ENG", "path": "reports/q3.md"}
	inline := map[string]any{"kind": "html", "title": "Quarterly report", "content": "<p>Q3</p>"}
	people := []struct {
		name   string
		grants []iam.Grant
		copies bool
	}{
		{"who does not use Worklet", []iam.Grant{iam.GrantArtifacts}, false},
		{"who uses Worklet", []iam.Grant{iam.GrantArtifacts, iam.GrantWorklet}, true},
	}
	// refusedForReading reports what is wrong with got as the refusal of a
	// caller who may not read the project's files, or "".
	refusedForReading := func(got tools.Result) string {
		switch {
		case !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden:
			return fmt.Sprintf("answered %v %q, want forbidden", tools.RefusalOf(got), got.Output)
		case !errors.Is(got.Cause, builtin.ErrRefused):
			return fmt.Sprintf("the refusal carries %v, not the authority's own answer", got.Cause)
		case !strings.Contains(got.Output, worklet.ReadProjectFileTool+" didn't run"):
			return fmt.Sprintf("%q does not name the read it was refused", got.Output)
		}
		return ""
	}

	for _, person := range people {
		for _, door := range copyDoors {
			t.Run("a seat working for somebody "+person.name+"/"+door.tool, func(t *testing.T) {
				t.Parallel()
				r, files, objects := newCopyRig(t, requesters{"founder": founderPrincipal(person.grants...)})
				keepFile(t, files, objects, "reports/q3.md", "# Q3")
				got := r.call(t, asked(t), door.tool, door.args(r, ref))
				if person.copies {
					if got.Failed || tallyOf(r).writes != 1 {
						t.Fatalf("answered %v %q, writing %+v: want the copy made",
							tools.RefusalOf(got), got.Output, tallyOf(r))
					}
					return
				}
				if wrong := refusedForReading(got); wrong != "" {
					t.Error(wrong)
				}
				if n := tallyOf(r); n != (tally{}) {
					t.Errorf("a copy the authority refused moved %+v", n)
				}
				if plain := r.call(t, asked(t), builtin.CreateArtifactTool, inline); plain.Failed {
					t.Errorf("the same create with inline content was refused: %s", plain.Output)
				}
			})
		}
		t.Run("a person "+person.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeArtifacts()
			chart := artifactChart{}
			deps := artifactDeps(f, chart, requesters{})
			company := organization(t)
			deps.Org = func() *org.Organization { return company }
			files, objects := newFakeFiles(), newFakeObjects()
			keepFile(t, files, objects, "reports/q3.md", "# Q3")
			create := operatorToolOver(t, builtin.OperatorDeps{Artifacts: deps,
				Work: fileDeps(files, objects), Authorize: builtin.Decide(chart)},
				builtin.CreateArtifactTool)
			ctx := iam.WithPrincipal(context.Background(), founderPrincipal(person.grants...))
			got, err := create.Call(ctx, copyDoors[0].args(artifactRig{f: f}, ref))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			r := artifactRig{f: f}
			if person.copies {
				if got.Failed || tallyOf(r).writes != 1 {
					t.Fatalf("answered %v %q, writing %+v: want the copy made",
						tools.RefusalOf(got), got.Output, tallyOf(r))
				}
				return
			}
			if wrong := refusedForReading(got); wrong != "" {
				t.Error(wrong)
			}
			if n := tallyOf(r); n != (tally{}) {
				t.Errorf("a copy the authority refused moved %+v", n)
			}
			if plain, err := create.Call(ctx, inline); err != nil || plain.Failed {
				t.Errorf("the same create with inline content was refused: %v %s", err, plain.Output)
			}
		})
	}
}

// A COPIED FILE IS RESERVED AT ITS ROW'S DIGEST AND SIZE: the upload intent
// declares it as the tracker records it, the transfer moves exactly its bytes,
// and the create names it as an upload. Its project and its path are taken as
// read_project_file takes them (`eng`, `/reports/q3.md`). A file the artifact
// already holds is carried, and its bytes are never read: the copy opens its
// content only for a transfer the intent asks for.
//
// Mutation: declare any other digest or size and the entry check fails; open
// the content before the intent and the carried publish fails on the cleared
// store.
func TestACopiedFileIsReservedAtItsRowsDigestAndSize(t *testing.T) {
	t.Parallel()
	r, files, objects := newCopyRig(t, requesters{})
	content := "# Q3\n\nIt went well."
	keepFile(t, files, objects, "reports/q3.md", content)
	turn := turnFor(t, "agent-cto")
	got := r.call(t, turn, builtin.CreateArtifactTool, copyDoors[0].args(r,
		map[string]any{"project": "eng", "path": "/reports/q3.md"}))
	if got.Failed {
		t.Fatalf("the copy was refused: %s", got.Output)
	}
	r.f.mu.Lock()
	intents, transfers, creates := r.f.intents, r.f.transfers, r.f.creates
	r.f.mu.Unlock()
	want := objstore.HashOf([]byte(content))
	if len(intents) != 1 || len(intents[0].Entries) != 1 ||
		intents[0].Entries[0].Path != "report.md" || intents[0].Entries[0].SHA256 != want ||
		intents[0].Entries[0].Size != int64(len(content)) {
		t.Fatalf("the upload intents %+v, want report.md declared at %s over %d bytes",
			intents, want, len(content))
	}
	if moved := string(transfers["upload-1/report.md"]); moved != content {
		t.Errorf("the transfer moved %q, want the file's bytes %q", moved, content)
	}
	upload := false
	for _, spec := range creates[0].Files {
		upload = upload || (spec.Path == "report.md" && spec.Upload)
	}
	if !upload {
		t.Errorf("the create names %+v, not report.md as an upload", creates[0].Files)
	}

	// A FILE THE ARTIFACT HOLDS ALREADY, with nothing left in the store to
	// read: carried, and nothing moves.
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>Q3</p>"},
		[2]string{"report.md", content})
	objects.mu.Lock()
	clear(objects.objects)
	objects.mu.Unlock()
	got = r.call(t, turn, builtin.PublishArtifactTool, map[string]any{
		"artifact": h.ID.String(), "base_version": 1, "files": map[string]any{
			"report.md": map[string]any{"from_project_file": map[string]any{
				"project": "ENG", "path": "reports/q3.md"}}}})
	switch {
	case got.Failed:
		t.Fatalf("the publish of a file the artifact holds was refused: %s", got.Output)
	case tallyOf(r).transfers != 1:
		t.Errorf("the publish of a file the artifact holds moved its bytes again")
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	published := r.f.publishes[len(r.f.publishes)-1]
	carried := false
	for _, spec := range published.Files {
		carried = carried || (spec.Path == "report.md" && spec.Carry == want)
	}
	if published.Artifact != h.ID || !carried {
		t.Errorf("the publish names %+v, not report.md carried at %s", published, want)
	}
}

// A COPY LARGER THAN AN ARTIFACT'S LARGEST FILE IS REFUSED BEFORE ANYTHING IS
// RESERVED: `invalid`, naming the bound, from the row's size alone — the
// content is never read, and here is nowhere to be read.
//
// Mutation: drop the bound and an intent is declared, and the transfer fails
// on the missing content.
func TestACopyLargerThanAnArtifactFileIsRefusedBeforeAnythingIsReserved(t *testing.T) {
	t.Parallel()
	for _, door := range copyDoors {
		t.Run(door.tool, func(t *testing.T) {
			t.Parallel()
			r, files, _ := newCopyRig(t, requesters{})
			files.mu.Lock()
			files.files[files.key("ENG", "big.bin")] = worklet.File{Project: "ENG", Path: "big.bin",
				Hash: objstore.HashOf([]byte("big")), Size: artifacts.MaxTextFileBytes + 1,
				Object: objstore.KeyAt(fakeAt), Version: 1}
			files.mu.Unlock()
			got := r.call(t, turnFor(t, "agent-cto"), door.tool, door.args(r,
				map[string]any{"project": "ENG", "path": "big.bin"}))
			if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
				!strings.Contains(got.Output, fmt.Sprint(artifacts.MaxTextFileBytes)) {
				t.Errorf("answered %v %q, want invalid naming the bound", tools.RefusalOf(got), got.Output)
			}
			if n := tallyOf(r); n != (tally{}) {
				t.Errorf("a copy past the bound moved %+v", n)
			}
		})
	}
}

// A COPY WHERE THE COMPANY RUNS NO WORKLET IS UNAVAILABLE, AND NO WAIT CLEARS
// IT: the company's choice of tracker changes by nobody's waiting, so the
// refusal carries no wait, says why, and reserves nothing.
//
// Mutation: drop the check and the call panics on the missing reader; mark
// it as one a wait clears and the wait is offered.
func TestACopyIsUnavailableWhereTheCompanyRunsNoWorklet(t *testing.T) {
	t.Parallel()
	for _, door := range copyDoors {
		t.Run(door.tool, func(t *testing.T) {
			t.Parallel()
			r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
			got := r.call(t, turnFor(t, "agent-cto"), door.tool, door.args(r,
				map[string]any{"project": "ENG", "path": "reports/q3.md"}))
			switch {
			case !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable:
				t.Errorf("answered %v %q, want unavailable", tools.RefusalOf(got), got.Output)
			case builtin.RetryAfter(got.Cause, time.Second) != 0:
				t.Errorf("%q offers a wait, and no wait runs Worklet", got.Output)
			case !strings.Contains(got.Output, "does not run Worklet"):
				t.Errorf("%q does not say the company runs no Worklet", got.Output)
			}
			if n := tallyOf(r); n != (tally{}) {
				t.Errorf("a copy with no Worklet moved %+v", n)
			}
		})
	}
}

// CONTENT THE OBJECT STORE CANNOT HAND BACK IS TOLD AS read_project_file TELLS
// IT, never as a fault of this node and never as a file that changed: content
// the store did not serve is `unavailable`, a wait invited and the store's own
// words kept for the log; content that is not what the file records is
// `internal_error`, which no retry reads past — whether the store says so, or
// hands back other bytes of the file's length, which the copy holds to the
// row's digest before a byte of them moves. Nothing is published either way.
// Each was told through the box's teller: a fault of this node, or, for the
// other bytes, a file that "changed while it was being published", after the
// pipeline had stored them.
//
// Mutation: tell the copy's open through boxFailure again and the first two
// cases are faults; drop the digest check and the third publishes.
func TestACopyWhoseContentTheStoreCannotServeIsToldAsReadProjectFileTellsIt(t *testing.T) {
	t.Parallel()
	const content = "# Q3\n\nIt went well."
	for _, c := range []struct {
		name   string
		damage func(file worklet.File, objects *fakeObjects)
		class  tools.Refusal
		says   string
		// asRead is whether read_project_file meets the same failure, and
		// so says the same words: a page read cannot hold other bytes of
		// the file's length to its digest.
		asRead bool
	}{
		{"content the store does not hold", func(_ worklet.File, o *fakeObjects) {
			clear(o.objects)
		}, tools.RefusalUnavailable, "NOT empty or missing", true},
		{"content the store says is corrupt", func(_ worklet.File, o *fakeObjects) {
			o.corrupt = true
		}, tools.RefusalInternalError, "will not help", true},
		{"other bytes of the file's length", func(f worklet.File, o *fakeObjects) {
			o.objects[f.Object] = []byte(strings.Repeat("x", len(content)))
		}, tools.RefusalInternalError, "will not help", false},
	} {
		for _, door := range copyDoors {
			t.Run(c.name+"/"+door.tool, func(t *testing.T) {
				t.Parallel()
				r, files, objects := newCopyRig(t, requesters{})
				file := keepFile(t, files, objects, "reports/q3.md", content)
				objects.mu.Lock()
				c.damage(file, objects)
				objects.mu.Unlock()
				turn := turnFor(t, "agent-cto")
				ref := map[string]any{"project": "ENG", "path": "reports/q3.md"}
				got := r.call(t, turn, door.tool, door.args(r, ref))
				switch {
				case !got.Failed || tools.RefusalOf(got) != c.class:
					t.Fatalf("answered %v %q, want %v", tools.RefusalOf(got), got.Output, c.class)
				case !strings.Contains(got.Output, c.says):
					t.Errorf("%q does not say %q", got.Output, c.says)
				case strings.Contains(got.Output, "no such object") ||
					strings.Contains(got.Output, file.Object.String()):
					t.Errorf("the object store's own words reached the sentence: %q", got.Output)
				}
				if c.class == tools.RefusalUnavailable && (!errors.Is(got.Cause, objstore.ErrNotFound) ||
					builtin.RetryAfter(got.Cause, time.Second) == 0) {
					t.Errorf("content the store did not serve carries %v and no wait", got.Cause)
				}
				if c.asRead {
					read := r.call(t, turn, worklet.ReadProjectFileTool, ref)
					copied, copiedOK := wordsAfter(got, door.tool)
					said, saidOK := wordsAfter(read, worklet.ReadProjectFileTool)
					if !copiedOK || !saidOK || copied != said {
						t.Errorf("the copy said %q and read_project_file %q: want the same words",
							got.Output, read.Output)
					}
				}
				if n := tallyOf(r); n.transfers != 0 || n.writes != 0 {
					t.Errorf("a copy whose content could not be served moved %+v", n)
				}
			})
		}
	}
}

// A LIVE FILE NAMING NO OBJECT IS NOT COPIED: its row is one the applier
// refuses to write, so reaching it is the engine's own fault, refused in
// read_project_file's words — never a file that is gone, never a retry — and
// before anything is reserved, since there is no content to reserve.
//
// Mutation: build the object off the row's fields again and an intent is
// declared before the transfer fails in other words.
func TestALiveFileNamingNoObjectIsNotCopied(t *testing.T) {
	t.Parallel()
	for _, door := range copyDoors {
		t.Run(door.tool, func(t *testing.T) {
			t.Parallel()
			r, files, _ := newCopyRig(t, requesters{})
			files.mu.Lock()
			files.files[files.key("ENG", "odd.md")] = worklet.File{Project: "ENG", Path: "odd.md",
				Hash: objstore.HashOf([]byte("odd")), Size: 3, Version: 1}
			files.mu.Unlock()
			turn := turnFor(t, "agent-cto")
			ref := map[string]any{"project": "ENG", "path": "odd.md"}
			got := r.call(t, turn, door.tool, door.args(r, ref))
			read := r.call(t, turn, worklet.ReadProjectFileTool, ref)
			copied, copiedOK := wordsAfter(got, door.tool)
			said, saidOK := wordsAfter(read, worklet.ReadProjectFileTool)
			switch {
			case !got.Failed || tools.RefusalOf(got) != tools.RefusalInternalError ||
				!strings.Contains(got.Output, "will not help"):
				t.Errorf("answered %v %q, want internal_error saying a retry will not help",
					tools.RefusalOf(got), got.Output)
			case !copiedOK || !saidOK || copied != said:
				t.Errorf("the copy said %q and read_project_file %q: want the same words",
					got.Output, read.Output)
			}
			if n := tallyOf(r); n != (tally{}) {
				t.Errorf("a copy of a file naming no object moved %+v", n)
			}
		})
	}
}
