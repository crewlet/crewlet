package pagepolicy

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE ARTIFACT ORIGIN'S POLICIES: what a browser is told about every response
// the api.artifacts listener sends, built here beside the dashboard's and the
// API's so every Content-Security-Policy the engine writes is in one package.
//
// An artifact is arbitrary script and its origin holds no credential, so the
// separate origin keeps a session safe, not the policy. The policy keeps the
// artifact's own data and viewers safe: a page that rendered a row another
// viewer wrote as markup would run its script as every viewer who opens it,
// with the viewer's bridge in reach. So scripts are the page's own and those
// they load, never what a row writes; fetches, sockets, beacons, images,
// media, fonts and styles reach only the artifact's own origin.

// WHAT THE POLICY DOES NOT BOUND: the page's own author. 'strict-dynamic' (D41)
// trusts every script a nonced script creates, from any URL, so a page's own
// script can send what its viewer may read through the bridge to a third
// party. Dropping it would not close that (a nonced element loads from any URL
// and a page's script can read its nonce); only a nonce-free policy would, and
// it breaks every page whose libraries insert scripts at run time. What bounds
// it is who may publish (an editor), the version history naming who published
// what, and the linter refusing every literal remote script source. A viewer
// who writes a row cannot reach this channel: their markup carries no nonce.

// ArtifactOrigin is what every artifact response's headers are built from: the
// facts about the origin and the frame a document is served into.
type ArtifactOrigin struct {
	// Origin is the artifact's own origin (`https://<label>.example.net`, or
	// the one shared origin): the only place a document may connect to and
	// load images, media, fonts and styles from. Scripts are admitted by
	// nonce and 'strict-dynamic' instead, which name no host (see the
	// package comment on what that leaves open).
	//
	// NAMED RATHER THAN 'self'. In a document served with an opaque origin
	// (shared mode) 'self' is the policy's own origin, which browsers have
	// resolved differently over time; the host itself is unambiguous in
	// both modes.
	Origin string

	// Ancestors are the dashboard origins that may frame a document: the
	// API's external origin and every origin the API admits.
	Ancestors []string

	// Sandbox are the frame's sandbox tokens, the very ones the dashboard
	// sets on the iframe ([artifacts.Frame.Sandbox]). The policy carries them
	// too, so a document opened anywhere but its frame is just as sandboxed.
	Sandbox []string

	// Shared is the shared (opaque origin) mode: the document's origin is
	// opaque, so every subresource it loads is a cross-origin request.
	Shared bool

	// Secure is an https external URL, which adds [HSTS].
	Secure bool
}

// ArtifactPermissions is the Permissions-Policy every artifact document is
// served under: every powerful feature denied, to the document and to anything
// it could frame.
//
// DENIED IN THE HEADER AS WELL AS BY THE FRAME. The dashboard's iframe carries
// an empty `allow` attribute, which already delegates nothing; the header is
// what still holds when a document is opened somewhere other than its frame,
// and it is the shape a later build's per-artifact consent edits (a feature a
// version declares and a viewer consented to becomes `=(self)`), one feature
// at a time, rather than a policy written from nothing.
const ArtifactPermissions = "accelerometer=(), ambient-light-sensor=(), autoplay=(), " +
	"battery=(), bluetooth=(), camera=(), clipboard-read=(), clipboard-write=(), " +
	"display-capture=(), document-domain=(), encrypted-media=(), fullscreen=(), " +
	"gamepad=(), geolocation=(), gyroscope=(), hid=(), identity-credentials-get=(), " +
	"idle-detection=(), keyboard-map=(), local-fonts=(), magnetometer=(), " +
	"microphone=(), midi=(), otp-credentials=(), payment=(), picture-in-picture=(), " +
	"publickey-credentials-create=(), publickey-credentials-get=(), " +
	"screen-wake-lock=(), serial=(), speaker-selection=(), storage-access=(), " +
	"usb=(), web-share=(), window-management=(), xr-spatial-tracking=()"

// ArtifactDocument sets the headers of one HTML (or other scriptable) document
// served into an artifact frame, under this response's nonce and the linter's
// script facts. Call it before anything writes the status.
// `script-src 'nonce-…' 'strict-dynamic'`: the linter put a placeholder in
// every script element and serving substitutes the nonce, so the page's
// scripts run while markup injected through a viewer's row carries none.
// Handlers are admitted by digest, eval and WebAssembly only where the linter
// found a need, a nil script admits none, and no host is named, so the page's
// author is unbound (the package comment). Other sources are the artifact's
// own origin; worker-src is blob: alone, so no service worker outlives a
// revocation; frame-ancestors are the dashboard's, so no other site frames it;
// the sandbox allows forms, or onsubmit never fires. No COEP: never isolated.
func ArtifactDocument(h http.Header, o ArtifactOrigin, nonce string, script *artifacts.ScriptPolicy) {
	h.Set("Content-Security-Policy", artifactDocumentPolicy(o, nonce, script))
	h.Set("Permissions-Policy", ArtifactPermissions)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Origin-Agent-Cluster", "?1")
	h.Set("Cache-Control", "private, no-store")
	artifactCommon(h, o)
}

// ArtifactSubresource sets the headers of one file a document loads: a script,
// a stylesheet, an image, a font, data. Call it before anything writes the
// status; the cache lifetime is the caller's, since it follows the ticket.
//
// NOTHING MAY RUN IN ONE OPENED ON ITS OWN. The policy is `default-src 'none'`
// with an empty sandbox, so a subresource navigated to directly (an SVG, an
// XML file) renders inert; a document the frame navigates to is served under
// [ArtifactDocument] instead, by the request's own destination.
func ArtifactSubresource(h http.Header, o ArtifactOrigin) {
	h.Set("Content-Security-Policy", ArtifactInert)
	h.Set("X-Frame-Options", "DENY")
	artifactCommon(h, o)
}

// ArtifactPublic sets the headers of an embedded runtime or kit file: public,
// the same bytes on every artifact host, and loadable from any of them.
//
// CROSS-ORIGIN IN BOTH MODES, unlike a ticketed file: an edge cache keys these
// without the host, so one copy answers every artifact's origin, and an opaque
// document's module load of the runtime is a CORS request. Nothing here is
// anybody's: the file is in the binary.
func ArtifactPublic(h http.Header, secure bool) {
	h.Set("Content-Security-Policy", ArtifactInert)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", artifactRobots)
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("Access-Control-Allow-Origin", "*")
	if secure {
		h.Set("Strict-Transport-Security", HSTS)
	}
}

// ArtifactInert is the policy of a response nothing may run in: a subresource
// opened on its own, an embedded runtime or kit file, and every refusal the
// artifact origin answers.
const ArtifactInert = Inert

// artifactRobots keeps every response off search engines and archives: a
// ticketed URL is a credential, and even a public runtime file is nothing a
// crawler should index under an artifact's host.
const artifactRobots = "noindex, nofollow, noarchive"

// artifactCommon is what every ticketed response carries, document or not.
//
// CORP BY MODE. In per-artifact mode every consumer of a file is the
// artifact's own origin, so `same-origin`; in shared mode the document's
// origin is opaque, so every classic script, image and stylesheet it loads is
// a cross-origin no-cors request that a same-origin (or same-site) CORP would
// fail, and the page would break: `cross-origin`, with
// `Access-Control-Allow-Origin: *` for the module scripts and fetches that are
// CORS requests from `Origin: null`. The ticket in the path is the
// capability, and no credential is ever involved, so the wildcard grants
// nothing a holder of the URL does not already have.
func artifactCommon(h http.Header, o ArtifactOrigin) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", artifactRobots)
	if o.Shared {
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
	}
	if o.Secure {
		h.Set("Strict-Transport-Security", HSTS)
	}
}

// artifactDocumentPolicy is [ArtifactDocument]'s Content-Security-Policy.
func artifactDocumentPolicy(o ArtifactOrigin, nonce string, script *artifacts.ScriptPolicy) string {
	scripts := []string{"'nonce-" + nonce + "'", "'strict-dynamic'"}
	if script != nil {
		var handlers []string
		for _, digest := range script.HandlerHashes {
			if handlerDigest(digest) {
				handlers = append(handlers, "'sha256-"+digest+"'")
			}
		}
		if len(handlers) > 0 {
			scripts = append(append(scripts, "'unsafe-hashes'"), handlers...)
		}
		if script.UnsafeEval {
			scripts = append(scripts, "'unsafe-eval'")
		}
		if script.WasmEval {
			scripts = append(scripts, "'wasm-unsafe-eval'")
		}
	}
	ancestors := "'none'"
	if len(o.Ancestors) > 0 {
		ancestors = strings.Join(o.Ancestors, " ")
	}
	sandbox := "sandbox"
	if len(o.Sandbox) > 0 {
		sandbox += " " + strings.Join(o.Sandbox, " ")
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src " + strings.Join(scripts, " "),
		"style-src " + o.Origin + " 'unsafe-inline'",
		"img-src " + o.Origin + " data: blob:",
		"media-src " + o.Origin + " blob:",
		"font-src " + o.Origin + " data:",
		"connect-src " + o.Origin,
		"worker-src blob:",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-src 'none'",
		"frame-ancestors " + ancestors,
		sandbox,
	}, "; ")
}

// handlerDigest reports a value that is the standard base64 of a SHA-256
// digest, which is all an inline handler's source expression may carry.
//
// A VALUE THAT IS NOT ONE IS LEFT OUT of the policy, which is the closed
// direction: the handler it would have admitted does not run. The publish
// linter never writes one; what this refuses is a digest field that ever
// carried a quote or a semicolon, which would otherwise be a directive written
// into a policy by whatever wrote the manifest.
func handlerDigest(s string) bool {
	raw, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(raw) == sha256.Size
}

// DashboardFor is the dashboard's policy on a deployment that serves artifacts:
// [Dashboard] with a frame-src naming the artifact origin, so the viewer may
// embed the sandboxed frame and nothing else may be framed.
//
// A FUNCTION OF TIER A, still built here, rather than a second literal: the
// sources are what `api.artifacts.external_url` renders to (one origin, or a
// wildcard over the hosts a template names). No source is [Dashboard]
// unchanged, whose default-src 'self' frames nothing, which is right for a
// node serving no artifacts.
func DashboardFor(frameSources []string) string {
	if len(frameSources) == 0 {
		return Dashboard
	}
	return Dashboard + "; frame-src " + strings.Join(frameSources, " ")
}
