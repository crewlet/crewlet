// Package cdp is the artifact render's Chrome DevTools Protocol client: a thin,
// typed client over one websocket to a browser ([Dial]), with every session
// flat on that connection.
//
// It is the engine's one DevTools client (internal/browser's doc, ADR-0163):
// sitting under internal/artifactrender, it is importable from there alone,
// and its directory is the one whose Go may name a DevTools method.
//
// # Thin, rather than chromedp or go-rod
//
// The render sends 25 commands and reads 10 events across 9 domains, itself
// or through this package's own machinery. chromedp and go-rod each bring a
// generated surface of the whole protocol and a session model of their own —
// chromedp's actions on a context it owns, go-rod's pages and its own waits —
// and the rules this package keeps (a target held until it is set up, a
// request decided before it leaves, a crash that fails the calls waiting on
// it) would be written against their model and around it. So this package
// declares only what the render sends and reads, each type holding exactly
// the fields that the render, this package's machinery or its cases read or
// send, by the protocol's own JSON names.
//
// # Sessions, answers and events
//
// One connection carries every session: the browser's own ([Conn.Browser])
// and one per attached target, each named by its sessionId (flat mode). A
// command's answer is matched to it by its id, one counter across every
// session; an event goes to its session's listeners that asked for it, in
// the order it arrived ([Session.Listen], [Listener.Next]). A child's
// Target.attachedToTarget registers it before it is delivered, so whatever
// Chromium sends the child next has a session to reach, and [Conn.Session]
// hands it back from then until Chromium detaches it; an id it holds no
// session of comes back as a session that has ended, never registered, since
// nothing would ever end it. A command's frame waits with its caller until the
// connection's one writer takes it, never in a queue that would send it after
// its caller gave up, and a call under a context that has ended sends nothing.
//
// A session ends when Chromium detaches it ([*DetachedError]), when its
// target crashes ([*CrashedError], which Chromium itself would answer with
// nothing at all), when an event one of its listeners asked for is past
// [ReadLimit] ([ErrMessageTooBig]), when what its listeners hold passes its
// bound ([ErrBacklog]), or when its connection ends ([ErrClosed]). Its calls in
// flight fail with that end and every later call fails at once; its listeners
// hand over what they hold, then report it — but for a backlog's end, which
// lets go of what they held, and says so — and the connection's other sessions
// go on. The browser's own session ends only with its connection, since a
// connection without it could create, attach and resume nothing: what would
// end it ends the connection. A session that ended other than by Chromium's
// detach is still held, ended, until Chromium detaches it, so the children it
// announced, whose detaches arrive on it, still end. What a listener holds
// counts against its session's backlog and its connection's until it is taken
// or the listener closed, its session's end included, so a reader that stops
// closes its listener.
//
// # Holding new targets until they are set up
//
// [AutoAttach] turns auto-attach on, Chromium told to hold each new target at
// its start, sets each one up (the caller's Setup: a request gate, a binding,
// a script, each sent with [Send] rather than called), turns auto-attach on
// for it in turn — an out-of-process frame's frames, a page's or a worker's
// workers — and only then resumes it, behind what its Setup sent. So no
// target of the kinds measured that Chromium starts once auto-attach is on —
// a popup, with an opener or without, of its opener's site or another, a tab
// opened on about:blank, an out-of-process frame, a worker of any kind —
// runs a line before the rules the caller put on it, however long its Setup
// takes, as long as no tab still on about:blank opens a window. A resume of a
// page also releases every popup it holds, so the attacher resumes only a
// target Chromium holds, never one already running, and the two commands that
// hold and release a target, Target.setAutoAttach and
// Runtime.runIfWaitingForDebugger, are this package's alone.
//
// What Chromium holds is each target of these kinds that it starts once
// auto-attach is on: a page — a popup window.open or a link opens, of its
// opener's site or another, with an opener or without one, or a tab
// Target.createTarget opens on about:blank — an out-of-process frame, and a
// worker of any kind, a frame's own and a worker's own among them. On both
// pinned Chromiums, with every Setup made to take two seconds, none of them
// ran a line before its setup (TestAutoAttachHoldsEveryNewTargetUntilItIsSetUp
// drives every kind named here). Any other kind of target was not measured,
// and is not known to be held. Two of the ways Chromium holds a page were
// measured, and the attacher keeps to both:
//
//   - A popup window.open opens with an opener is held inside that
//     window.open, which returns to the opener's script once the popup is
//     released. A resume on the popup's own session releases it, and so does
//     one on its OPENER'S: on both pins a running page's popup, held for
//     three seconds, ran at once when the page was sent
//     Runtime.runIfWaitingForDebugger, before any setup of its own, while
//     resuming the page's dedicated, shared or service worker released
//     nothing. So the attacher resumes only a target Chromium holds, which
//     has run nothing and so opened nothing, and nothing makes a held target
//     open a window before it is resumed (AttachOptions.Setup). A resume can
//     release a popup through another page that shares its opener's
//     renderer, not only through the opener's own session: on both pins,
//     with Chromium run with --renderer-process-limit=1, the attacher's
//     resume of a tab created on about:blank, held and set up in a second,
//     released a popup that a running about:blank tab held inside
//     window.open, before the popup's own six-second Setup was done. When the
//     opener was on a site, its popup waited for its own resume even so, the
//     site's renderer not shared with the about:blank tab, and so did both
//     when Chromium ran by default, each tab with a renderer of its own. So a
//     caller never opens a window from a tab still on about:blank.
//   - A popup with no opener, and a tab Target.createTarget opens on
//     about:blank, are held by the browser until a resume reaches their own
//     session: on both pins a popup with no opener stayed held while the page
//     that opened it was resumed, and a navigation sent to a held tab waited
//     for the tab's resume. Such a popup has no renderer while it is held,
//     which is why Setup sends rather than calls: held for three seconds on
//     both pins, a popup opened by window.open's noopener or a link's
//     target=_blank answered Fetch.enable and Target.setAutoAttach, and its
//     Runtime, Page, Network and Emulation commands only once resumed, so a
//     Setup that waited on one would hold its target until [CommandTimeout],
//     and fail. What Setup sent is in place before its target runs a line:
//     such a popup ran its first script with the binding and the script its
//     Setup had sent.
//
// Two kinds are not held:
//
//   - A target already running when auto-attach reaches it — a tab opened
//     before it, the browser's first tab once it has started, and their
//     frames and workers — is attached as it runs, announced with
//     WaitingForDebugger false, and its setup comes after whatever it has
//     run. A caller that must not drive such a target refuses it in Setup,
//     or closes it and opens a tab of its own. The attacher never resumes
//     it, so a popup it opens is held until that popup's own setup.
//   - A tab Target.createTarget opens with an address is announced held, with
//     WaitingForDebugger true, yet on both pins it ran its first script
//     before a Setup that took two seconds. Announced held, it is resumed
//     once its Setup is done, and that resume releases a popup it has opened
//     meanwhile: on both pins such a tab's popup ran as the tab's two-second
//     Setup ended, before a Setup of its own that took four. So the caller
//     creates its tabs on about:blank, and navigates each once its Setup's
//     answers are in.
//
// [AutoAttach] refuses a Filter that takes tabs, sending nothing. Chromium
// then announces the page in each tab unheld (WaitingForDebugger false), and
// the attacher resumes only what Chromium says it holds: on both pins, with
// auto-attach taking tabs, a page opened from a running tab was announced so
// under its tab, which was held and resumed, and the page never ran. Taking
// tabs also had Chromium attach a page's prerender, under its tab, held
// before it had a renderer, where taking pages, as Chromium's default and the
// zero AttachOptions do, a page's speculation rules prerender nothing: on
// both pins the page they named was never requested.
//
// # The bounds
//
// Every command has its own deadline, [CommandTimeout] from its sending under
// the caller's own context. Every message is read whole up to [ReadLimit], by
// this package's own reader; a larger one is read through and ends only what
// it is for — the call it answers, or the session whose listener asked for it
// — so a page that provokes one ends at most its own session. The events
// queued for a session's listeners are held to [MaxBacklog], each counted once
// however many of them hold it, so a page that outruns its reader ends its own
// session rather than grow the engine's heap; and every session's together to
// [MaxConnBacklog], past which the session holding the most ends, so the heap
// a connection holds is bounded however many sessions it carries. What the
// browser's own session meets — an event past ReadLimit, a backlog past a
// bound — ends the connection, so what a page can make arrive there, the
// requests a gate there pauses among it, can end its whole browser, and a
// caller that must not let it listens for that on the page's own session
// instead, as the render does.
//
// [ReadLimit] is 16 MiB because the largest answer the render asks for by
// design is a screenshot of its widest viewport, 1920 by 900 CSS pixels at a
// device scale of 1 (internal/artifacts' Render bounds): at worst a PNG of its
// raw RGBA pixels, 6.6 MiB, about 8.8 MiB once base64 has carried it, and
// 16 MiB is that and more than half again. Nothing holds every other answer or
// event under it, since a page makes its own as large as it likes — a console
// line, an exception, or a paused request, which carries its body (on Chromium
// 157, measured on 2026-11-11, a POST of 40 MiB was paused as an event of
// 97.9 MB, 2.3 times its body, so a POST of about 7 MiB passes ReadLimit) —
// so a caller that listens for them, as the render does on its page's own
// session, meets [ErrMessageTooBig] there when a page passes it: a page that
// POSTs that much while it renders ends its own page session, and a render
// refuses every write anyway. The websocket library's own limit, 32 KiB by
// default, is lifted: a message past ReadLimit is read through, never
// refused. Each other bound's reason is at its definition.
//
// # The floor
//
// [CheckVersion] refuses a browser older than [browser.MinChromiumMajor],
// naming both versions; internal/browser argues the value. What this package
// declares needs no newer protocol: every command, event and field of it was
// described by major 142, read on 2026-11-09 from the devtools-protocol
// package's description at each major's Chrome for Testing revision, for every
// major from 138 to 145 and a dozen older ones back to 113. The suite holds
// it on every run against the two pinned Chrome for Testing builds, the newest
// and the floor's own (scripts/chromium-pin.env), reading each one's own
// description of the protocol against every command, event and field declared
// here (TestThePinsDescribeEveryMethodTheClientDeclares); a declaration that
// needs a newer protocol than the floor describes fails it on the floor's arm,
// and is a reason to raise the floor, argued in internal/browser.
//
// That proof is complete by construction rather than by review: [Call] and
// [Send] send, and [Session.Listen] and [Event.Of] read, only an entry of the
// package's inventory with the very types it is declared with there, so a
// descriptor built by any other spelling — a type alias of Method, a name set
// on a zero one, a struct of its shape converted — sends and reads nothing;
// and [Method] and [Event] keep their names unexported, so only this package
// can declare one, and each of their instantiations is a type of its own, so
// no declared one converts to other parameters. A test holds the inventory
// to the package's declarations both ways; the commands the package's own
// tests send beside it are registered by a test file, and by no other. Every
// command the render can send is one the floor was measured against.
//
// # What is never logged
//
// A command's parameters and results carry what a page holds and what the
// render types into it, so neither is ever logged or put in an error:
// [*ProtocolError] names the method, Chromium's code and its message,
// and never its data, which may echo the parameters. The connection logs at
// DEBUG alone, and only three lines: cdp_answer_unmatched, an answer whose
// call had gone (at its deadline, or given up by its caller), by the answer's
// id and session, since the method went with the call; cdp_event_unrouted, an
// event for a session the connection holds no record of, by its method and
// session; and cdp_message_too_big, a message past [ReadLimit] read through,
// by its id or its method, its session and its size. Every end is the
// caller's to report, through Err, because only the caller knows whose
// browser it was.
//
// # What it does not decide
//
// Which targets to refuse, what a crash means for a render, which requests a
// page may make, and what of a page's words reaches a model are the render's
// to decide (internal/artifactrender): these protocol types carry no
// provenance tags. The caller's HTTP client is the caller's too
// ([DialOptions]): this package refuses to dial without one, and never follows
// a redirect with it.
package cdp
