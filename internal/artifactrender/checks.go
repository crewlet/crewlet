package artifactrender

import (
	"encoding/json"
	"strconv"
)

// THE CHECKS, as scripts the engine runs in an isolated world of the page's
// frame: they share its DOM and none of its script, so a page cannot change
// what they find. Each answers by value, bounded by its own caps.

// checkWorld is the isolated world the checks run in.
const checkWorld = "crewlet-render-check"

// describeJS names an element as a reader finds it in their source: its tag,
// its id and its first two classes.
const describeJS = `const describe = (el) => {
  if (!el || !el.tagName) return "";
  let s = el.tagName.toLowerCase();
  if (el.id) s += "#" + el.id;
  const cls = typeof el.className === "string" ? el.className.trim().split(/\s+/).filter(Boolean) : [];
  for (const c of cls.slice(0, 2)) s += "." + c;
  return s.slice(0, 120);
};`

// loadedJS answers whether the frame's document has loaded, after its fonts
// and two frames have, or half a second, whichever is first.
const loadedJS = `(async () => {
  if (document.readyState !== "complete") return { loaded: false };
  const frames = new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
  const later = new Promise((r) => setTimeout(r, 500));
  await Promise.race([Promise.all([document.fonts ? document.fonts.ready : null, frames]), later]);
  return { loaded: true, url: location.href.split("#")[0] };
})()`

// loaded is what loadedJS answers.
type loaded struct {
	Loaded bool   `json:"loaded"`
	URL    string `json:"url"`
}

// stylesJS is the style the page holds that the engine did not serve as a
// file: each <style> element's text, each constructed sheet's rules, and
// each style attribute, bounded.
const stylesJS = `(() => {
  const out = { blocks: [], attrs: [] };
  for (const el of document.querySelectorAll("style")) {
    if (out.blocks.length >= 200) break;
    out.blocks.push((el.textContent || "").slice(0, 262144));
  }
  for (const s of document.adoptedStyleSheets || []) {
    if (out.blocks.length >= 200) break;
    try { out.blocks.push(Array.from(s.cssRules, (r) => r.cssText).join("\n").slice(0, 262144)); } catch (e) {}
  }
  for (const el of document.querySelectorAll("[style]")) {
    if (out.attrs.length >= 1000) break;
    out.attrs.push((el.getAttribute("style") || "").slice(0, 4096));
  }
  return out;
})()`

// styles is what stylesJS answers.
type styles struct {
	Blocks []string `json:"blocks"`
	Attrs  []string `json:"attrs"`
}

// matchesJS counts the elements each selector matches, and describes the
// first: -1 for a selector the page's browser cannot read.
func matchesJS(selectors []string) string {
	arg, _ := json.Marshal(selectors)
	return `(() => {
  ` + describeJS + `
  return ` + string(arg) + `.map((sel) => {
    try {
      const all = document.querySelectorAll(sel);
      return { count: all.length, example: describe(all[0]) };
    } catch (e) { return { count: -1, example: "" }; }
  });
})()`
}

// match is one selector's answer from matchesJS.
type match struct {
	Count   int    `json:"count"`
	Example string `json:"example"`
}

// scrollJS moves the frame's document to s at once, never smoothly, and
// answers whether the element it names exists.
func scrollJS(s Scroll) string {
	sel, _ := json.Marshal(s.Selector)
	return `(() => {
  const se = document.scrollingElement || document.documentElement;
  const sel = ` + string(sel) + `;
  if (sel) {
    let el = null;
    try { el = document.querySelector(sel); } catch (e) { return { missing: true }; }
    if (!el) return { missing: true };
    el.scrollIntoView({ block: "start", inline: "nearest", behavior: "instant" });
    return { missing: false };
  }
  const top = ` + scrollTop(s) + `;
  se.scrollTo({ top, left: 0, behavior: "instant" });
  return { missing: false };
})()`
}

// scrollTop is the expression for a position's offset.
func scrollTop(s Scroll) string {
	if s.Bottom {
		return "se.scrollHeight"
	}
	return strconv.Itoa(s.Pixels)
}

// scrolled is what scrollJS answers.
type scrolled struct {
	Missing bool `json:"missing"`
}

// barUse is an unresolved property a rule's background asks for, which a
// see-through bar the rule matches is told by.
type barUse struct {
	Selector string `json:"selector"`
	Name     string `json:"name"`
}

// shotJS reads the frame where it stands: its sticky and fixed bars, the
// see-through ones among them, and at a position a reader lands on, every
// control or heading a bar covers. uses are the unresolved backgrounds.
func shotJS(atRest bool, uses []barUse) string {
	arg, _ := json.Marshal(uses)
	rest := "false"
	if atRest {
		rest = "true"
	}
	return `(() => {
  ` + describeJS + `
  const uses = ` + string(arg) + `;
  const vw = innerWidth, vh = innerHeight;
  const alpha = (c) => {
    if (!c || c === "transparent") return 0;
    let m = c.match(/\/\s*([\d.]+%?)\s*\)$/) || c.match(/^rgba\([^)]*,\s*([\d.]+%?)\s*\)$/);
    if (!m) return 1;
    return m[1].endsWith("%") ? parseFloat(m[1]) / 100 : parseFloat(m[1]);
  };
  const bars = [];
  let seen = 0;
  for (const el of document.querySelectorAll("body *")) {
    if (++seen > 5000) break;
    const cs = getComputedStyle(el);
    if (cs.position !== "fixed" && cs.position !== "sticky") continue;
    const r = el.getBoundingClientRect();
    if (r.width < 1 || r.height < 1 || r.bottom <= 0 || r.top >= vh) continue;
    if (cs.visibility === "hidden" || cs.display === "none" || parseFloat(cs.opacity) === 0) continue;
    const clear = alpha(cs.backgroundColor) === 0 && cs.backgroundImage === "none" &&
      (!cs.backdropFilter || cs.backdropFilter === "none");
    bars.push({ el, r, clear, position: cs.position, desc: describe(el) });
  }
  const seeThrough = [];
  for (const b of bars) {
    if (!b.clear || b.r.width < vw * 0.5) continue;
    let unresolved = "";
    for (const u of uses) {
      try { if (b.el.matches(u.selector)) { unresolved = u.name; break; } } catch (e) {}
    }
    seeThrough.push({ element: b.desc, position: b.position, unresolved });
  }
  const covered = [];
  if (` + rest + ` && bars.length > 0) {
    let checked = 0;
    const wanted = "a[href],button,input,select,textarea,summary,label,h1,h2,h3,h4,h5,h6,[role=button],[tabindex]";
    for (const el of document.querySelectorAll(wanted)) {
      if (++checked > 2000 || covered.length >= 20) break;
      const r = el.getBoundingClientRect();
      if (r.width < 1 || r.height < 1 || r.bottom <= 0 || r.top >= vh) continue;
      const under = bars.find((b) => !b.el.contains(el) &&
        r.left < b.r.right && r.right > b.r.left && r.top < b.r.bottom && r.bottom > b.r.top);
      if (!under) continue;
      const x = Math.min(Math.max(r.left + Math.min(r.width / 2, 16), 0), vw - 1);
      const y = Math.min(Math.max(r.top + Math.min(r.height / 2, 8), 0), vh - 1);
      const hit = document.elementFromPoint(x, y);
      if (!hit || el.contains(hit) || hit.contains(el)) continue;
      const bar = bars.find((b) => b.el.contains(hit));
      if (bar && !bar.el.contains(el)) covered.push({ element: describe(el), by: bar.desc });
    }
  }
  const se = document.scrollingElement || document.documentElement;
  return { seeThrough, covered, scrollY: Math.round(se.scrollTop), scrollHeight: se.scrollHeight };
})()`
}

// where is what shotJS answers.
type where struct {
	SeeThrough []struct {
		Element    string `json:"element"`
		Position   string `json:"position"`
		Unresolved string `json:"unresolved"`
	} `json:"seeThrough"`
	Covered []struct {
		Element string `json:"element"`
		By      string `json:"by"`
	} `json:"covered"`
	ScrollY      int `json:"scrollY"`
	ScrollHeight int `json:"scrollHeight"`
}

// anchorsJS follows each in-page link's target as the link would, and names
// the ones a sticky or fixed bar then covers.
const anchorsJS = `(() => {
  ` + describeJS + `
  const se = document.scrollingElement || document.documentElement;
  const start = se.scrollTop;
  const out = [];
  const seen = new Set();
  for (const a of document.querySelectorAll('a[href^="#"]')) {
    if (out.length >= 20 || seen.size >= 40) break;
    const id = decodeURIComponent(a.getAttribute("href").slice(1));
    if (!id || seen.has(id)) continue;
    seen.add(id);
    const el = document.getElementById(id);
    if (!el) continue;
    el.scrollIntoView({ block: "start", behavior: "instant" });
    const r = el.getBoundingClientRect();
    const hit = document.elementFromPoint(Math.max(r.left + 4, 0), Math.max(r.top + 4, 0));
    if (!hit || el.contains(hit) || hit.contains(el)) continue;
    let bar = hit;
    while (bar && bar !== document.body) {
      const p = getComputedStyle(bar).position;
      if (p === "fixed" || p === "sticky") break;
      bar = bar.parentElement;
    }
    if (bar && bar !== document.body && !bar.contains(el)) {
      out.push({ element: "#" + id, by: describe(bar) });
    }
  }
  se.scrollTo({ top: start, behavior: "instant" });
  return out;
})()`

// landing is one in-page link target a bar covers.
type landing struct {
	Element string `json:"element"`
	By      string `json:"by"`
}

// linksJS is every link of the page, its own spelling and where it goes.
const linksJS = `(() => {
  const out = [];
  for (const a of document.querySelectorAll("a[href], area[href]")) {
    if (out.length >= 2000) break;
    const href = a.getAttribute("href") || "";
    let target = null;
    if (href.startsWith("#") && href.length > 1) {
      try { target = !!document.getElementById(decodeURIComponent(href.slice(1))); } catch (e) { target = false; }
    }
    out.push({
      href: href.slice(0, 600),
      resolved: (a.href || "").slice(0, 600),
      text: ((a.textContent || a.getAttribute("aria-label") || "").trim()).slice(0, 80),
      blank: (a.getAttribute("target") || "") === "_blank",
      target,
    });
  }
  return out;
})()`

// link is one link as linksJS answers it.
type link struct {
	Href     string `json:"href"`
	Resolved string `json:"resolved"`
	Text     string `json:"text"`
	Blank    bool   `json:"blank"`
	Target   *bool  `json:"target"`
}

// pointJS is the centre of the element sel names, in the frame's viewport,
// scrolled into view where it is not, and what is on top there.
func pointJS(sel string) string {
	arg, _ := json.Marshal(sel)
	return `(() => {
  ` + describeJS + `
  let el = null;
  try { el = document.querySelector(` + string(arg) + `); } catch (e) { return { found: false, invalid: true }; }
  if (!el) return { found: false };
  let r = el.getBoundingClientRect();
  if (r.top < 0 || r.bottom > innerHeight || r.left < 0 || r.right > innerWidth) {
    el.scrollIntoView({ block: "center", inline: "center", behavior: "instant" });
    r = el.getBoundingClientRect();
  }
  if (r.width < 1 || r.height < 1) return { found: true, hidden: true, element: describe(el) };
  const x = r.left + r.width / 2, y = r.top + r.height / 2;
  const hit = document.elementFromPoint(x, y);
  const on = !!hit && (el === hit || el.contains(hit));
  return { found: true, x, y, on, element: describe(el), hit: describe(hit) };
})()`
}

// point is what pointJS answers.
type point struct {
	Found   bool    `json:"found"`
	Invalid bool    `json:"invalid"`
	Hidden  bool    `json:"hidden"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	On      bool    `json:"on"`
	Element string  `json:"element"`
	Hit     string  `json:"hit"`
}

// focusJS focuses the element sel names.
func focusJS(sel string) string {
	arg, _ := json.Marshal(sel)
	return `(() => {
  let el = null;
  try { el = document.querySelector(` + string(arg) + `); } catch (e) { return false; }
  if (!el) return false;
  el.focus({ preventScroll: false });
  return document.activeElement === el;
})()`
}
