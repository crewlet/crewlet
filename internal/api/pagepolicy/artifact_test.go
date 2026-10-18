package pagepolicy_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/artifacts"
)

// directives splits a policy into its directives, failing on one named twice:
// a browser honours the first and ignores the second, so a duplicate is a
// directive somebody thought they were setting.
func directives(t *testing.T, policy string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, part := range strings.Split(policy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), " ")
		if _, dup := out[name]; dup {
			t.Fatalf("%s is named twice in %q", name, policy)
		}
		out[name] = value
	}
	return out
}

func origin(shared, secure bool) pagepolicy.ArtifactOrigin {
	o := pagepolicy.ArtifactOrigin{
		Origin:    "https://0123456789abcdefghjkmnpqrs.example-usercontent.net",
		Ancestors: []string{"https://crewlet.example.com"},
		Sandbox:   []string{"allow-scripts", "allow-forms", "allow-same-origin"},
		Secure:    secure,
	}
	if shared {
		o.Origin = "https://artifacts.example.com"
		o.Sandbox = []string{"allow-scripts", "allow-forms"}
		o.Shared = true
	}
	return o
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// TestAnArtifactDocumentGetsTheStrictPolicy holds every directive of the
// document policy to its decided value: a fresh nonce with strict-dynamic and
// nothing that admits inline script wholesale, the artifact's own origin as the
// only place it may reach, every embedding and navigation sink closed, and the
// frame's own sandbox repeated.
func TestAnArtifactDocumentGetsTheStrictPolicy(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	pagepolicy.ArtifactDocument(h, origin(false, true), "bm9uY2U", &artifacts.ScriptPolicy{Nonced: true})
	o := origin(false, true).Origin
	got := directives(t, h.Get("Content-Security-Policy"))
	want := map[string]string{
		"default-src":     "'none'",
		"script-src":      "'nonce-bm9uY2U' 'strict-dynamic'",
		"style-src":       o + " 'unsafe-inline'",
		"img-src":         o + " data: blob:",
		"media-src":       o + " blob:",
		"font-src":        o + " data:",
		"connect-src":     o,
		"worker-src":      "blob:",
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"form-action":     "'none'",
		"frame-src":       "'none'",
		"frame-ancestors": "https://crewlet.example.com",
		"sandbox":         "allow-scripts allow-forms allow-same-origin",
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the document policy has %d directives, want exactly %d: %v", len(got), len(want), got)
	}
	for name, value := range map[string]string{
		"Permissions-Policy":           pagepolicy.ArtifactPermissions,
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Origin-Agent-Cluster":         "?1",
		"Cache-Control":                "private, no-store",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Strict-Transport-Security":    pagepolicy.HSTS,
	} {
		if h.Get(name) != value {
			t.Errorf("%s = %q, want %q", name, h.Get(name), value)
		}
	}
	for _, absent := range []string{"Cross-Origin-Embedder-Policy", "X-Frame-Options",
		"Access-Control-Allow-Origin", "Set-Cookie"} {
		if v := h.Get(absent); v != "" {
			t.Errorf("an artifact document carries %s: %q", absent, v)
		}
	}
}

// TestAPublishedHandlerIsAdmittedByItsDigestAlone: 'unsafe-hashes' appears only
// beside a digest, a value that is not a digest never reaches the policy, and
// the two evaluation escapes appear only where the linter recorded them.
func TestAPublishedHandlerIsAdmittedByItsDigestAlone(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	pagepolicy.ArtifactDocument(h, origin(false, false), "n", &artifacts.ScriptPolicy{
		Nonced:        true,
		HandlerHashes: []string{digest("go()"), "'; script-src *", "short"},
		WasmEval:      true,
	})
	script := directives(t, h.Get("Content-Security-Policy"))["script-src"]
	want := "'nonce-n' 'strict-dynamic' 'unsafe-hashes' 'sha256-" + digest("go()") + "' 'wasm-unsafe-eval'"
	if script != want {
		t.Fatalf("script-src = %q, want %q", script, want)
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Errorf("an http origin sent HSTS")
	}

	h = http.Header{}
	pagepolicy.ArtifactDocument(h, origin(false, false), "n", &artifacts.ScriptPolicy{UnsafeEval: true})
	if got := directives(t, h.Get("Content-Security-Policy"))["script-src"]; got != "'nonce-n' 'strict-dynamic' 'unsafe-eval'" {
		t.Fatalf("script-src with string evaluation = %q", got)
	}
	h = http.Header{}
	pagepolicy.ArtifactDocument(h, origin(false, false), "n", nil)
	if got := directives(t, h.Get("Content-Security-Policy"))["script-src"]; got != "'nonce-n' 'strict-dynamic'" {
		t.Fatalf("script-src with no recorded script facts = %q", got)
	}
}

// TestTheSharedModeIsCrossOriginAndThePerArtifactModeIsNot: an opaque document's
// own subresources are cross-origin requests, so shared mode answers them with
// CORP cross-origin and a wildcard CORS grant, and per-artifact mode with
// neither.
func TestTheSharedModeIsCrossOriginAndThePerArtifactModeIsNot(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{false, true} {
		for name, set := range map[string]func(http.Header, pagepolicy.ArtifactOrigin){
			"document": func(h http.Header, o pagepolicy.ArtifactOrigin) {
				pagepolicy.ArtifactDocument(h, o, "n", nil)
			},
			"subresource": pagepolicy.ArtifactSubresource,
		} {
			h := http.Header{}
			set(h, origin(shared, false))
			corp, acao := "same-origin", ""
			if shared {
				corp, acao = "cross-origin", "*"
			}
			if h.Get("Cross-Origin-Resource-Policy") != corp || h.Get("Access-Control-Allow-Origin") != acao {
				t.Errorf("%s, shared=%v: CORP %q and ACAO %q, want %q and %q", name, shared,
					h.Get("Cross-Origin-Resource-Policy"), h.Get("Access-Control-Allow-Origin"), corp, acao)
			}
		}
	}
}

// TestASubresourceRunsNothingOnItsOwn: opened directly, a file renders inert,
// and nothing may frame it.
func TestASubresourceRunsNothingOnItsOwn(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	pagepolicy.ArtifactSubresource(h, origin(false, true))
	got := directives(t, h.Get("Content-Security-Policy"))
	for name, value := range map[string]string{
		"default-src": "'none'", "frame-ancestors": "'none'", "sandbox": "",
	} {
		if v, ok := got[name]; !ok || v != value {
			t.Errorf("%s = %q (present %v), want %q", name, v, ok, value)
		}
	}
	if h.Get("X-Frame-Options") != "DENY" || h.Get("Permissions-Policy") != "" {
		t.Errorf("subresource headers: %v", h)
	}

	h = http.Header{}
	pagepolicy.ArtifactPublic(h, true)
	if h.Get("Cross-Origin-Resource-Policy") != "cross-origin" || h.Get("Access-Control-Allow-Origin") != "*" ||
		h.Get("Content-Security-Policy") != pagepolicy.ArtifactInert || h.Get("Strict-Transport-Security") != pagepolicy.HSTS {
		t.Errorf("public headers: %v", h)
	}
}

// TestTheArtifactPermissionsDenyEveryFeature: every member of the header is a
// feature denied to everyone, so none can be delegated or used.
func TestTheArtifactPermissionsDenyEveryFeature(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, member := range strings.Split(pagepolicy.ArtifactPermissions, ",") {
		feature, allow, ok := strings.Cut(strings.TrimSpace(member), "=")
		if !ok || allow != "()" {
			t.Errorf("%q is not a feature denied to everyone", member)
		}
		if seen[feature] {
			t.Errorf("%s is named twice", feature)
		}
		seen[feature] = true
	}
	for _, device := range []string{"camera", "microphone", "geolocation", "display-capture",
		"usb", "serial", "hid", "bluetooth", "payment", "clipboard-read"} {
		if !seen[device] {
			t.Errorf("the artifact permissions do not deny %s", device)
		}
	}
}

// TestTheDashboardMayFrameTheArtifactOriginAndNothingElse: DashboardFor adds a
// frame-src and changes no other directive, and a node serving no artifacts
// keeps the dashboard policy as it is.
func TestTheDashboardMayFrameTheArtifactOriginAndNothingElse(t *testing.T) {
	t.Parallel()
	if got := pagepolicy.DashboardFor(nil); got != pagepolicy.Dashboard {
		t.Fatalf("DashboardFor(nil) = %q, want the dashboard policy unchanged", got)
	}
	got := directives(t, pagepolicy.DashboardFor([]string{"https://*.example-usercontent.net"}))
	base := directives(t, pagepolicy.Dashboard)
	for name, value := range base {
		if got[name] != value {
			t.Errorf("%s moved: %q, was %q", name, got[name], value)
		}
	}
	if got["frame-src"] != "https://*.example-usercontent.net" || len(got) != len(base)+1 {
		t.Errorf("frame-src = %q in %v", got["frame-src"], got)
	}
}

// recognized is every directive a shipping browser parses (CSP Level 3 as
// Chrome, Firefox and Safari implement it). A directive outside it is logged as
// "Unrecognized Content-Security-Policy directive" on every response that
// carries it, which is what the retired `webrtc 'block'` did to every frame.
var recognized = map[string]bool{
	"default-src": true, "script-src": true, "script-src-elem": true, "script-src-attr": true,
	"style-src": true, "style-src-elem": true, "style-src-attr": true, "img-src": true,
	"media-src": true, "font-src": true, "connect-src": true, "worker-src": true,
	"object-src": true, "base-uri": true, "form-action": true, "frame-src": true,
	"child-src": true, "manifest-src": true, "frame-ancestors": true, "sandbox": true,
	"upgrade-insecure-requests": true, "report-uri": true, "report-to": true,
	"require-trusted-types-for": true, "trusted-types": true,
}

// TestEveryArtifactDirectiveIsOneBrowsersRecognize: the artifact origin's
// policies name only directives a browser parses, so a frame load logs no
// console error for the policy itself.
func TestEveryArtifactDirectiveIsOneBrowsersRecognize(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{false, true} {
		doc := http.Header{}
		pagepolicy.ArtifactDocument(doc, origin(shared, true), "n", &artifacts.ScriptPolicy{Nonced: true})
		sub := http.Header{}
		pagepolicy.ArtifactSubresource(sub, origin(shared, true))
		for _, policy := range []string{doc.Get("Content-Security-Policy"), sub.Get("Content-Security-Policy")} {
			if policy == "" {
				continue
			}
			for name := range directives(t, policy) {
				if !recognized[name] {
					t.Errorf("the artifact policy names %q, which browsers do not recognize: %s", name, policy)
				}
			}
		}
	}
}
