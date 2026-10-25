package manifest

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ErrInvalid reports a path, an entry or a manifest an artifact version may not
// hold, naming what to change.
var ErrInvalid = errors.New("manifest: invalid")

// ErrReserved reports a path the serving origin owns: a version may not publish
// under `_crewlet/` or `_assets/`, and `preflight.js` at the root is held to
// its own bounds.
var ErrReserved = errors.New("manifest: the path is reserved")

// The reserved prefixes, each the serving origin's own route under a ticket
// (`/t/{ticket}/_assets/{asset}`, `/_crewlet/runtime/...`). A file published
// there would be shadowed by the route or would shadow it, depending on which
// the origin matched first, and neither is a page anybody can reason about.
const (
	ReservedRuntimePrefix = "_crewlet/"
	ReservedAssetsPrefix  = "_assets/"
)

// PreflightPath is the root script a runtime may run before the entry, which
// the engine stores, validates and never executes.
const PreflightPath = "preflight.js"

// CleanPath is a published path as it is stored, served and compared, or a
// refusal naming why.
//
// THE ENGINE'S ONE FILE-PATH RULE ([worklet.NormalizeFilePath]): no empty, dot
// or dot-dot segment, no backslash, no control character, no trailing slash, a
// leading slash and surrounding space trimmed. A second grammar here would be
// a path the tracker's files accept and a version refuses, or the reverse.
// What is added is artifacts' own: the length bound, and the reserved
// prefixes, refused case-insensitively so `_Assets/x` cannot be published
// beside a route a proxy might fold to lower case.
func CleanPath(raw string) (string, error) {
	p, err := worklet.NormalizeFilePath(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(p) > artifacts.MaxPathBytes {
		return "", fmt.Errorf("%w: the path is %d bytes and the maximum is %d",
			ErrInvalid, len(p), artifacts.MaxPathBytes)
	}
	lower := strings.ToLower(p)
	for _, prefix := range []string{ReservedRuntimePrefix, ReservedAssetsPrefix} {
		if strings.HasPrefix(lower, prefix) {
			return "", fmt.Errorf("%w: %q is under %q, which the serving origin "+
				"routes itself; publish the file under another folder", ErrReserved,
				p, prefix)
		}
	}
	return p, nil
}
