//#region src/protocol/store.ts
/**
* Longest activity feed a tab keeps.
*
* Matches the server's own retention (`livestate.EventFeedLimit`) so a
* reconnect's snapshot neither truncates the feed nor leaves rows the server
* cannot resend. Exported because it is also the limit of what anything derived
* from the feed can HONESTLY claim to know: a busy company fills 400 events in
* minutes, and a panel covering an hour has to say where the record actually
* starts rather than drawing the gap as quiet.
*/
var MAX_EVENTS = 400;
var ALL_DATA_SLICES = [
	"agents",
	"events",
	"sandboxes",
	"org",
	"tools",
	"tokens",
	"budget",
	"schedules",
	"health"
];
function emptyState() {
	return {
		agents: [],
		events: [],
		phases: [],
		sandboxes: [],
		org: {},
		tools: [],
		health: { status: "unknown" },
		tokens: null,
		budget: {},
		schedules: null,
		recentRuns: null,
		connected: false,
		authRejected: false
	};
}
var Store = class {
	state = emptyState();
	subs = /* @__PURE__ */ new Map();
	eventSubs = /* @__PURE__ */ new Set();
	/**
	* A monotonic counter per slice.
	*
	* React binds through `useSyncExternalStore`, which compares snapshots by
	* identity and re-renders when they differ. Slices are mutated in place (the
	* arrays are large and pushed at high frequency), so the version is what
	* gives each slice a cheap, stable identity to compare — and it is per-slice
	* rather than global for the same reason the subscriptions are.
	*/
	versions = {};
	version(slice) {
		return this.versions[slice] ?? 0;
	}
	/** Call `fn` when any of `slices` changes. Returns an unsubscribe function. */
	subscribe(slices, fn) {
		for (const slice of slices) {
			let set = this.subs.get(slice);
			if (!set) {
				set = /* @__PURE__ */ new Set();
				this.subs.set(slice, set);
			}
			set.add(fn);
		}
		return () => {
			for (const slice of slices) this.subs.get(slice)?.delete(fn);
		};
	}
	/** Call `fn` for every event envelope, as it arrives. */
	onEvent(fn) {
		this.eventSubs.add(fn);
		return () => {
			this.eventSubs.delete(fn);
		};
	}
	emit(...slices) {
		for (const slice of slices) this.versions[slice] = (this.versions[slice] ?? 0) + 1;
		const called = /* @__PURE__ */ new Set();
		for (const slice of slices) for (const fn of this.subs.get(slice) ?? []) {
			if (called.has(fn)) continue;
			called.add(fn);
			fn();
		}
	}
	applySnapshot(snap) {
		if (!snap) return;
		this.state.agents = snap.agents ?? [];
		this.state.events = (snap.events ?? []).slice(0, 400);
		this.state.sandboxes = snap.sandboxes ?? [];
		this.state.org = snap.org ?? {};
		this.state.tools = snap.tools ?? [];
		if (snap.tokens && snap.tokens.totals) this.state.tokens = snap.tokens;
		this.state.budget = snap.budget ?? {};
		if (snap.schedules) this.state.schedules = snap.schedules;
		if (snap.health) this.state.health = snap.health;
		this.emit(...ALL_DATA_SLICES);
	}
	/** Changed seat overlays, keyed by role. */
	applyAgents(rows) {
		if (!Array.isArray(rows) || rows.length === 0) return;
		const byRole = new Map(rows.map((r) => [r.role, r]));
		this.state.agents = this.state.agents.map((a) => {
			const patch = byRole.get(a.role);
			if (!patch) return a;
			byRole.delete(a.role);
			return {
				...a,
				...patch
			};
		});
		for (const row of byRole.values()) this.state.agents = [...this.state.agents, {
			id: row.role,
			...row
		}];
		this.emit("agents");
	}
	/**
	* The complete seat list, replacing what is on screen.
	*
	* Distinct from `applyAgents`, which merges changed overlays by role: a merge
	* cannot express a deletion, so a revision that removes a role would leave
	* its card rendered until the next reload.
	*/
	applySeats(rows) {
		if (!Array.isArray(rows)) return;
		const live = new Map(this.state.agents.map((a) => [a.role, a]));
		this.state.agents = rows.map((row) => {
			const current = live.get(row.role);
			return current ? {
				...current,
				...row
			} : row;
		});
		this.emit("agents");
	}
	applySandboxes(list) {
		this.state.sandboxes = list ?? [];
		this.emit("sandboxes", "agents");
	}
	applyTokens(rollup) {
		if (!rollup) return;
		this.state.tokens = rollup;
		this.emit("tokens");
	}
	applyBudget(budget) {
		this.state.budget = budget ?? {};
		this.emit("budget");
	}
	applySchedules(payload) {
		if (!payload) return;
		if (payload.schedules) this.state.schedules = payload.schedules;
		if (payload.recent_runs) this.state.recentRuns = payload.recent_runs;
		this.emit("schedules");
	}
	applyOrg(org) {
		this.state.org = org ?? {};
		this.emit("org");
	}
	applyTools(tools) {
		this.state.tools = tools ?? [];
		this.emit("tools");
	}
	applyHealth(health) {
		this.state.health = health ?? { status: "unknown" };
		this.state.connected = !!health && health.status !== "unknown";
		this.emit("health");
	}
	setConnected(value) {
		this.state.connected = value;
		if (!value) this.state.health = { status: "unknown" };
		this.emit("health");
	}
	setAuthRejected(value) {
		const next = !!value;
		if (this.state.authRejected === next) return;
		this.state.authRejected = next;
		this.emit("health");
	}
	applyEvent(ev) {
		if (!ev || !ev.id) return;
		if (ev.category && !this.state.events.some((e) => e.id === ev.id)) {
			this.state.events = [ev, ...this.state.events].slice(0, 400);
			this.emit("events");
		}
		if (ev.type === "agent_phase_completed" && ev.payload) {
			if (!this.state.phases.some((p) => p.id === ev.id)) {
				this.state.phases = [ev, ...this.state.phases].slice(0, 200);
				this.emit("phases");
			}
		}
		for (const fn of this.eventSubs) fn(ev);
	}
	agentById(id) {
		return this.state.agents.find((a) => a.id === id || a.role === id) ?? null;
	}
	/**
	* Resolve a seat by whatever the URL carried.
	*
	* Seats are addressed by HANDLE — the canonical identity everywhere else in
	* the system, and the one an operator can read off a chat mention. Runtime
	* ids and role names still resolve, because links minted before the move
	* used them and they are in people's history.
	*/
	agentByKey(key) {
		if (!key) return null;
		const wanted = String(key);
		const lower = wanted.toLowerCase();
		return this.state.agents.find((a) => a.handle === wanted || a.id === wanted || a.role === wanted || String(a.handle ?? "").toLowerCase() === lower || String(a.role ?? "").toLowerCase() === lower) ?? null;
	}
};
//#endregion
//#region src/protocol/authToken.ts
/**
* The API bearer token the auth-gated screens share.
*
* Configuration and Secrets sit behind the auth middleware, and the socket
* carries the same credential on its handshake and on every query frame. One
* token between them, so setting it anywhere unlocks everything.
*
* Asking for it is the shell's job, over a real dialog. This module only knows
* how to read and write it: the prompting used to live here as a
* `window.prompt`, which meant a request for a credential arrived in a
* chrome-drawn box that could not say who was asking or why.
*/
var TOKEN_KEY = "crewlet_api_token";
/** The stored token, or "". Never throws. */
function apiToken() {
	try {
		return localStorage.getItem(TOKEN_KEY) ?? "";
	} catch {
		return "";
	}
}
/**
* Persist a token. Returns false if the browser refused the write.
*
* The caller has to know: a silently-unsaved token works until the next reload
* and is then unauthenticated again, with nothing on screen to explain why.
*/
function storeToken(token) {
	try {
		localStorage.setItem(TOKEN_KEY, String(token ?? "").trim());
		return true;
	} catch {
		return false;
	}
}
var listeners = /* @__PURE__ */ new Set();
/** Ask for the token dialog. A no-op when no shell is mounted. */
function requestToken() {
	for (const listener of listeners) listener();
}
/** Subscribe the shell. Returns the unsubscribe. */
function onTokenRequested(listener) {
	listeners.add(listener);
	return () => listeners.delete(listener);
}
/** Forget the stored token. Returns false if the browser refused the write. */
function clearToken() {
	try {
		localStorage.removeItem(TOKEN_KEY);
		return true;
	} catch {
		return false;
	}
}
//#endregion
//#region src/protocol/api.ts
/**
* The one HTTP read the dashboard still makes.
*
* Everything else goes over the WebSocket — state arrives as pushes and
* anything on demand is a query on the same socket. This remains for exactly
* one case: a browser that cannot upgrade to a WebSocket at all, usually a
* corporate proxy. While the socket is down the client polls this snapshot so
* the page keeps telling the truth, and it stops the moment the socket is back.
*
* It had a second entry once, and that one is why the Fleet screen shipped
* dead: a screen reaching for its own transport takes its client from
* somewhere, and the somewhere it chose was a context field the shell never
* populated. There is one transport for reads, and only `socket.ts` imports
* this file.
*
* The REST API itself is much larger than this — it is a public read surface
* documented in docs/reference/api-endpoints.md. The dashboard simply does not
* use it.
*/
var api = { 
/**
* The degraded-mode snapshot, or `null` if it could not be read.
*
* `null` and not an `{_error}` object. That shape was tried: the caller
* guarded with `!snap._error`, which is TRUE for zero, so the one case this
* whole fallback exists for — the network completely gone — applied the
* error object as if it were a snapshot and replaced agents, events,
* sandboxes, org and tools with empties. The page went blank at the exact
* moment the last state it received was the only thing it had.
*/
async snapshot() {
	try {
		const stored = apiToken();
		const response = await fetch(location.origin + "/stream/snapshot", stored ? { headers: { Authorization: "Bearer " + stored } } : void 0);
		if (!response.ok) return null;
		return await response.json();
	} catch {
		return null;
	}
} };
//#endregion
//#region src/protocol/socket.ts
/**
* The dashboard's single connection to the engine.
*
* State arrives as pushes (`snapshot`, then `agents` / `seats` / `sandboxes` /
* `tokens` / `budget` / `event` / `org` / `tools` / `schedules` / `health`), and
* anything the dashboard needs on demand — a seat's phase history, one event's
* payload, a trace, a different spend window, the configuration document — is
* asked for over the same socket and answered on it.
*
* There are no HTTP fetches in normal operation. The REST snapshot is used for
* exactly one thing: keeping the page honest while the socket is down (a proxy
* that refuses to upgrade, a restarting engine), and it stops the moment the
* socket is back.
*/
var PATH = "/ws/stream";
/**
* `policy violation` — a refusal delivered as a close FRAME, which is only
* reachable once a connection has opened. The engine does not currently refuse
* anyone that late (it answers 401 to the handshake instead, see
* `probeRefusal`), so nothing here fires today; it is honoured because a close
* code meaning "your credential stopped being good" is the one a long-lived
* socket would use.
*/
var CLOSE_UNAUTHORIZED = 1008;
/**
* Reconnect backoff ceiling. Long enough that a dashboard left open against a
* stopped engine is not hammering it, short enough that bringing the engine
* back feels immediate.
*/
var MAX_BACKOFF_MS = 3e4;
/** Application-level keepalive, comfortably inside the 60 s idle timeout most reverse proxies apply. */
var PING_MS = 25e3;
/** Degraded-mode poll — only ever runs while the socket is down. */
var FALLBACK_MS = 5e3;
/**
* How long a query waits for its answer ONCE SENT.
*
* The clock starts when the frame goes out, not when the query is made, so time
* spent waiting for a socket is not counted against the server. Ten seconds is
* far beyond the slowest query's normal latency and still short enough that a
* screen shows an error rather than an eternal skeleton.
*/
var QUERY_TIMEOUT_MS = 1e4;
var LiveSocket = class {
	store;
	sock = null;
	attempt = 0;
	reconnectTimer = 0;
	pingTimer = 0;
	fallbackTimer = 0;
	isClosed = false;
	nextQueryId = 1;
	inflight = /* @__PURE__ */ new Map();
	token = "";
	/** Whether the shell has already been asked to collect a token. */
	askedForToken = false;
	authRejectedHandler = null;
	constructor(store) {
		this.store = store;
	}
	/** Operator bearer token, sent on the handshake and with every query frame. */
	setToken(token) {
		this.token = token || "";
		if (this.token) this.askedForToken = false;
	}
	start() {
		this.connect();
	}
	/**
	* Drop this socket and re-dial immediately.
	*
	* A dropped envelope is gone: the server's per-client queue discards the
	* OLDEST frame under backpressure, so a lost `agents` overlay is never
	* re-sent and the only true repair is a fresh handshake snapshot.
	*/
	reconnect() {
		if (this.sock) this.sock.close();
		else this.connect();
	}
	stop() {
		this.isClosed = true;
		clearTimeout(this.reconnectTimer);
		this.stopPing();
		this.stopFallback();
		this.failInflight("closed");
		if (this.sock) this.sock.close();
	}
	get connected() {
		return !!this.sock && this.sock.readyState === WebSocket.OPEN;
	}
	/**
	* Ask the server for something and resolve with its reply.
	*
	* A query made before the socket is open, or while it is reconnecting,
	* **waits** for the connection rather than failing. That matters more than it
	* sounds: a screen issues its first query as the page boots, so rejecting
	* when not-yet-connected meant every deep link and every reload rendered
	* "could not load" and stayed there. Queries are pure reads, so one that was
	* in flight when the socket dropped is simply re-sent on reconnect.
	*
	* Rejects with an Error carrying the server's machine-readable code
	* (`not_found`, `unauthorized`, `no_event_store`, …), `timeout` if a sent
	* query goes unanswered, or `closed` if the client shuts down.
	*/
	query(what, params) {
		const id = this.nextQueryId++;
		return new Promise((resolve, reject) => {
			const entry = {
				id,
				what,
				params: params ?? {},
				resolve,
				reject,
				timer: 0
			};
			this.inflight.set(id, entry);
			this.sendQuery(entry);
		});
	}
	sendQuery(entry) {
		if (!this.connected || !this.sock) return;
		const frame = {
			kind: "query",
			id: entry.id,
			what: entry.what,
			params: entry.params
		};
		if (this.token) frame.token = this.token;
		try {
			this.sock.send(JSON.stringify(frame));
		} catch {
			return;
		}
		clearTimeout(entry.timer);
		entry.timer = setTimeout(() => {
			this.inflight.delete(entry.id);
			entry.reject(/* @__PURE__ */ new Error("timeout"));
		}, QUERY_TIMEOUT_MS);
	}
	flushQueries() {
		for (const entry of this.inflight.values()) this.sendQuery(entry);
	}
	connect() {
		if (this.sock && (this.sock.readyState === WebSocket.OPEN || this.sock.readyState === WebSocket.CONNECTING)) return;
		const proto = location.protocol === "https:" ? "wss" : "ws";
		const token = this.token || apiToken();
		const qs = token ? `?token=${encodeURIComponent(token)}` : "";
		let sock;
		try {
			sock = new WebSocket(`${proto}://${location.host}${PATH}${qs}`);
		} catch {
			this.scheduleReconnect();
			return;
		}
		this.sock = sock;
		let handshakeCompleted = false;
		sock.onopen = () => {
			handshakeCompleted = true;
			this.attempt = 0;
			this.store.setAuthRejected(false);
			this.store.setConnected(true);
			clearTimeout(this.reconnectTimer);
			this.stopFallback();
			this.startPing();
			this.flushQueries();
		};
		sock.onmessage = (e) => this.onMessage(String(e.data));
		sock.onclose = (e) => {
			this.stopPing();
			this.sock = null;
			for (const entry of this.inflight.values()) {
				clearTimeout(entry.timer);
				entry.timer = 0;
			}
			this.store.setConnected(false);
			this.scheduleReconnect();
			this.startFallback();
			if (e && e.code === CLOSE_UNAUTHORIZED) this.authRejected();
			else if (!handshakeCompleted) this.probeRefusal();
		};
		sock.onerror = () => {
			this.store.setConnected(false);
		};
	}
	/**
	* Ask, over plain HTTP, whether that dial was refused or merely failed.
	*
	* A handshake the engine answers 401 NEVER reaches this page as close(1008).
	* A close code travels in a close frame, and a connection that never opened
	* has no frames — so the browser reports 1006, the same code it gives for an
	* engine that is simply down, and withholds the status deliberately (a page
	* that could read it could use a socket to scan ports it cannot otherwise
	* reach).
	*
	* This client believed otherwise once, and the whole repair path — the
	* banner, the dialog, "forget this token" — hung off a code that never
	* arrived. A wrong token in `localStorage` therefore produced a dashboard
	* that reconnected for ever, said "retrying", and offered no way to correct
	* the one thing that was wrong.
	*
	* So the status is fetched where a browser will hand it over. A plain GET of
	* the same path runs the same guard and stops one line short of the upgrade:
	* 401 is a refused credential, 426 (Upgrade Required) means it was accepted
	* and only the missing header stopped it. A throw is the network, which is
	* not an auth problem and must not raise a dialog.
	*
	* The credential goes in the HEADER here, not the query string the handshake
	* is forced to use: a fetch can set one, and a token in a URL is a token in
	* every proxy's access log.
	*/
	async probeRefusal() {
		if (this.isClosed) return;
		const token = this.token || apiToken();
		try {
			if ((await fetch(PATH, {
				headers: token ? { Authorization: "Bearer " + token } : {},
				cache: "no-store"
			})).status === 401) this.authRejected();
		} catch {}
	}
	/**
	* The engine refused this browser's credential.
	*
	* Two things happen, and both are needed. Asking for a token is the repair —
	* the dashboard is served unauthenticated by design (the page that asks for a
	* token cannot itself require one), so the browser has no other moment to
	* learn it needs one. The store flag is what happens when the reader
	* dismisses that request: the ask fires once and only once, deliberately, so
	* a 30-second reconnect backoff does not reopen a dialog forever — which
	* leaves the page looking like an outage unless the chrome can say otherwise.
	*
	* The socket does not own the asking. It cannot: the dialog belongs to the
	* shell, and a transport that reaches into the DOM to draw one is a transport
	* that cannot be tested without a browser.
	*/
	authRejected() {
		this.store.setAuthRejected(true);
		if (this.askedForToken || !this.authRejectedHandler) return;
		this.askedForToken = true;
		this.authRejectedHandler();
	}
	/** Register what to do the first time the engine refuses a credential. */
	onAuthRejected(fn) {
		this.authRejectedHandler = fn;
	}
	/**
	* The dispatch table.
	*
	* Public because the e2e replay reaches it directly: `internal/e2e` captures
	* the frames a real company's socket produced and pushes them through THIS
	* function, so the gate asks "does the client understand what the server
	* sent" rather than "did the server send something". Re-implementing the
	* switch in the replay would let it agree with a server the dashboard does
	* not.
	*/
	onMessage(raw) {
		let msg;
		try {
			msg = JSON.parse(raw);
		} catch {
			return;
		}
		switch (msg.kind) {
			case "snapshot":
				this.store.applySnapshot(msg.data);
				break;
			case "event":
				this.store.applyEvent(msg.data);
				break;
			case "agents":
				this.store.applyAgents(msg.data);
				break;
			case "seats":
				this.store.applySeats(msg.data);
				break;
			case "sandboxes":
				this.store.applySandboxes(msg.data);
				break;
			case "tokens":
				this.store.applyTokens(msg.data);
				break;
			case "budget":
				this.store.applyBudget(msg.data);
				break;
			case "schedules":
				this.store.applySchedules(msg.data);
				break;
			case "org":
				this.store.applyOrg(msg.data);
				break;
			case "tools":
				this.store.applyTools(msg.data);
				break;
			case "health":
				this.store.applyHealth(msg.data);
				break;
			case "result":
				this.settle(msg.id, null, msg.data);
				break;
			case "error": this.settle(msg.id, msg.error || "error", null);
		}
	}
	settle(id, error, data) {
		if (id === void 0) return;
		const entry = this.inflight.get(id);
		if (!entry) return;
		this.inflight.delete(id);
		clearTimeout(entry.timer);
		if (error) entry.reject(new Error(error));
		else entry.resolve(data);
	}
	failInflight(reason) {
		for (const entry of this.inflight.values()) {
			clearTimeout(entry.timer);
			entry.reject(new Error(reason));
		}
		this.inflight.clear();
	}
	scheduleReconnect() {
		if (this.isClosed) return;
		clearTimeout(this.reconnectTimer);
		const delay = Math.min(1e3 * 2 ** Math.min(this.attempt, 10), MAX_BACKOFF_MS);
		this.attempt++;
		this.reconnectTimer = setTimeout(() => this.connect(), delay);
	}
	startPing() {
		this.stopPing();
		this.pingTimer = setInterval(() => {
			if (this.connected && this.sock) try {
				this.sock.send(JSON.stringify({ kind: "ping" }));
			} catch {}
		}, PING_MS);
	}
	stopPing() {
		clearInterval(this.pingTimer);
		this.pingTimer = 0;
	}
	startFallback() {
		if (this.isClosed || this.fallbackTimer) return;
		this.fallbackFetch();
		this.fallbackTimer = setInterval(() => void this.fallbackFetch(), FALLBACK_MS);
	}
	stopFallback() {
		clearInterval(this.fallbackTimer);
		this.fallbackTimer = 0;
	}
	async fallbackFetch() {
		const snap = await api.snapshot();
		if (this.connected) return;
		if (snap) this.store.applySnapshot(snap);
	}
};
//#endregion
export { LiveSocket, MAX_EVENTS, Store, api, apiToken, clearToken, onTokenRequested, requestToken, storeToken };
