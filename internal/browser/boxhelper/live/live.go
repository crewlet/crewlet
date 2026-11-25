package live

import (
	"embed"
	"fmt"
	"slices"
)

// js is the live view's modules, the bytes the engine writes into a box
// beside the helper's own.
//
//go:embed js/*.mjs
var js embed.FS

// modules is every module of the live view, in the order the box helper
// writes them, after its own common.mjs: each after every module it imports.
var modules = []string{
	"cdp.mjs", "jpeg.mjs", "frames.mjs", "state.mjs", "pages.mjs", "screen.mjs", "relaysock.mjs", "live.mjs",
}

// Modules is the live view's modules by name, in the order they are written.
func Modules() []string { return slices.Clone(modules) }

// Module is the embedded module name, shared by every caller, which reads
// it and never writes into it. The modules are embedded from this
// repository, so a listed one that is missing is a build whose modules are
// not the list's, which TestEveryModuleIsListedAndEmbedded refuses: it
// panics.
func Module(name string) []byte {
	body, err := js.ReadFile("js/" + name)
	if err != nil {
		panic(fmt.Sprintf("live: js/%s is listed and not embedded: %v", name, err))
	}
	return body
}

// The box's own knobs for its screen, which config.json carries as the
// frames block beside internal/browser's wire bounds.
const (
	// ScreencastFlowMajor is the oldest Chromium major whose screencast
	// keeps one frame unacknowledged and sends the last one made meanwhile
	// once it is acknowledged (the package doc, "The screen"): measured
	// 2026-11-25 on 153 to 159, 154 the first that does.
	ScreencastFlowMajor = 154
	// QuietMS is one watch slot: a page still that long has finished
	// painting, so a capture then shows its last picture, and a capture
	// sooner races the screencast's own last frame.
	QuietMS = 500
	// OversizeQuality and OversizeScale are the capture that replaces a
	// frame past its byte bound: half the size at quality 40, both cut,
	// since a page of noise defeats either alone.
	OversizeQuality = 40
	OversizeScale   = 0.5
	// MaxHeaderBytes bounds one header on relay.sock: a state at its 64 KiB
	// bound, with room for its record's own fields.
	MaxHeaderBytes = 80 << 10
	// TitleMS is how often every page's title is read, in one call to the
	// browser: no DevTools event tells a title a script sets (measured
	// 2026-11-25), so a tab's title lags a script's by at most a second, for
	// one browser-process call a second that touches no page.
	TitleMS = 1000
)
