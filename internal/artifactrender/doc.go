// Package artifactrender renders a published version in an operator-run
// Chromium as a viewer sees it, for a seat checking its own page; every
// request is answered in process by the node's own artifact origin.
//
// # Credential and bounds
//
// The frame is minted through a render-only credential (a lineage under
// [LineagePrefix]), so the public origin treats it as ended; its ticket dies
// with the render (ADR-0035). The browser is the operator's, named by Tier A
// artifacts.render.browser_url and driven over this package's internal cdp
// client (ADR-0165). A render writes nothing, and [artifacts]' Render
// constants bound its time and concurrency.
package artifactrender
