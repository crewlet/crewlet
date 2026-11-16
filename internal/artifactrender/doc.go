// Package artifactrender renders a published artifact version as a viewer
// sees it, for a seat checking its own page before anybody reviews it
// (render_artifact).
//
// # The engine renders, so nothing leaves it
//
// A seat's box holds no credential a page can be opened with: a frame is
// bound to a person's sign-in or token, and a box reaches the engine only
// through its tool bridge. So the engine renders the version itself, in a
// headless Chromium the operator runs (Tier A artifacts.render.browser_url),
// over internal/browser/cdp, and hands the seat its findings and its
// screenshots.
//
// # The same frame, kit and runtime a viewer gets
//
// The frame is minted by artifactserve.Frames, as a viewer's is, and every
// request on the artifact origin is served by an artifactserve.Handler built
// over the node's own estate router, object store, chart and embedded tree,
// in process: the runtime, the script nonce, the policy, the kit and the
// brand are the bytes a viewer's browser is sent. The host around the frame
// is a page on the dashboard's own origin, so the frame's ancestors are a
// viewer's, and it answers the page's hello and its reads as the seat.
//
// # No network, and no credential anywhere else
//
// Every request of every frame and worker is paused before it leaves the
// browser and answered by the engine; anything not on the artifact origin is
// refused and reported, so the browser needs no network and the engine
// fetches nothing a page names. The ticket is minted through a render
// credential (LineagePrefix) that only the render's own resolver knows, for
// the render's life: the public origin answers it as an ended credential.
//
// # Bounded, and nothing written
//
// The bounds are internal/artifacts' (the Render constants): a time bound,
// a node's concurrency, shots, widths, served and written bytes, findings.
// A render records no open and joins no room, and every write a page asks
// for is refused and reported.
package artifactrender
