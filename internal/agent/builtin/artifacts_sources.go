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
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/worklet"
)

// WHERE AN AGENT'S FILES COME FROM: five sources, one write path.
//
// Inline text (`content`, `files[...].content`) rides the call itself, at most
// [artifacts.MaxInlineBytes] decoded across it, because anything larger cannot
// have been typed by a model and belongs in one of the other two. A coding
// run's box (`from_sandbox`, `source.sandbox`) and a project's file
// (`from_project_file`) are STREAMED: their digests are read first, the
// publish pipeline reserves their bytes in an upload intent, and each file is
// then put through the engine's own transfer leg, so the bytes a version names
// were reserved before they moved and are read back verified before the
// version is decided. A project file is copied rather than shared, because the
// object store never shares one object between two rows (ADR-0026). A logo or a
// face of the company's adopted brand (`from_brand`) is copied by the publish
// pipeline itself from the adoption's snapshot, named by its slot and never by
// a path or an object, so a copy reads only what an administrator adopted.
//
// # The box is walked by the engine, and never uploads
//
// A sandbox is never handed a URL or a credential: the tool, running on the
// node that holds the seat and therefore the run's box, lists the directory
// through the box itself, treats the listing as untrusted, holds every path to
// the workspace and the published-path grammar again, and streams each file
// out. One more file than a version holds is a refusal, never a truncation.

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
// and size read first, and a way to open it again for the transfer.
type streamedFile struct {
	hash objstore.Hash
	size int64
	open func(ctx context.Context) (io.ReadCloser, error)
	// from says where it was read, for a refusal that names it.
	from string
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
			"artifact may publish: %s", s.tool, clip(raw), domainSentence(err)))
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
			"larger files with your coding run and publish them with `from_sandbox`, or "+
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
		clip(raw), strings.Join(names, ", ")))
	return "", &r
}

// openBox reattaches the box of the coding run this turn launched, once per
// call.
func (s *fileSources) openBox(ctx context.Context) (ArtifactBox, *tools.Result) {
	if s.box != nil {
		return s.box, nil
	}
	if s.caller.turn == nil || !s.caller.agent() {
		r := refused(tools.RefusalForbidden, s.tool+" reads `from_sandbox` from the box "+
			"a seat's coding run used in its own turn, and this call has no such turn. "+
			"Publish the files inline, or from a project file.")
		return nil, &r
	}
	if s.deps.Boxes == nil {
		r := refused(tools.RefusalUnavailable, s.tool+" cannot read a coding run's box: "+
			"this node runs no sandbox. Publish the files inline instead.")
		return nil, &r
	}
	box, err := s.deps.Boxes.Box(ctx, s.caller.turn)
	switch {
	case errors.Is(err, ErrNoBox):
		r := refusedBy(tools.RefusalNotRunning, err, s.tool+" found no coding run's box "+
			"to read in this turn: a box is readable after "+RunSandboxTool+" returns and until "+
			"this Execute phase ends, and is torn down after it. Ask the coding run to "+
			"build into a directory with "+RunSandboxTool+", then publish from it in the same turn.")
		return nil, &r
	case err != nil:
		r := nodeFailure(ctx, s.tool, err, "Could not reattach the coding run's box",
			"Nothing was published.")
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
		r := failed(fmt.Sprintf("%s: %q is not a path inside your coding run's workspace. "+
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
	if err != nil {
		r := nodeFailure(ctx, s.tool, err, "Could not list "+dir+" in the coding run's box",
			"Nothing was published.")
		return &r
	}
	switch {
	case len(listed) == 0:
		r := failed(fmt.Sprintf("%s found no files under %q in your coding run's box. "+
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

	rc, err := open(ctx)
	if err != nil {
		r := nodeFailure(ctx, s.tool, err, "Could not read "+from, "Nothing was published.")
		return nil, &r
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, artifacts.MaxTextFileBytes+1))
	if err != nil {
		r := nodeFailure(ctx, s.tool, err, "Could not read "+from, "Nothing was published.")
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
	return &streamedFile{hash: hash, size: n, open: open, from: from}, nil
}

// projectFile is a project's file, copied: its digest and size are the row's,
// and its bytes are read back through the object store's verifying reader.
func (s *fileSources) projectFile(ctx context.Context, published string,
	ref map[string]any) (artifactFile, *tools.Result) {

	project := worklet.ProjectKey(strings.TrimSpace(argString(ref, "project")))
	from := strings.TrimSpace(argString(ref, "path"))
	if project == "" || from == "" {
		r := failed(fmt.Sprintf("%s: `from_project_file` for %q takes `{project, path}`.",
			s.tool, published))
		return artifactFile{}, &r
	}
	work := s.deps.work
	if work.Files == nil || work.Objects == nil {
		r := refused(tools.RefusalUnavailable, s.tool+" cannot read project files: this "+
			"company does not run Worklet.")
		return artifactFile{}, &r
	}
	// READING THE FILE IS read_project_file's VERB, asked as that tool's own
	// gate asks it: a caller who may not read a project's files may not copy
	// one into an artifact either.
	if refusal := askAuthority(ctx, s.deps.authorize, authz.Action(worklet.ReadProjectFileTool),
		authz.Object{}); refusal != nil {
		return artifactFile{}, refusal
	}
	detail, err := work.Files.File(ctx, project, from, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoFile):
		r := refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s: project %s has no "+
			"file %q to copy into %q.", s.tool, project, clip(from), published))
		return artifactFile{}, &r
	case err != nil:
		r := readFailure(ctx, s.tool, err)
		return artifactFile{}, &r
	}
	obj := objstore.Object{Key: detail.File.Object, Hash: detail.File.Hash, Size: detail.File.Size}
	if detail.File.Size > artifacts.MaxTextFileBytes {
		r := failed(fmt.Sprintf("%s: project file %s/%s is larger than the %d bytes an "+
			"artifact's largest file may hold.", s.tool, project, from,
			artifacts.MaxTextFileBytes))
		return artifactFile{}, &r
	}
	objects := work.Objects
	open := func(ctx context.Context) (io.ReadCloser, error) {
		data, err := objects.ReadAt(ctx, obj, 0, obj.Size)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return artifactFile{path: published, stream: &streamedFile{hash: obj.Hash, size: obj.Size,
		open: open, from: "project file " + project + "/" + from}}, nil
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
		r := nodeFailure(ctx, s.tool, err, "Could not read "+f.stream.from, "Nothing was published.")
		return &r
	}
	defer func() { _ = rc.Close() }()
	err = s.deps.Publisher.Transfer(ctx, call, upload, f.path, rc, f.stream.size)
	var mismatch *artifactupload.UploadMismatch
	switch {
	case err == nil, errors.Is(err, artifactupload.ErrUploadConsumed):
		return nil
	case errors.As(err, &mismatch):
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

// domainSentence is a refusal the artifacts packages composed for a caller,
// without the package names their errors carry — the tracker's among them,
// because a version's paths are held to the engine's one file-path rule
// ([worklet.NormalizeFilePath], which manifest.CleanPath wraps), so a path
// refused there read "invalid: tracker: the path …".
func domainSentence(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"artifactupload: ", "artifactsdomain: ", "manifest: ",
		"mediatype: ", "lint: ", "edits: ", "artifacts: ", "tracker: "} {
		msg = strings.ReplaceAll(msg, prefix, "")
	}
	return msg
}

// streamedInline is an inline file as a stream of its own bytes, for the one
// write that takes no inline content (an asset is always its own upload).
func streamedInline(f artifactFile) artifactFile {
	if f.inline == nil {
		return f
	}
	data := f.inline
	f.stream = &streamedFile{hash: objstore.HashOf(data), size: int64(len(data)),
		open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}, from: "the inline content of " + f.path}
	f.inline = nil
	return f
}
