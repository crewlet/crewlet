package builtin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/words"
)

// WHERE AN AGENT'S FILES COME FROM: five sources, one write path.
//
// Inline text (`content`, `files[...].content`) rides the call itself, at most
// [artifacts.MaxInlineBytes] decoded across it, because anything larger cannot
// have been typed by a model and belongs in one of the other two. An executor's
// box (`from_sandbox`, `source.sandbox`) and a project's file
// (`from_project_file`) are STREAMED: their digests are taken first — a box's
// file by reading it, a project's from its row — the publish pipeline reserves
// their bytes in an upload intent, and each file is then put through the
// engine's own transfer leg, so the bytes a version names were reserved before
// they moved and are read back verified before the version is decided. A
// project file is copied rather than shared, because the object store never
// shares one object between two rows (ADR-0026). A logo or a face of the
// company's adopted brand (`from_brand`) is copied by the publish pipeline
// itself from the adoption's snapshot, named by its slot and never by a path or
// an object, so a copy reads only what an administrator adopted.
//
// # The box is walked by the engine, and never uploads
//
// A sandbox is never handed a URL or a credential: the tool, running on the
// node that holds the seat and therefore the run's box, lists the directory
// through the box itself, treats the listing as untrusted, holds every path to
// the workspace and the published-path grammar again, and streams each file
// out. One more file than a version holds is a refusal, never a truncation.
//
// # A project's file is read as read_project_file reads it
//
// Through the one copy read beside that tool ([WorkDeps.copyFrom]): decided by
// its verb, its path held to the tracker's rule, and every refusal in its words
// ([fileReadFailure], [fileContent], [fileContentFailure]), so a seat told why
// a copy failed is told what the read would have told it. One thing differs,
// the class of a project or a file that is not there: the caller's argument to
// change, since a copy only NAMES its source, where the read is ABOUT the file
// and answers not-found. Its bytes are read whole, at the transfer, and held
// to the row's digest before any of them moves: a page read can be held only
// to its length ([objstore.Store.ReadAt]), a whole read to the digest, so
// content that is not the file's is refused as the damage it is before a byte
// of it is stored. The publish pipeline's own check would catch it only after
// storing it, and answer it as a file that changed while it was being
// published: a conflict, which invites a retry that reads the same bytes.

// artifactFile is one path of the next version and where its bytes come from:
// exactly one of inline, stream, brand and remove.
type artifactFile struct {
	path   string
	inline []byte
	stream *streamedFile
	brand  brandformat.Slot
	remove bool
}

// streamedFile is a file whose bytes are streamed into an upload: its digest
// and size taken first, and a way to open it again for the transfer.
type streamedFile struct {
	hash objstore.Hash
	size int64
	open func(ctx context.Context) (io.ReadCloser, error)
	// from says where it was read, for a refusal that names it.
	from string
	// unread is how a failed open of it is told, by the read its source is:
	// a box's by why the box failed ([boxFailure]), a project file's as
	// read_project_file tells a file's content ([fileContentFailure]), and
	// the call's own inline bytes by [nodeFailure], which no open of them
	// reaches. Every constructor sets it.
	unread func(ctx context.Context, err error) tools.Result
}

// fileSources is one call's files as it parses them, with the box it read
// from opened at most once.
type fileSources struct {
	deps   ArtifactDeps
	caller artifactCaller
	tool   string

	files  []artifactFile
	inline int64
	box    ArtifactBox
}

func newFileSources(d ArtifactDeps, c artifactCaller, tool string) *fileSources {
	return &fileSources{deps: d, caller: c, tool: tool}
}

// add appends one file, refusing a path named twice.
func (s *fileSources) add(f artifactFile) *tools.Result {
	for _, held := range s.files {
		if held.path == f.path {
			r := failed(fmt.Sprintf("%s names %q twice: say each file once.", s.tool, f.path))
			return &r
		}
	}
	s.files = append(s.files, f)
	return nil
}

// cleanPath is a published path as the caller wrote it, or the refusal.
func (s *fileSources) cleanPath(raw string) (string, *tools.Result) {
	p, err := manifest.CleanPath(raw)
	if err != nil {
		r := refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s: %q is not a path an "+
			"artifact may publish: %s", s.tool, clip(raw), artifactupload.Sentence(err)))
		return "", &r
	}
	return p, nil
}

// addInline appends text or base64 content the call carries.
func (s *fileSources) addInline(p, content, encoding string) *tools.Result {
	var data []byte
	switch encoding {
	case "", "text":
		// WITHOUT THE FENCE a body copied from a read still wears.
		data = []byte(unfence(s.caller.turn, content))
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			r := failed(fmt.Sprintf("%s: the content of %q is not valid base64: %v",
				s.tool, p, err))
			return &r
		}
		data = decoded
	default:
		r := failed(fmt.Sprintf("%s takes `encoding` as `text` or `base64`, and %q "+
			"says %q.", s.tool, p, clip(encoding)))
		return &r
	}
	s.inline += int64(len(data))
	if s.inline > artifacts.MaxInlineBytes {
		r := failed(fmt.Sprintf("%s carries more than %d bytes of inline content. Build "+
			"larger files in your box and publish them with `from_sandbox`, or "+
			"keep them as project files and use `from_project_file`.", s.tool,
			artifacts.MaxInlineBytes))
		return &r
	}
	return s.add(artifactFile{path: p, inline: data})
}

// parseFiles reads the `files` argument: a map of published paths to a
// source, or to null (patch mode only) to remove the path.
func (s *fileSources) parseFiles(ctx context.Context, args map[string]any, patch bool) *tools.Result {
	raw, present := args["files"]
	if !present || raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		r := failed(s.tool + " takes `files` as an object of published paths, each to " +
			"`{content}`, `{from_sandbox}`, `{from_project_file}` or `{from_brand}`.")
		return &r
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		p, refusal := s.cleanPath(k)
		if refusal != nil {
			return refusal
		}
		v := m[k]
		if v == nil {
			if !patch {
				r := failed(fmt.Sprintf("%s: %q is null, which removes a file, and a new "+
					"artifact has nothing to remove.", s.tool, p))
				return &r
			}
			if refusal := s.add(artifactFile{path: p, remove: true}); refusal != nil {
				return refusal
			}
			continue
		}
		spec, ok := v.(map[string]any)
		if !ok {
			r := failed(fmt.Sprintf("%s: %q must be `{content}`, `{from_sandbox}`, "+
				"`{from_project_file}`, `{from_brand}` or null.", s.tool, p))
			return &r
		}
		if refusal := s.parseSpec(ctx, p, spec); refusal != nil {
			return refusal
		}
	}
	return nil
}

func (s *fileSources) parseSpec(ctx context.Context, p string, spec map[string]any) *tools.Result {
	for field := range spec {
		switch field {
		case "content", "encoding", "from_sandbox", "from_project_file", "from_brand":
		default:
			r := failed(fmt.Sprintf("%s: %q carries %q, and a file takes `content` (with "+
				"an optional `encoding`), `from_sandbox`, `from_project_file` or `from_brand`.",
				s.tool, p, field))
			return &r
		}
	}
	content, hasContent := spec["content"]
	box, hasBox := spec["from_sandbox"]
	project, hasProject := spec["from_project_file"]
	brand, hasBrand := spec["from_brand"]
	n := 0
	for _, has := range []bool{hasContent, hasBox, hasProject, hasBrand} {
		if has {
			n++
		}
	}
	if n != 1 {
		r := failed(fmt.Sprintf("%s: %q must name exactly one of `content`, "+
			"`from_sandbox`, `from_project_file` and `from_brand`.", s.tool, p))
		return &r
	}
	switch {
	case hasBrand:
		raw, _ := argval.String(brand)
		slot, refusal := s.brandSlot(p, raw)
		if refusal != nil {
			return refusal
		}
		return s.add(artifactFile{path: p, brand: slot})
	case hasContent:
		text, ok := argval.String(content)
		if !ok || content == nil {
			r := failed(fmt.Sprintf("%s: the content of %q must be a string.", s.tool, p))
			return &r
		}
		encoding, _ := argval.String(spec["encoding"])
		return s.addInline(p, text, strings.TrimSpace(encoding))
	case hasBox:
		from, _ := argval.String(box)
		f, refusal := s.boxFile(ctx, p, from)
		if refusal != nil {
			return refusal
		}
		return s.add(f)
	}
	ref, _ := project.(map[string]any)
	f, refusal := s.projectFile(ctx, p, ref)
	if refusal != nil {
		return refusal
	}
	return s.add(f)
}

// brandSlot is a `from_brand` value: a logo's slot, a face's (its regular
// file), or one file of an uploaded family (`font.heading.1`), or the refusal
// naming the slots there are. Whether the adopted brand holds it is the
// pipeline's to answer, from the adoption it copies.
func (s *fileSources) brandSlot(p, raw string) (brandformat.Slot, *tools.Result) {
	slot := brandformat.Slot(strings.TrimSpace(raw))
	if slices.Contains(brandformat.SourceSlots, slot) {
		return slot, nil
	}
	for _, face := range []brandformat.Slot{brandformat.SlotFontHeading, brandformat.SlotFontBody} {
		for i := range brandformat.MaxFontFilesPerFamily {
			if slot == brandformat.FontFileSlot(face, i) {
				return slot, nil
			}
		}
	}
	names := make([]string, len(brandformat.SourceSlots))
	for i, v := range brandformat.SourceSlots {
		names[i] = string(v)
	}
	r := failed(fmt.Sprintf("%s: %q copies `from_brand` %q, which is no slot of the brand; "+
		"take one of %s ("+CompanyBrandTool+" lists what your company's brand holds).", s.tool, p,
		clip(raw), words.Or(names)))
	return "", &r
}

// openBox reattaches the box this turn's executor runs in, once per call.
func (s *fileSources) openBox(ctx context.Context) (ArtifactBox, *tools.Result) {
	if s.box != nil {
		return s.box, nil
	}
	if s.caller.turn == nil || !s.caller.agent() {
		r := refused(tools.RefusalForbidden, s.tool+" reads `from_sandbox` from the box "+
			"a seat's executor ran in during its own turn, and this call has no such turn. "+
			"Publish the files inline, or from a project file.")
		return nil, &r
	}
	if s.deps.Boxes == nil {
		r := refusedBy(tools.RefusalUnavailable, waitless(), s.tool+" cannot read an executor's box: "+
			"this node runs no sandbox. Publish the files inline instead.")
		return nil, &r
	}
	box, err := s.deps.Boxes.Box(ctx, s.caller.turn)
	switch {
	case errors.Is(err, ErrNoBox):
		r := refusedBy(tools.RefusalNotRunning, err, s.tool+" found no live box "+
			"to read in this turn: a box is readable by the executor working in it, while "+
			"it runs, and is torn down after it. Build into a directory in your workspace, "+
			"then publish from it before you finish.")
		return nil, &r
	case err != nil:
		r := boxFailure(ctx, s.tool, err, "Could not reattach this turn's box")
		return nil, &r
	}
	s.box = box
	return box, nil
}

// boxPath is a path in the box as the caller wrote it, held to the workspace:
// relative, cleaned, and never above the workspace root.
func (s *fileSources) boxPath(raw string) (string, *tools.Result) {
	p := strings.TrimSpace(raw)
	p = strings.TrimPrefix(p, "./")
	clean := path.Clean(p)
	if p == "" || strings.HasPrefix(p, "/") || clean == ".." || strings.HasPrefix(clean, "../") ||
		strings.ContainsAny(p, "\x00\n\r") {
		r := failed(fmt.Sprintf("%s: %q is not a path inside your workspace. "+
			"Name it relative to the workspace, e.g. `dist/index.html`.", s.tool, clip(raw)))
		return "", &r
	}
	return clean, nil
}

// boxFile is one file of the box, its digest read.
func (s *fileSources) boxFile(ctx context.Context, published, raw string) (artifactFile, *tools.Result) {
	from, refusal := s.boxPath(raw)
	if refusal != nil {
		return artifactFile{}, refusal
	}
	box, refusal := s.openBox(ctx)
	if refusal != nil {
		return artifactFile{}, refusal
	}
	stream, refusal := s.digest(ctx, "sandbox path "+from,
		func(ctx context.Context) (io.ReadCloser, error) { return box.Open(ctx, from) })
	if refusal != nil {
		return artifactFile{}, refusal
	}
	return artifactFile{path: published, stream: stream}, nil
}

// addBoxDir appends every regular file under dir in the box, each published
// at its path below dir: the whole file set of a build.
func (s *fileSources) addBoxDir(ctx context.Context, raw string) *tools.Result {
	dir, refusal := s.boxPath(raw)
	if refusal != nil {
		return refusal
	}
	box, refusal := s.openBox(ctx)
	if refusal != nil {
		return refusal
	}
	listed, err := box.Files(ctx, dir, artifacts.MaxFilesPerVersion+1)
	switch {
	case errors.Is(err, ErrNotADirectory):
		// THE CALLER'S PATH, as a file's is ([fileSources.digest]): it was
		// told as a fault of this node, and the model gave up on a publish
		// one corrected argument would have made.
		r := failed(fmt.Sprintf("%s: %s isn't a directory in your workspace: "+
			"it's missing, or it's a file or a link. Name the directory the build wrote its "+
			"output to. Nothing was published.", s.tool, dir))
		return &r
	case errors.Is(err, ErrNoWorkspace):
		// NO DIR WOULD LIST, so naming another is no advice: the box has to
		// hold a build again before anything in it can be published.
		r := refusedBy(tools.RefusalNotRunning, err, s.tool+": your workspace "+
			"is gone from its box, so nothing in it can be published. Build it again, "+
			"then publish from it before you finish.")
		return &r
	case errors.Is(err, ErrListIncomplete):
		// A WALK THAT STOPPED PARTWAY is not the build: published, it shipped
		// whatever the box could read and dropped the rest without a word.
		r := refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s: %s couldn't be listed "+
			"whole in your box: part of it can't be read, most often a "+
			"directory without read permission. Nothing was published. Make the build's "+
			"output readable, then publish it again.", s.tool, dir))
		return &r
	case err != nil:
		r := boxFailure(ctx, s.tool, err, "Could not list "+dir+" in this turn's box")
		return &r
	}
	switch {
	case len(listed) == 0:
		r := failed(fmt.Sprintf("%s found no files under %q in your box. "+
			"Check where the build wrote its output.", s.tool, dir))
		return &r
	case len(listed) > artifacts.MaxFilesPerVersion:
		r := failed(fmt.Sprintf("%s found more than %d files under %q, and a version "+
			"holds at most %d. Publish the build's output directory alone.", s.tool,
			artifacts.MaxFilesPerVersion, dir, artifacts.MaxFilesPerVersion))
		return &r
	}
	prefix := dir + "/"
	if dir == "." {
		prefix = ""
	}
	slices.Sort(listed)
	for _, got := range listed {
		// THE LISTING IS THE BOX'S, AND UNTRUSTED: every path it names is
		// held to the directory asked for and to the published-path
		// grammar again, and one that is not is refused by name rather
		// than skipped.
		if path.Clean(got) != got || !strings.HasPrefix(got, prefix) {
			r := refused(tools.RefusalInvalid, fmt.Sprintf("%s: the box listed %q under "+
				"%q, which is not a path inside it. Nothing was published.", s.tool,
				clip(got), dir))
			return &r
		}
		published, refusal := s.cleanPath(strings.TrimPrefix(got, prefix))
		if refusal != nil {
			return refusal
		}
		from := got
		stream, refusal := s.digest(ctx, "sandbox path "+from,
			func(ctx context.Context) (io.ReadCloser, error) { return box.Open(ctx, from) })
		if refusal != nil {
			return refusal
		}
		if refusal := s.add(artifactFile{path: published, stream: stream}); refusal != nil {
			return refusal
		}
	}
	return nil
}

// digest reads a streamed file once for its digest and size, refusing one
// past the largest file an artifact holds before any byte is reserved.
func (s *fileSources) digest(ctx context.Context, from string,
	open func(ctx context.Context) (io.ReadCloser, error)) (*streamedFile, *tools.Result) {

	unread := func(ctx context.Context, err error) tools.Result {
		return boxFailure(ctx, s.tool, err, "Could not read "+from)
	}
	rc, err := open(ctx)
	if err != nil {
		// A PATH THAT NAMES NO FILE is the caller's to change: a box streams
		// only a regular file, and one that is missing, a directory or a
		// link is refused by name. It was told as a fault of this node.
		if errors.Is(err, sandbox.ErrNotRegularFile) {
			r := failed(fmt.Sprintf("%s: %s isn't a file in your workspace: "+
				"it's missing, or it's a directory or a link. Name a file written there. "+
				"Nothing was published.", s.tool, from))
			return nil, &r
		}
		r := unread(ctx, err)
		return nil, &r
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, artifacts.MaxTextFileBytes+1))
	if err != nil {
		r := unread(ctx, err)
		return nil, &r
	}
	if n > artifacts.MaxTextFileBytes {
		r := failed(fmt.Sprintf("%s: %s is larger than the %d bytes an artifact's "+
			"largest file may hold.", s.tool, from, artifacts.MaxTextFileBytes))
		return nil, &r
	}
	hash, err := objstore.ParseHash(hex.EncodeToString(h.Sum(nil)))
	if err != nil {
		r := faulted(ctx, s.tool, err, s.tool+" could not name the digest of "+from+": "+faultSaid+".")
		return nil, &r
	}
	return &streamedFile{hash: hash, size: n, open: open, from: from, unread: unread}, nil
}

// projectFile is a project's file, copied, decided and read as
// read_project_file reads it ([WorkDeps.copyFrom]): its digest and size are
// the row's, and its bytes are read whole and held to the row's digest when
// the transfer opens them.
func (s *fileSources) projectFile(ctx context.Context, published string,
	ref map[string]any) (artifactFile, *tools.Result) {

	project := strings.TrimSpace(argString(ref, "project"))
	from := strings.TrimSpace(argString(ref, "path"))
	if project == "" || from == "" {
		r := failed(fmt.Sprintf("%s: `from_project_file` for %q takes `{project, path}`.",
			s.tool, published))
		return artifactFile{}, &r
	}
	src, refusal := s.deps.work.copyFrom(ctx, s.tool, project, from)
	if refusal != nil {
		return artifactFile{}, refusal
	}
	obj := src.object
	if obj.Size > artifacts.MaxTextFileBytes {
		r := failed(fmt.Sprintf("%s: project file %s/%s is larger than the %d bytes an "+
			"artifact's largest file may hold.", s.tool, src.project, src.filePath,
			artifacts.MaxTextFileBytes))
		return artifactFile{}, &r
	}
	objects := s.deps.work.Objects
	open := func(ctx context.Context) (io.ReadCloser, error) {
		data, err := objects.ReadAt(ctx, obj, 0, obj.Size)
		if err != nil {
			return nil, err
		}
		// THE WHOLE FILE IS READ, so it is held to the row's digest here,
		// as a page read cannot be ([objstore.Store.ReadAt] checks a
		// length): bytes that are not the file's never reach the upload.
		if got := objstore.HashOf(data); got != obj.Hash {
			return nil, fmt.Errorf("%w: %s hashes to %s, and its row says %s",
				objstore.ErrCorrupt, obj.Key, got, obj.Hash)
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return artifactFile{path: published, stream: &streamedFile{hash: obj.Hash, size: obj.Size,
		open: open, from: "project file " + src.project + "/" + src.filePath,
		unread: func(ctx context.Context, err error) tools.Result {
			return fileContentFailure(ctx, s.tool, src.file, err)
		}}}, nil
}

// stage reserves and transfers every streamed file of the call through the
// publish pipeline, and answers the file specs the publish names: the upload
// the bytes came through, and each path by its source.
func (s *fileSources) stage(ctx context.Context, call artifactsdomain.Call, id artifacts.ID,
	asset bool) (string, []artifactupload.FileSpec, *tools.Result) {

	var entries []artifacts.UploadEntry
	var total int64
	for _, f := range s.files {
		if f.stream == nil {
			continue
		}
		total += f.stream.size
		entries = append(entries, artifacts.UploadEntry{Path: f.path, SHA256: f.stream.hash,
			Size: f.stream.size})
	}
	if total > artifacts.MaxPublishBytes {
		r := failed(fmt.Sprintf("%s would move %d bytes, and a publish uploads at most "+
			"%d. Publish fewer or smaller files.", s.tool, total, artifacts.MaxPublishBytes))
		return "", nil, &r
	}
	carried := map[string]bool{}
	var upload string
	if len(entries) > 0 {
		intent, err := s.deps.Publisher.Intend(ctx, call, id,
			artifacts.UploadRequest{Entries: entries, Asset: asset})
		if err != nil {
			r := publishFailure(ctx, s.tool, s.caller, err)
			return "", nil, &r
		}
		upload = intent.Upload
		for _, p := range intent.Carried {
			carried[p] = true
		}
		for _, tr := range intent.Transfers {
			f, ok := s.find(tr.Path)
			if !ok || f.stream == nil {
				r := faulted(ctx, s.tool, fmt.Errorf("builtin: the upload names %q, "+
					"which this call did not declare", tr.Path), s.tool+" could not match "+
					"its upload to its files: "+faultSaid+".")
				return "", nil, &r
			}
			if refusal := s.transfer(ctx, call, upload, f); refusal != nil {
				return "", nil, refusal
			}
		}
	}
	specs := make([]artifactupload.FileSpec, 0, len(s.files))
	for _, f := range s.files {
		switch {
		case f.remove:
			specs = append(specs, artifactupload.FileSpec{Path: f.path, Remove: true})
		case f.brand != "":
			// THE PIPELINE COPIES IT from the adopted brand's snapshot: no
			// byte of it rides this call or its upload.
			specs = append(specs, artifactupload.FileSpec{Path: f.path, Brand: f.brand})
		case f.stream == nil:
			specs = append(specs, artifactupload.FileSpec{Path: f.path, Inline: f.inline})
		case carried[f.path]:
			// THE ARTIFACT HOLDS THESE BYTES ALREADY: the version names them
			// again and nothing moves.
			specs = append(specs, artifactupload.FileSpec{Path: f.path, Carry: f.stream.hash})
		default:
			specs = append(specs, artifactupload.FileSpec{Path: f.path, Upload: true})
		}
	}
	return upload, specs, nil
}

// transfer streams one file's bytes through the engine's transfer leg. A file
// already stored by an earlier attempt of the same operation is the same
// file, and moves nothing again.
func (s *fileSources) transfer(ctx context.Context, call artifactsdomain.Call, upload string,
	f artifactFile) *tools.Result {

	rc, err := f.stream.open(ctx)
	if err != nil {
		r := f.stream.unread(ctx, err)
		return &r
	}
	defer func() { _ = rc.Close() }()
	err = s.deps.Publisher.Transfer(ctx, call, upload, f.path, rc, f.stream.size)
	var mismatch *artifactupload.UploadMismatch
	switch {
	case err == nil, errors.Is(err, artifactupload.ErrUploadConsumed):
		return nil
	case errors.As(err, &mismatch):
		// A BOX'S FILE MOVED UNDER THE PUBLISH. A project file's bytes are
		// held to its row's digest by its own open, and the call's own bytes
		// cannot change, so no other source meets this arm.
		r := refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s: %s changed while it "+
			"was being published (%s). Nothing was published; publish again once the "+
			"build has finished writing it.", s.tool, f.stream.from, mismatch.Reason))
		return &r
	}
	r := publishFailure(ctx, s.tool, s.caller, err)
	return &r
}

func (s *fileSources) find(p string) (artifactFile, bool) {
	for _, f := range s.files {
		if f.path == p {
			return f, true
		}
	}
	return artifactFile{}, false
}

// streamedInline is an inline file as a stream of its own bytes, for the one
// write that takes no inline content (an asset is always its own upload).
func (s *fileSources) streamedInline(f artifactFile) artifactFile {
	if f.inline == nil {
		return f
	}
	data, from := f.inline, "the inline content of "+f.path
	f.stream = &streamedFile{hash: objstore.HashOf(data), size: int64(len(data)),
		open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}, from: from,
		// THE CALL'S OWN BYTES, in memory: an open of them never fails, and
		// one that did would be this node's own.
		unread: func(ctx context.Context, err error) tools.Result {
			return nodeFailure(ctx, s.tool, err, "Could not read "+from, "Nothing was published.")
		}}
	f.inline = nil
	return f
}

// boxFailure is a read of a publish's source that failed, nothing having been
// published, as a model is told it: by the class the sandbox backend marked the failure with
// ([sandbox.WhyOf]) — the box's provider not answering, at a limit, refusing
// the company's key, or a setup that cannot reach the box — and otherwise as
// the condition or fault of this node it is ([nodeFailure]). A project file's
// content is not read here: it is told as read_project_file tells it
// ([fileContentFailure]).
//
// A PROVIDER'S OUTAGE IS NOT THIS NODE'S FAULT. Every such failure was told as
// one, "not something trying again clears", so a publish whose box E2B was
// slow to answer was abandoned by a model told that waiting would not help.
// The error itself goes to the node's log here, as a fault's does in
// [faulted].
func boxFailure(ctx context.Context, tool string, err error, stopped string) tools.Result {
	const fact = "Nothing was published."
	why, classed := sandbox.WhyOf(err)
	switch {
	case ctx.Err() != nil:
		// The call's own context ended: told as that, whatever the
		// provider's request it ended said.
		log.WarnContext(ctx, "builtin_box_read_failed", "tool", tool, "error", err.Error())
		return nodeFailure(ctx, tool, ctx.Err(), stopped, fact)
	case !classed:
		// A CONDITION IS WRITTEN HERE: [nodeFailure] logs only a fault, so a
		// box read a coordination store or a deadline stopped left nothing
		// in the log that said what stopped it.
		if _, condition := Condition(err); condition {
			log.WarnContext(ctx, "builtin_box_read_failed", "tool", tool, "error", err.Error())
		}
		return nodeFailure(ctx, tool, err, stopped, fact)
	}
	log.WarnContext(ctx, "builtin_box_read_failed", "tool", tool, "why", string(why),
		"error", err.Error())
	switch why {
	case sandbox.WhyProviderUnavailable:
		return refusedBy(tools.RefusalUnavailable, waits(), sentence(stopped+" (the executor's "+
			"box didn't answer, or its provider failed on its side).", fact,
			"Try again shortly."))
	case sandbox.WhyProviderLimit:
		return refusedBy(tools.RefusalUnavailable, waits(), sentence(stopped+" (the sandbox "+
			"provider is at a limit of the company's account).", fact, "Try again later."))
	case sandbox.WhyProviderBilling:
		return refusedBy(tools.RefusalUnavailable, waitless(), sentence(stopped+" (the "+
			"sandbox provider's account for the company needs paying for, and waiting "+
			"won't change that).", fact, "Tell whoever asked that an administrator needs "+
			"to check the sandbox provider's account."))
	case sandbox.WhyProviderRefused:
		return refusedBy(tools.RefusalUnavailable, waitless(), sentence(stopped+" (the "+
			"sandbox provider refused the company's credentials, and waiting won't change "+
			"that).", fact, "Tell whoever asked that an administrator needs to fix the "+
			"sandbox provider's credentials."))
	case sandbox.WhyConfiguration:
		return refusedBy(tools.RefusalUnavailable, waitless(), sentence(stopped+" (the "+
			"sandbox isn't set up so this node can reach the box, and waiting won't change "+
			"that).", fact, "Tell whoever asked that an administrator needs to fix the "+
			"sandbox setup; the node's log says what."))
	}
	// A class no box read meets (setup_unavailable, a launch's own: its setup
	// step's download failing for now) is told by the classification every
	// tool shares ([Condition]), which words every class there is.
	return nodeFailure(ctx, tool, err, stopped, fact)
}
