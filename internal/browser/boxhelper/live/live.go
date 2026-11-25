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
	"dial.mjs", "relay.mjs",
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

// The relay's own knobs, which config.json carries as the relay block beside
// internal/browser's bounds on what it posts.
const (
	// RetryBaseMS, RetryCeilingMS and RetryJitter are the pace a relay's
	// retries keep: doubling from a tenth of a second to two, each spread a
	// fifth either way, the bridge's own pace for the seat's holder, whose
	// 503s name two seconds, so the relays of a fleet never wake in step.
	RetryBaseMS    = 100
	RetryCeilingMS = 2000
	RetryJitter    = 0.2
	// FrameGiveUpMS is how long a frame is posted for: a picture five
	// seconds old is worth nothing, and the next is coming.
	FrameGiveUpMS = 5000
	// PostGiveUpMS bounds one post of a state or a check-in: the poll's own
	// hold, the slowest answer the bridge gives a relay on purpose.
	PostGiveUpMS = 10_000
	// PollGiveUpMS bounds one poll: two seat leases of holder wait (90 s at
	// the 45 s lease) and the 10 s hold, with a margin; a poll given up is
	// asked again at once.
	PollGiveUpMS = 120_000
	// LogBytes bounds the relay's log, one line saying it stopped past it:
	// a job's relay logs refusals and failures, a few hundred lines a job.
	LogBytes = 256 << 10
	// TermMS is how long a relay TERMed has to say bye and go: under the
	// job's stop grace (internal/sandbox's JobStopGrace, five seconds), so
	// a relay ended as its job is ends inside the harness's own grace, and
	// a post abandoned costs nothing, since frames are not sent again and
	// states supersede.
	TermMS = 2000
	// ParentMS is how often a relay reads its parent's pid, leaving once it
	// changes: one read a second costs nothing, and a relay outlives its
	// script by at most this and TermMS.
	ParentMS = 1000
)
