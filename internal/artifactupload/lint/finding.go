package lint

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Code names what a finding is about, from a closed set, so a surface can map a
// refusal to its own words and a test can name what it expects.
type Code string

// The refusals: a publish carrying any of them is refused whole.
const (
	// CodeRemoteScript is a script element whose source is another origin
	// and not an allowlisted CDN file the engine can copy in. Under the
	// artifact origin's strict script policy a script element carrying the
	// response's nonce runs from wherever it points, so the linter, not the
	// policy, is what keeps a page's scripts its own.
	CodeRemoteScript Code = "remote_script"

	// CodeRemoteStylesheet is a stylesheet from a host outside the allowlist.
	CodeRemoteStylesheet Code = "remote_stylesheet"

	// CodeUnpinned is a package CDN reference without an exact version: the
	// bytes behind it would change under the page.
	CodeUnpinned Code = "unpinned_version"

	// CodeRemoteImport is a module that imports another URL: copying it in
	// would mean rebuilding its module graph, which is a bundler's job.
	CodeRemoteImport Code = "remote_import"

	// CodeFormOffOrigin is a form that posts to another origin.
	CodeFormOffOrigin Code = "form_off_origin"

	// CodeRefreshOffOrigin is a meta refresh to another origin.
	CodeRefreshOffOrigin Code = "refresh_off_origin"

	// CodeJavaScriptURL is a javascript: URL, which the strict policy
	// refuses to run in the browser anyway.
	CodeJavaScriptURL Code = "javascript_url"

	// CodePlaceholder is source text that already carries the nonce
	// placeholder, which only the engine writes.
	CodePlaceholder Code = "nonce_placeholder"

	// CodeVendorOff is an allowlisted reference on an engine that copies no
	// CDN file in.
	CodeVendorOff Code = "vendoring_off"

	// CodeVendorFailed is an allowlisted file that could not be copied in:
	// the CDN refused it, it failed its integrity check, or it is too large.
	CodeVendorFailed Code = "vendor_failed"

	// CodeUnparseable is a page the linter could not rewrite into a form it
	// could verify: refused, since a page whose scripts the linter cannot
	// account for is one whose policy it cannot vouch for.
	CodeUnparseable Code = "unparseable"

	// CodeSlideFormat is a deck outside the slide format
	// (internal/artifacts/slideformat): an element, attribute or style the
	// subset does not take, a bound passed, or a file the deck names that the
	// version does not hold. The message names the rule broken.
	CodeSlideFormat Code = "slide_format"
)

// The warnings: legal, and almost certainly not what the author meant.
const (
	// CodeRemoteBlocked is an image, frame, media or other resource from
	// another origin, which the artifact origin's policy will not load.
	CodeRemoteBlocked Code = "remote_blocked"

	// CodeBrowserStorage is browser storage, which is per viewer and empty
	// on an opaque origin: shared state belongs in the datastore.
	CodeBrowserStorage Code = "browser_storage"

	// CodeNoDarkMode is a page with no dark-mode rules: it ignores the
	// reader's theme.
	CodeNoDarkMode Code = "no_dark_mode"

	// CodeNoViewport is a page with no viewport meta: it renders at desktop
	// width on a phone.
	CodeNoViewport Code = "no_viewport"

	// CodeAuthorNonce is a script that carries a nonce of its own, which the
	// engine's placeholder (written first, so the browser reads it) replaces.
	CodeAuthorNonce Code = "author_nonce"

	// CodePasswordField is a password or payment field: a page that asks
	// for credentials is the shape of phishing, whoever published it.
	CodePasswordField Code = "password_field"
)

// Refusing reports whether a finding of code c refuses the publish.
func (c Code) Refusing() bool {
	return slices.Contains([]Code{
		CodeRemoteScript, CodeRemoteStylesheet, CodeUnpinned, CodeRemoteImport,
		CodeFormOffOrigin, CodeRefreshOffOrigin, CodeJavaScriptURL, CodePlaceholder,
		CodeVendorOff, CodeVendorFailed, CodeUnparseable, CodeSlideFormat,
	}, c)
}

// Finding is one thing the linter found in one file.
type Finding struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", f.Path, f.Line, f.Message)
	}
	return fmt.Sprintf("%s: %s", f.Path, f.Message)
}

// ErrRefused is every refusal this package answers, wrapped by [*Refusal].
var ErrRefused = errors.New("lint: the publish is refused")

// Refusal is a publish the linter refuses, with every refusing finding in the
// order found: an author fixes them all in one pass rather than one per try.
type Refusal struct {
	Findings []Finding
}

func (r *Refusal) Error() string {
	lines := make([]string, len(r.Findings))
	for i, f := range r.Findings {
		lines[i] = f.String()
	}
	return "the version breaks the artifact policy: " + strings.Join(lines, "; ")
}

// Unwrap makes every refusal an [ErrRefused].
func (r *Refusal) Unwrap() error { return ErrRefused }

// findings collects one file's findings, split as they are answered.
type findings struct {
	path     string
	refusals []Finding
	warnings []Finding
}

func (fs *findings) add(line int, code Code, format string, args ...any) {
	f := Finding{Path: fs.path, Line: line, Code: code, Message: fmt.Sprintf(format, args...)}
	if code.Refusing() {
		fs.refusals = append(fs.refusals, f)
		return
	}
	fs.warnings = append(fs.warnings, f)
}

// err is the refusal the file earned, nil when it earned none.
func (fs *findings) err() error {
	if len(fs.refusals) == 0 {
		return nil
	}
	return &Refusal{Findings: fs.refusals}
}
