// state.mjs: the box's state of its browser, as the hub reads one: its boot,
// a rev that rises with every document the boot makes, what the browser is
// doing, and its tabs, each address without a user name or password, each
// title without control characters, the whole fitted to its byte bound in
// the contract's order, addresses to origins and then titles, from the last
// tab, measured as the bytes the relay will post. internal/browser/boxhelper/
// live's package doc carries the argument.

// RUNNING is what the browser is doing, by the supervisor's state: a state
// is made only while it is one of these.
export const RUNNING = Object.freeze({ ready: "running", restarting: "restarting", crashed_out: "crashed_out" })

// States makes the documents of one box, rev counted per boot.
export class States {
  constructor(bounds) {
    this.bounds = bounds
    this.boot = undefined
    this.rev = 0
  }

  // make is the next document of boot, running and tabs ({title, url,
  // front, loading, crashed, id}, in order), fitted.
  make(boot, running, tabs) {
    if (boot !== this.boot) {
      this.boot = boot
      this.rev = 0
    }
    this.rev += 1
    const listed = tabs.slice(0, this.bounds.max_tabs)
    const front = tabs.find((tab) => tab.front)
    if (front !== undefined && !listed.includes(front)) listed[listed.length - 1] = front
    const doc = {
      v: 1,
      boot,
      rev: this.rev,
      running,
      tabs: listed.map((tab) => ({
        id: tab.id,
        ...titleOf(tab.title, this.bounds.title_chars),
        ...addressOf(tab.url, this.bounds.url_bytes),
        front: tab.front,
        loading: tab.loading,
        crashed: tab.crashed,
      })),
    }
    return fit(doc, this.bounds.max_bytes)
  }
}

// CONTROLS are C0 and C1 control characters and DEL, which a title loses
// before it is counted.
const CONTROLS = /[\u0000-\u001f\u007f-\u009f]/gu

// titleOf is text as a tab's title: well formed, its controls stripped, cut
// to chars code points, and whether it was cut.
export function titleOf(text, chars) {
  const points = [...String(text ?? "").toWellFormed().replace(CONTROLS, "")]
  if (points.length <= chars) return { title: points.join(""), title_cut: false }
  return { title: points.slice(0, chars).join(""), title_cut: true }
}

// AUTHORITY is the authority of an address as the hub reads one, by its
// bytes (internal/browser's carriesUser): what follows the scheme's // up to
// a path, a query or a fragment. A user name or password is what precedes
// its last @.
const AUTHORITY = /^[A-Za-z][A-Za-z0-9+.-]*:\/\/([^/?#]*)/

// carriesUser is whether address names a user to the hub's reader, whether
// or not the rest of it parses.
export function carriesUser(address) {
  const m = AUTHORITY.exec(address)
  return m !== null && m[1].includes("@")
}

// addressOf is url as a tab's address: no user name or password, and past
// bytes its origin, or its scheme and a colon where it has none, cut.
export function addressOf(url, bytes) {
  let address = String(url ?? "").toWellFormed()
  if (carriesUser(address)) address = withoutUser(address)
  if (Buffer.byteLength(address) <= bytes) return { url: address, url_cut: false }
  return { url: originOf(address), url_cut: true }
}

// withoutUser is address with its user name and password gone, or its
// origin where the URL parser leaves the hub's reader one.
function withoutUser(address) {
  try {
    const u = new URL(address)
    u.username = ""
    u.password = ""
    if (!carriesUser(u.href)) return u.href
  } catch {
    // Not a URL the parser reads: its scheme alone.
  }
  return originOf(address)
}

// originOf is address's origin, or its scheme and a colon where the URL
// parser gives it none.
function originOf(address) {
  try {
    const u = new URL(address)
    if (u.origin !== "null" && !carriesUser(u.origin)) return u.origin
    return u.protocol
  } catch {
    const scheme = /^[A-Za-z][A-Za-z0-9+.-]*:/.exec(address)
    return scheme === null ? "" : scheme[0]
  }
}

// posted is how many bytes doc takes as the relay posts it.
export function posted(doc) {
  return Buffer.byteLength(JSON.stringify(doc))
}

// fit is doc within bytes: each address, from the last tab, replaced by its
// origin, then each title, from the last tab, cut by what it is over, until
// it fits. Origins and empty titles of the most tabs fit far inside the bound.
export function fit(doc, bytes) {
  let over = posted(doc) - bytes
  for (let i = doc.tabs.length - 1; i >= 0 && over > 0; i--) {
    const tab = doc.tabs[i]
    if (tab.url_cut) continue
    const origin = originOf(tab.url)
    if (origin === tab.url) continue
    over -= jsonBytes(tab.url) - jsonBytes(origin)
    tab.url = origin
    tab.url_cut = true
  }
  over = posted(doc) - bytes
  for (let i = doc.tabs.length - 1; i >= 0 && over > 0; i--) {
    const tab = doc.tabs[i]
    const points = [...tab.title]
    let cut = points.length
    while (cut > 0 && over > 0) {
      cut -= 1
      over -= jsonBytes(points[cut]) - 2
    }
    if (cut === points.length) continue
    tab.title = points.slice(0, cut).join("")
    tab.title_cut = true
  }
  return doc
}

// jsonBytes is how many bytes text takes as a JSON string, quotes included.
function jsonBytes(text) {
  return Buffer.byteLength(JSON.stringify(text))
}
