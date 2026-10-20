package artifactupload

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// THE CHECK: a version's text files judged as a publish of its kind judges
// them, with nothing reserved, stored, copied or recorded. It is what lets a
// person writing a page see the linter's findings while they write, rather
// than only when a publish is refused.
//
// THE SAME STEPS, NOT A COPY OF THEM: the check builds a [run] and calls the
// steps a publish composes with ([run.checkNewText], [run.lintFiles],
// [run.themeWarnings]), so a rule added to the publish is a rule the check
// answers. What it leaves out is what needs the bytes stored or the network:
// the upload, the format a native kind is read by, and the copy of each
// allowlisted CDN file, which the check names at the path the copy would be
// served from without fetching it. A page the check passes can therefore
// still be refused at publish when a CDN refuses a file, fails its integrity
// check or answers one too large ([lint.CodeVendorFailed]).

// MaxCheckFiles bounds the files one check reads.
const MaxCheckFiles = 64

// MaxCheckBytes bounds the text one check reads, across its files: a check is
// asked for while a person writes, so it is held far under a publish's bound.
const MaxCheckBytes = 2 << 20

// CheckFile is one text file of the version being checked.
type CheckFile struct {
	Path string
	Text []byte
}

// CheckReport is what a publish of the files would answer: the refusals that
// would refuse it, every one, and the warnings it would land with.
type CheckReport struct {
	Refusals []lint.Finding `json:"refusals"`
	Warnings []lint.Finding `json:"warnings"`
}

// Check judges files as a version of kind with entry would be judged at
// publish, and writes nothing. Only a framed kind's files are pages a check
// can judge; a file that is not text, a path the manifest grammar refuses and
// a request past the bounds are refused whole, as a publish would refuse them.
func (s *Service) Check(ctx context.Context, c artifactsdomain.Call, kind artifacts.Kind, entry string,
	files []CheckFile) (CheckReport, error) {

	if !kind.Valid() || !kind.Framed() {
		return CheckReport{}, fmt.Errorf("%w: only a page's files are checked, and %q is not a "+
			"kind whose files are pages", ErrInvalid, kind)
	}
	if len(files) == 0 || len(files) > MaxCheckFiles {
		return CheckReport{}, fmt.Errorf("%w: a check reads 1 to %d files, and this one names %d",
			ErrInvalid, MaxCheckFiles, len(files))
	}
	principal, err := principalOf(c)
	if err != nil {
		return CheckReport{}, err
	}
	r := &run{s: s, c: c, principal: principal, kind: kind, dry: true,
		files: map[string]*pending{}, removes: map[string]bool{}, vendored: map[string]bool{},
		versions: map[int]artifactsdomain.Version{}}
	total := 0
	for _, f := range files {
		p, err := manifest.CleanPath(f.Path) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return CheckReport{}, classify(err)
		}
		if r.files[p] != nil {
			return CheckReport{}, fmt.Errorf("%w: %q is named twice", ErrInvalid, p)
		}
		typ, err := typeOf(p, "")
		if err != nil {
			return CheckReport{}, err
		}
		if !typ.Text {
			return CheckReport{}, fmt.Errorf("%w: %q is a %s file; a check reads text files only",
				ErrInvalid, p, typ.Essence())
		}
		if total += len(f.Text); total > MaxCheckBytes {
			return CheckReport{}, fmt.Errorf("%w: a check reads at most %d bytes of text",
				ErrInvalid, MaxCheckBytes)
		}
		if err := checkContent(p, typ, f.Text, int64(len(f.Text))); err != nil {
			return CheckReport{}, err
		}
		r.files[p] = &pending{path: p, typ: typ, text: f.Text}
	}
	if entry != "" {
		p, err := manifest.CleanPath(entry)
		if err != nil {
			return CheckReport{}, classify(err)
		}
		entry = p
	}
	if err := r.checkNewText(); err != nil {
		return CheckReport{}, err
	}
	report := CheckReport{Refusals: []lint.Finding{}, Warnings: []lint.Finding{}}
	var refusal *lint.Refusal
	switch err := r.lintFiles(ctx); {
	case errors.As(err, &refusal):
		report.Refusals = refusal.Findings
	case err != nil:
		return CheckReport{}, err
	}
	r.themeWarnings(artifacts.Manifest{Entry: entry})
	if r.warnings != nil {
		report.Warnings = r.warnings
	}
	return report, nil
}
