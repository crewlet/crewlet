// Package live is the box helper's live view of a seat's browser: what its
// supervisor serves a job's relay over relay.sock, beside the model's
// browser on browser.sock (internal/browser/boxhelper). It reads the browser
// and acts on no page: every page of every context, each a tab of the box's
// boot; the page in view; that page's screen while somebody watches; the
// state of the tabs; and whether the relay's own job's shim holds the
// browser, the check-in. Its modules, js/*.mjs ([Modules]), are written into
// the helper's folder after the helper's own and before its entry, under the
// helper's one Version, and helper.mjs attaches them to the supervisor, which
// imports nothing of them. internal/browser holds the wire, and the bounds
// config.json carries for the box beside this package's own knobs
// ([ScreencastFlowMajor] and its fellows). The split is boxhelper's: what
// sits behind relay.sock is here, the browser's server (ensure, the tree,
// the supervisor, serving, the fence, the shim) there.
//
// # What attaches
//
// The live view is one of the supervisor's attachments: attached once
// browser.sock listens and before the supervisor says ready, it awaits only
// its own socket's listen, never the browser, and one that fails costs the
// model nothing (boxhelper's doc). It is told each browser launched, each
// state, the model connection's job, and each browser_tabs call that moved the
// server's current tab, and is closed first on a stop. Every listener it hands
// Playwright, a timer or a socket logs what it throws, and every promise it
// leaves unawaited what it rejects with: Playwright MCP puts a rejection
// nobody handled into the model's next answer, and an exception would end the
// supervisor and the model's browser with it.
//
// # The pages
//
// Every page of every context of the browser served: the persistent
// context's, and those the model's server opens in isolated contexts, which a
// person would otherwise never see the model browse. Each takes a tab id of
// the boot, t1, t2 and on, never used twice; past nine digits a new boot is
// drawn and the live pages are numbered again from t1, no Chromium restarted.
// The boot is the supervisor's, drawn once, so a crash's relaunch keeps the
// numbering and a new supervisor starts it again. Each page has a DevTools
// session of the helper's own, never Playwright's or the server's: on it the
// main frame says when it loads, and its target once, by which one session on
// the browser reads every page's title in one call every state.title_ms and
// as a page loads, since no DevTools event tells a title a script sets
// (measured 2026-11-25: target discovery tells the title a page loads with
// and no later one) and Playwright has none. A user name or password in an
// address never leaves the box.
//
// # The page in view
//
// No page's own state says which is shown. Under the supervisor's launch every
// page reads visible and focused, a tab behind another included, since
// Playwright turns focus emulation on for every page: measured on 2026-11-25
// on both pins and Playwright's expected build, which, started without
// Playwright, read a background tab hidden. So the page in view is the
// model's: the tab its last browser_tabs new opened or its last select chose;
// a page another page opens, a popup or a link's new tab, as it opens, as a
// person at the screen would see it; when the page in view closes, the tab
// the server makes current then; and, as a model connection begins or the
// browser is launched again under one, its first tab, as Playwright MCP's own
// does. Whichever came last wins; no tab is no page in view. The view keeps
// each connection's tab order as the server keeps its own, every page of the
// contexts it opened in the order Playwright reports them, and reads a tab
// call's answer, its Result section alone, for the current tab: a count or an
// address that disagrees leaves the view where it was, logged. So a Playwright
// MCP bump that changes its order or its answer fails the suite on its bump.
//
// # The screen
//
// A screencast of the page in view, on its own session, only while the
// relay's newest watch says on; never Playwright's own, whose frames carry no
// device size and whose frames in flight it fixes. It stops on a watch of
// off, on the relay's bye and on its connection's close; the next relay's
// watch starts it again. Each start, and each change of page, opens with one
// capture of the page as it is, sent before any frame of the screencast.
//
// One frame is in flight at every rate: the container is the browser's one
// cell, where a post's round trip is milliseconds against a slot. A slot opens
// once 1000/fps ms have passed since the last frame sent and none awaits its
// frame_ack: the newest frame held is sent and the older are acknowledged to
// Chromium unsent, so no picture is slots old; and a frame sent is
// acknowledged once its frame_ack has come and the next slot has opened, so
// what Chromium sends then is what the page shows then.
//
// The last picture always arrives. From ScreencastFlowMajor on, the screencast
// is given its flow pair, one frame in flight and the last frame made meanwhile
// sent on its acknowledgement: measured on 2026-11-25, every Chrome for Testing
// build from 154 to 159 and Playwright's 159.0.8154.6 does so, and 153
// declares neither and keeps three. Below it, once a screencast frame sent
// has had its slot, frames.quiet_ms with no frame takes one capture, and a
// still page, its capture taken, takes no more. On either arm a page that
// resizes takes one capture: measured the same day, after a scaled capture
// the screencast sent the resized page under its old device size, or not at
// all, in some runs; with no capture, in none of twelve. A capture is an
// ordinary frame, of the visual viewport, its metadata from the same layout
// metrics; it resizes no page, and is scaled only where it must be, for a
// page past the frame bound or a frame past the byte bound.
//
// A frame's header is what the panel maps clicks with: its JPEG's own size,
// read as the hub reads it (jpeg.mjs mirrors image/jpeg's DecodeConfig within
// internal/browser's JPEGHeaderBytes), and the page's device size, offset and
// scale. A frame the hub would refuse is dropped, and one past
// frames.max_bytes replaced by one capture at frames.oversize_scale and
// oversize_quality, sent only if it fits; either is acknowledged to Chromium
// at the next slot, as an older frame is, since acknowledged at once, a page
// whose every frame passed the bound was measured running Chromium flat out.
// The screen decodes no pixel and evaluates nothing in a page, and every
// call it makes is in cdp.mjs's table, which refuses one off it and which the
// suite holds to each pin.
//
// # The state and the check-in
//
// The state is the boot, a rev rising with each document the boot makes, what
// the browser is doing (running, restarting or crashed_out, by the
// supervisor's state), and its tabs: at most state.max_tabs, the page in view
// among them and alone in front, none in front when no page is in view. A
// title loses its control characters and is cut to state.title_chars; an
// address past state.url_bytes is its origin. The whole is fitted to
// state.max_bytes as the relay will post it, addresses to origins and then
// titles, each from the last tab. The newest replaces one not yet written.
// The check-in tells the relay whether its own job's shim holds the browser,
// and since when by the box's clock: after its hello, and as that changes.
//
// # relay.sock
//
// A filesystem socket in the helper's folder at 0600, a stale one removed
// first, listening before the supervisor says ready. A record is a big-endian
// u32 header length, the header's JSON, at most relay.header_bytes, and a
// frame's JPEG after it. A relay says its hello within hello_ms, naming its
// helper and job, and is answered the boot and the browser, or refused:
// helper_mismatch, or not_ready before the first launch is served. The newest
// accepted relay closes the one before, and is sent the newest state and its
// check-in. A record past its bounds or off its end's types, a watch at a
// rate the settings do not name, or any other malformed record ends the
// connection, logged, and the supervisor serves on.
//
// # The suite
//
// Real-browser cases run at each end of what a box may bring
// (boxhelpertest.Each), speak relay.sock as a relay would
// (boxhelpertest.Box.DialRelay) and drive the model through the shim; the
// screen's pacing, the table, the state's fitting and the ids run on the
// probe's clock. No Go text names a DevTools method (internal/browser).
package live
