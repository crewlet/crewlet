package speech

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/nativelib"
)

// Model is one pinned recognizer model: each file it loads, pinned by its
// SHA-256 and size, and where that file is published. A model directory holds
// the files under their names, whoever wrote it.
type Model struct {
	// Name is the model's directory in every directory that holds it.
	Name string

	Files []ModelFile
}

// ModelFile is one file the recognizer loads.
type ModelFile struct {
	Role   Role
	Name   string
	SHA256 string
	Size   int64

	// URL fetches the file itself, pinned to a commit. Empty for a file
	// upstream publishes only inside an archive: From, at Member.
	URL    string
	From   *ModelArchive
	Member string
}

// ModelArchive is an official release archive (tar, bzip2): the native
// library's, and a model file's that is published no other way.
type ModelArchive struct {
	URL    string
	SHA256 string
	// Size is the archive's length: the progress a download reports, and
	// the most it reads.
	Size int64
}

// Role is what the recognizer loads a model file as.
type Role string

// The roles of a streaming transducer with a punctuation and casing model.
const (
	RoleEncoder          Role = "encoder"
	RoleDecoder          Role = "decoder"
	RoleJoiner           Role = "joiner"
	RoleTokens           Role = "tokens"
	RolePunctuation      Role = "punctuation"
	RolePunctuationVocab Role = "punctuation_vocab"
)

// roles are every role a [Model] must fill.
var roles = []Role{RoleEncoder, RoleDecoder, RoleJoiner, RoleTokens, RolePunctuation, RolePunctuationVocab}

const zipformer = "https://huggingface.co/csukuangfj/sherpa-onnx-streaming-zipformer-en-2023-06-21/resolve/" +
	"9a65b6ea94c311ca770c2bf895b30f456a22d703/"

// punctuation is the English punctuation and casing model's sherpa-onnx
// release: upstream publishes its files one by one nowhere (Hugging Face has
// them only as a 7z), so the archive's two int8 files are kept of its 30.7 MB.
var punctuation = &ModelArchive{
	URL:    "https://github.com/k2-fsa/sherpa-onnx/releases/download/punctuation-models/sherpa-onnx-online-punct-en-2024-08-06.tar.bz2",
	SHA256: "9f5e5a72c7d2829635bd074fce92b6bbd5b78da8a52e7ad8ed1be933f366b99d",
	Size:   30667839,
}

// DefaultModel is k2-fsa's streaming Zipformer transducer trained on
// LibriSpeech and GigaSpeech (Apache-2.0), int8, from its Hugging Face
// repository at a pinned commit, with frankyoujian's English punctuation and
// casing model (Apache-2.0). Measured on the dictation bench: 2.3% WER, first
// partial 0.59 s, a partial every 0.3 s, 0.19 of a core per stream.
var DefaultModel = Model{
	Name: "zipformer-en-2023-06-21",
	Files: []ModelFile{
		{Role: RoleEncoder, Name: "encoder-epoch-99-avg-1.int8.onnx", Size: 187823992,
			SHA256: "32c98281c7bd8b63e3e142d007251b37f120572e8fdea9a4f5a79ce22b10ec4f",
			URL:    zipformer + "encoder-epoch-99-avg-1.int8.onnx"},
		{Role: RoleDecoder, Name: "decoder-epoch-99-avg-1.int8.onnx", Size: 539246,
			SHA256: "093e23c90869898f761f60aa3363f96d43b9c6e5c06a57860c3a5b3407ab8320",
			URL:    zipformer + "decoder-epoch-99-avg-1.int8.onnx"},
		{Role: RoleJoiner, Name: "joiner-epoch-99-avg-1.int8.onnx", Size: 259335,
			SHA256: "831477d390e59a61f1b6a6f763b9903e6c6366ff6034f1ddba613be82637122f",
			URL:    zipformer + "joiner-epoch-99-avg-1.int8.onnx"},
		{Role: RoleTokens, Name: "tokens.txt", Size: 5048,
			SHA256: "49e3c2646595fd907228b3c6787069658f67b17377c60aeb8619c4551b2316fb",
			URL:    zipformer + "tokens.txt"},
		{Role: RolePunctuation, Name: "model.int8.onnx", Size: 7490500,
			SHA256: "9d611f445fe4a46186080fe161be6059d87d72eb88d3a8cb00c1a06e83a6067e",
			From:   punctuation, Member: "sherpa-onnx-online-punct-en-2024-08-06/model.int8.onnx"},
		{Role: RolePunctuationVocab, Name: "bpe.vocab", Size: 149430,
			SHA256: "e118b7ad88c54db562517df49e1cffd4836d166c34fb190fd311d7f34eb238f5",
			From:   punctuation, Member: "sherpa-onnx-online-punct-en-2024-08-06/bpe.vocab"},
	},
}

// models are the pinned models by name: what a worker is told to load.
var models = map[string]Model{DefaultModel.Name: DefaultModel}

// File is the path of the model's file for role in dir.
func (m Model) File(dir string, role Role) string {
	for _, f := range m.Files {
		if f.Role == role {
			return filepath.Join(dir, f.Name)
		}
	}
	return ""
}

// Download is the bytes a fetch of every file reads: each file published
// alone, and each archive once.
func (m Model) Download() int64 {
	var n int64
	seen := map[*ModelArchive]bool{}
	for _, f := range m.Files {
		switch {
		case f.From == nil:
			n += f.Size
		case !seen[f.From]:
			seen[f.From] = true
			n += f.From.Size
		}
	}
	return n
}

// pins are the model's files as the native-library cache pins them.
func (m Model) pins() []nativelib.File {
	out := make([]nativelib.File, 0, len(m.Files))
	for _, f := range m.Files {
		out = append(out, nativelib.File{Path: f.Name, SHA256: f.SHA256})
	}
	return out
}

// verified memoises a model directory that verified, per process: a worker
// restarted after an idle unload does not hash ~200 MB again.
var verified sync.Map

// Verify checks every file of m in dir against its pin.
func (m Model) Verify(dir string) error {
	if _, ok := verified.Load(dir); ok {
		return nil
	}
	if err := nativelib.Verify(dir, m.pins()); err != nil {
		return err
	}
	verified.Store(dir, true)
	return nil
}

// fetchTimeout bounds one preparation's downloads: about 230 MB at a slow
// 1 MB/s is four minutes, and Go's bzip2 adds seconds.
const fetchTimeout = 30 * time.Minute

// Fetch downloads every file of m that is missing from dir or does not verify
// from where it is pinned, an archive once for the files it holds. progress
// hears bytes read of [Model.Download]. Two processes sharing dir download
// once: the second waits on the first's lock, then finds the files verified.
func (m Model) Fetch(ctx context.Context, dir string, progress func(done, total int64)) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("speech: creating the model directory %s: %w", dir, err)
	}
	ctx, release := locked(ctx, dir)
	defer release()
	total := m.Download()
	var done int64
	members := map[*ModelArchive]map[string]string{}
	for _, f := range m.Files {
		final := filepath.Join(dir, f.Name)
		pin := []nativelib.File{{Path: f.Name, SHA256: f.SHA256}}
		if nativelib.Verify(dir, pin) == nil {
			if f.From == nil {
				done += f.Size
			}
			continue
		}
		if f.From != nil {
			if members[f.From] == nil {
				members[f.From] = map[string]string{}
			}
			members[f.From][path.Clean(f.Member)] = final
			continue
		}
		base := done
		if err := fetchFile(ctx, f, final, func(n int64) { progress(base+n, total) }); err != nil {
			return err
		}
		done += f.Size
	}
	for a, want := range members {
		base := done
		if err := a.extract(ctx, want, func(n int64) { progress(base+n, total) }); err != nil {
			return err
		}
		done += a.Size
	}
	for a := range seenArchives(m) {
		if members[a] == nil {
			done += a.Size
		}
	}
	progress(done, total)
	verified.Delete(dir)
	return m.Verify(dir)
}

// seenArchives are the archives m's files come from.
func seenArchives(m Model) map[*ModelArchive]bool {
	out := map[*ModelArchive]bool{}
	for _, f := range m.Files {
		if f.From != nil {
			out[f.From] = true
		}
	}
	return out
}

// locked is ctx bounded by [fetchTimeout], holding the cross-process lock on
// dir until release.
func locked(ctx context.Context, dir string) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	deadline, _ := ctx.Deadline()
	unlock := func() {}
	if release, held := nativelib.Lock(dir, deadline); held {
		unlock = release
	}
	return ctx, func() {
		unlock()
		cancel()
	}
}

// fetchFile downloads one file published alone, verifying it before it is
// renamed into place.
func fetchFile(ctx context.Context, f ModelFile, final string, progress func(n int64)) error {
	body, err := get(ctx, f.URL, f.Size, progress)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	tmp, err := nativelib.Stage(body, final, f.SHA256)
	if err != nil {
		return fmt.Errorf("speech: %s: %w", f.URL, err)
	}
	return nativelib.Commit(map[string]string{final: tmp})
}

// extract downloads the archive once and keeps the members want names
// (cleaned archive path to final path), in one verified pass.
func (a ModelArchive) extract(ctx context.Context, want map[string]string, progress func(n int64)) error {
	body, err := get(ctx, a.URL, a.Size, progress)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	staged, err := nativelib.Extract(body, a.SHA256, want)
	if err != nil {
		return fmt.Errorf("speech: %s: %w", a.URL, err)
	}
	return nativelib.Commit(staged)
}

// get opens a pinned URL's body, read at most to size and one byte past it
// (so a longer answer fails its digest rather than being cut to fit), with
// progress at most once per [progressStep].
func get(ctx context.Context, url string, size int64, progress func(n int64)) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("speech: %s: %w", url, err)
	}
	// GUARDED: the pinned URL is ours, but its redirect is the host's to
	// name (ADR-0072).
	resp, err := httpx.Guarded(0).Do(req)
	if err != nil {
		return nil, fmt.Errorf("speech: downloading %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("speech: downloading %s: %s", url, resp.Status)
	}
	return &counting{r: io.LimitReader(resp.Body, size+1), c: resp.Body, progress: progress}, nil
}

// counting reports bytes read, at most once per [progressStep].
type counting struct {
	r        io.Reader
	c        io.Closer
	n        int64
	reported int64
	progress func(n int64)
}

// progressStep is how many bytes pass between progress reports: about 1% of
// a first download, which a screen shows as a percentage.
const progressStep = 2 << 20

func (c *counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n-c.reported >= progressStep || (errors.Is(err, io.EOF) && c.n != c.reported) {
		c.reported = c.n
		c.progress(c.n)
	}
	return n, err
}

func (c *counting) Close() error { return c.c.Close() }
