package artifactrender

import (
	"context"
	"encoding/json"
	"html"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/browser/cdp"
)

// The host: the dashboard's part around a frame, served on the dashboard's
// origin so frame-ancestors admit it. Its script and binding live in an
// isolated world, out of reach of a page sharing the host's process.

// hostWorld is the isolated world the host's script runs in, and hostBinding
// the binding it reports through, reachable from that world alone.
const (
	hostWorld   = "crewlet-render-host"
	hostBinding = "__crewletRenderHost"
	hostAnswer  = "__crewletRenderAnswer"
	frameID     = "crewlet-frame"
)

// frameBridge is the frame bridge version the host speaks (uilet's
// FRAME_BRIDGE): the one the dashboard speaks.
const frameBridge = 1

// hostPage is the host document for the current load.
func (u *run) hostPage() []byte {
	theme := string(u.loadTheme())
	base, _, _ := strings.Cut(u.frame.URL, "#")
	src := base + "#nonce=" + u.frame.Nonce + "&theme=" + theme + "&palette=crewlet&density=normal"
	kit := ""
	if u.r.kit != "" {
		kit = `<link rel="stylesheet" href="` + hostKit + `kit.css">`
	}
	return []byte(`<!doctype html><html lang="en" data-theme="` + theme + `"><head>` +
		`<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>` + html.EscapeString(u.req.Head.Title) + `</title>` + kit +
		`<style>html,body{margin:0;height:100%;overflow:hidden;` +
		`background:var(--color-surface-background,#fff)}` +
		`#` + frameID + `{display:block;border:0;width:100vw;height:100vh;background:transparent}</style>` +
		`</head><body><iframe id="` + frameID + `" title="` + html.EscapeString(u.req.Head.Title) +
		`" sandbox="` + html.EscapeString(u.frame.Sandbox) + `" allow="` +
		html.EscapeString(u.frame.Allow) + `" src="` + html.EscapeString(src) + `"></iframe></body></html>`)
}

// hostScript is the handshake, run in [hostWorld] at the start of every
// document of the tab, and doing nothing but in the host's.
func (u *run) hostScript() string {
	target := u.frame.Origin
	if u.frame.Mode == artifacts.OriginShared {
		// AN OPAQUE ORIGIN can be posted to only as "*"; the window and the
		// nonce are what bind the port to the frame.
		target = "*"
	}
	init, _ := json.Marshal(map[string]any{
		"type": "bridge.init", "contract": u.r.contract, "capabilities": u.frame.Capabilities,
		"viewer": map[string]any{"seat": u.req.Viewer.Seat, "kind": u.req.Viewer.Kind,
			"displayName": u.req.Viewer.DisplayName, "role": string(u.req.Viewer.Role)},
		"connection": "live",
	})
	host, _ := json.Marshal(u.r.dashboard + hostPath)
	nonce, _ := json.Marshal(u.frame.Nonce)
	origin, _ := json.Marshal(target)
	return `(() => {
  if (location.origin + location.pathname !== ` + string(host) + `) return;
  const report = (m) => { try { globalThis.` + hostBinding + `(JSON.stringify(m)); } catch (e) {} };
  let port = null;
  globalThis.` + hostAnswer + ` = (m) => { if (port) port.postMessage(m); };
  window.addEventListener("message", (e) => {
    const f = document.getElementById("` + frameID + `");
    const d = e.data;
    if (!f || e.source !== f.contentWindow || port || !d || typeof d !== "object") return;
    if (d.type !== "crewlet.hello" || d.nonce !== ` + string(nonce) + `) return;
    const channel = new MessageChannel();
    port = channel.port1;
    port.onmessage = (m) => {
      let data;
      try { data = JSON.parse(JSON.stringify(m.data)); } catch (err) { data = null; }
      report({ kind: "call", data });
    };
    f.contentWindow.postMessage({ type: "crewlet.port", bridge: ` + jsonInt(frameBridge) + ` }, ` +
		string(origin) + `, [channel.port2]);
    port.postMessage(` + string(init) + `);
    report({ kind: "hello" });
  });
})();`
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

// hostMessage is one report from the host's script.
type hostMessage struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// bridgeCall is a page's call as the bridge carries it.
type bridgeCall struct {
	Type   string         `json:"type"`
	ID     *int64         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// capabilityOf is the capability a bridge method needs (the bridge's own
// table, BRIDGE_METHODS), empty for one that needs none, and false for a
// method the bridge does not carry.
func capabilityOf(method string) (artifacts.Capability, bool) {
	switch method {
	case "identity.me", "identity.people":
		return artifacts.CapabilityIdentity, true
	case "data.schema", "data.get", "data.list", "data.query", "data.subscribe",
		"data.unsubscribe", "data.write", "data.undo", "data.redo":
		return artifacts.CapabilityDB, true
	case "state.get", "state.set", "state.delete", "state.list":
		return artifacts.CapabilityState, true
	case "presence.set":
		return artifacts.CapabilityPresence, true
	case "assets.list":
		return artifacts.CapabilityAssets, true
	case "openLink", "print":
		return "", true
	}
	return "", false
}

// hosted handles one report from the host's script, in the context it came
// from.
func (u *run) hosted(ctx context.Context, ev cdp.RuntimeBindingCalledEvent) {
	if ev.Name != hostBinding {
		return
	}
	var m hostMessage
	if err := json.Unmarshal([]byte(ev.Payload), &m); err != nil {
		return
	}
	switch m.Kind {
	case "hello":
		u.helloed()
	case "call":
		var c bridgeCall
		if err := json.Unmarshal(m.Data, &c); err != nil || c.Type != "bridge.call" || c.ID == nil {
			return
		}
		value, refusal := u.call(ctx, c)
		answer := map[string]any{"type": "bridge.result", "id": *c.ID}
		if refusal != nil {
			answer["ok"] = false
			answer["error"] = map[string]any{"code": refusal.Code, "message": refusal.Message}
		} else {
			answer["ok"] = true
			answer["value"] = value
		}
		body, err := json.Marshal(answer)
		if err != nil {
			return
		}
		if _, err := cdp.Call(ctx, u.tab.page, cdp.RuntimeEvaluate, cdp.RuntimeEvaluateParams{
			Expression: "globalThis." + hostAnswer + "(" + string(body) + ")",
			ContextID:  ev.ExecutionContextID,
		}); err != nil && ctx.Err() == nil {
			log.Debug("artifact_render_unanswered", "method", c.Method, "error", err.Error())
		}
	}
}

// call answers one bridge call, as the render's viewer and writing nothing.
func (u *run) call(ctx context.Context, c bridgeCall) (any, *Refusal) {
	needs, known := capabilityOf(c.Method)
	switch {
	case !known:
		return nil, &Refusal{Code: "invalid", Message: "the bridge carries no " + clip(c.Method, 64)}
	case needs != "" && !hasCapability(u.frame.Capabilities, needs):
		return nil, &Refusal{Code: "capability_not_declared", Message: "this page's version " +
			"does not declare the `" + string(needs) + "` capability"}
	}
	p := c.Params
	if p == nil {
		p = map[string]any{}
	}
	switch c.Method {
	case "identity.me":
		return map[string]any{"seat": u.req.Viewer.Seat, "kind": u.req.Viewer.Kind,
			"displayName": u.req.Viewer.DisplayName, "role": string(u.req.Viewer.Role)}, nil
	case "identity.people":
		people := make([]map[string]any, 0, len(u.req.People))
		for _, person := range u.req.People {
			people = append(people, map[string]any{"seat": person.Seat, "kind": person.Kind,
				"displayName": person.DisplayName})
		}
		return people, nil
	case "data.subscribe":
		// A SNAPSHOT: a render is one moment, so the subscription answers
		// the collection as it stands and is told nothing after.
		page, refusal := u.req.Bridge.Read(ctx, "data.list", p)
		if refusal != nil {
			return nil, refusal
		}
		docs := any([]any{})
		if m, ok := page.(map[string]any); ok && m["docs"] != nil {
			docs = m["docs"]
		}
		u.subscriptions++
		return map[string]any{"subscription": u.subscriptions, "docs": docs}, nil
	case "data.unsubscribe", "presence.set":
		// TOLD TO NOBODY: a render is in no room.
		return nil, nil
	case "data.schema", "data.get", "data.list", "data.query", "state.get", "state.list":
		return u.req.Bridge.Read(ctx, c.Method, p)
	case "assets.list":
		return u.assets(ctx)
	case "openLink":
		target, _ := p["url"].(string)
		u.found.bridged(BridgeNote{Method: c.Method, Detail: "asked to open " + clip(target, 200) +
			"; a viewer is shown the address and asked first, and the render opens nothing"})
		return false, nil
	case "print":
		return nil, &Refusal{Code: "unavailable", Message: "this page cannot be printed here"}
	}
	u.found.bridged(BridgeNote{Method: c.Method, Detail: "a write, refused: a render writes " +
		"nothing; for a viewer it would be decided by their role and the collection's rules"})
	return nil, &Refusal{Code: "forbidden", Message: "this render writes nothing"}
}

// hasCapability reports whether granted holds c.
func hasCapability(granted []artifacts.Capability, c artifacts.Capability) bool {
	for _, g := range granted {
		if g == c {
			return true
		}
	}
	return false
}

// assets is the artifact's asset store as a page lists it.
func (u *run) assets(ctx context.Context) (any, *Refusal) {
	list, err := u.r.assets.Assets(ctx, u.req.Head.ID)
	if err != nil {
		return nil, &Refusal{Code: "unavailable", Message: "the asset store could not be read"}
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, map[string]any{"id": a.ID.String(), "name": a.Name,
			"contentType": a.ContentType, "size": a.Object.Size})
	}
	return out, nil
}
