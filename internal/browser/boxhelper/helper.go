package boxhelper

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strconv"
	"sync"

	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
)

// js is the helper's modules, the bytes the engine writes into a box.
//
//go:embed js/*.mjs
var js embed.FS

// modules is every module of the helper but its entry, and entry the entry
// itself: [Files] writes them, and [Version] reads them, as modules, then
// the live view's (live.Modules), then entry, each after every module it
// imports.
var modules = []string{
	"common.mjs", "tree.mjs", "chromium.mjs", "fence.mjs", "serve.mjs", "ensure.mjs", "supervise.mjs", "shim.mjs",
}

const entry = "helper.mjs"

// File is one module of the helper.
type File struct {
	// Name is its file name in the helper's folder ([Folder]).
	Name string
	// Body is its bytes, shared by every caller, which reads them and never
	// writes into them.
	Body []byte
}

// files is the modules, read once. They are embedded from this repository,
// so one that is missing is a build whose modules are not the list's, which
// TestEveryEmbeddedModuleIsWrittenAfterWhatItImports refuses: it panics.
var files = sync.OnceValue(func() []File {
	own := func(name string) File {
		body, err := js.ReadFile("js/" + name)
		if err != nil {
			panic(fmt.Sprintf("boxhelper: js/%s is listed and not embedded: %v", name, err))
		}
		return File{Name: name, Body: body}
	}
	out := make([]File, 0, len(modules)+len(live.Modules())+1)
	for _, name := range modules {
		out = append(out, own(name))
	}
	for _, name := range live.Modules() {
		out = append(out, File{Name: name, Body: live.Module(name)})
	}
	return append(out, own(entry))
})

// Files is the helper's modules, in the order [Write] writes them.
func Files() []File {
	return slices.Clone(files())
}

// Version names the helper the engine writes: the first 16 hex digits of
// SHA-256 over each module's name, a NUL, its length in decimal, a NUL and its
// bytes, in [Files] order. config.json carries it, so a box tells this build's
// helper from another's by it.
var Version = sync.OnceValue(func() string { return version(Files()) })

// version is [Version] over any modules: the name and the length frame each
// body, so no rename, reordering or shift of bytes from one module to the next
// leaves it where it was.
func version(files []File) string {
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.Name + "\x00" + strconv.Itoa(len(f.Body)) + "\x00"))
		h.Write(f.Body)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Folder is the helper's folder in the box whose home is home:
// <home>/.crewlet/browser-box, beside the executor's own files under
// <home>/.crewlet, and the one place the path is spelled.
func Folder(home string) string {
	return path.Join(home, ".crewlet", "browser-box")
}
