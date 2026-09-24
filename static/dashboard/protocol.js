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
		connected: false,
		authRejected: false
	};
}
var Store = class {
	state = emptyState();
	subs = /* @__PURE__ */ new Map();
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
	} catch {
		return false;
	}
	announce();
	return true;
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
/**
* The symmetric signal: the token CHANGED.
*
* The socket learns through `setToken` + `reconnect`, which the shell calls
* from the dialog. Every REST-backed surface learned nothing at all: it had
* fetched once on mount, so setting a token left `/setup` and `/secrets`
* still showing the refusal that prompted the reader to set one. The screen
* said "needs an operator token", the reader supplied it, and nothing moved.
*
* Fired from `storeToken` and `clearToken` themselves rather than from the
* dialog, so a future writer cannot forget to announce it.
*/
var changed = /* @__PURE__ */ new Set();
/** Subscribe to token changes. Returns the unsubscribe. */
function onTokenChanged(listener) {
	changed.add(listener);
	return () => changed.delete(listener);
}
function announce() {
	for (const listener of changed) listener();
}
/** Forget the stored token. Returns false if the browser refused the write. */
function clearToken() {
	try {
		localStorage.removeItem(TOKEN_KEY);
	} catch {
		return false;
	}
	announce();
	return true;
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
/**
* Every {@link QueryErrorCode}, as a value a rejection's message can be tested
* against.
*
* A Record over the union rather than a list beside it: the compiler refuses a
* key the union lacks and a member left out, so the two cannot drift. The
* union itself is pinned to the engine's own codes by a Go test in
* `internal/api/stream` that reads it.
*/
var QUERY_ERROR_CODES = {
	unknown_query: true,
	unauthorized: true,
	query_failed: true,
	bad_params: true,
	not_found: true,
	unavailable: true,
	timeout: true,
	closed: true
};
/**
* The query error code `value` is, or null for anything else: no failure at
* all, or prose a screen wrote itself.
*
* Branching on the narrowed value is what keeps a screen's handling inside the
* vocabulary. A comparison against a code the union lacks, such as the
* `no_event_store` the engine never sent, is then a type error rather than a
* branch that can never run. `Object.hasOwn`, because `in` would also accept
* `toString` and every other name an object inherits.
*/
function queryErrorCode(value) {
	return value && Object.hasOwn(QUERY_ERROR_CODES, value) ? value : null;
}
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
	* (`not_found`, `unauthorized`, `unavailable`, …), `timeout` if a sent
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
			case "error": this.settle(msg.id, msg.error || "query_failed", null);
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
//#region src/protocol/rest.ts
/**
* The dashboard's one REST transport: every write, and the guarded reads the
* socket has no question for.
*
* The socket remains the data channel for state. This is not a second one: it
* carries the requests that are not questions about state at all. Writes never
* go over the socket, deliberately — its token rides the query string on the
* handshake, and a channel whose credential appears in a proxy log is not
* where a credential-bearing write belongs (see internal/api/auth's own note
* on that). And a handful of reads exist only as REST, `GET /secrets` above
* all, because no query in the registry answers them.
*
* ONE MODULE, for the reason `api.ts` states about itself: a screen reaching
* for its own transport takes its client from somewhere, and the somewhere the
* Fleet screen chose was a context field the shell never populated, so it
* shipped dead. Everything here is a plain function over `fetch` against
* `location.origin`, which is where the dashboard is served from and the only
* origin the engine answers on (it writes no CORS header at all).
*
* Every call carries the operator bearer token. The engine guards `/config`,
* `/secrets` and `/setup` in full, reads included, whatever the anonymous-read
* posture is, so a call with no token is refused rather than silently served.
*/
/**
* What the engine said when it refused.
*
* `status` alone is not enough to act on: the setup surface distinguishes a
* revision that moved under the caller from a config slot holding a literal
* from a fleet with no keyring, and all three are a 409 or a 503. The engine
* answers those with a `code`, and the screen branches on it.
*/
var RestError = class extends Error {
	status;
	code;
	detail;
	hint;
	/** Everything else the body carried, for a caller that needs a field. */
	body;
	constructor(status, body) {
		const code = typeof body.error === "string" ? body.error : "";
		const detail = typeof body.detail === "string" ? body.detail : "";
		super(detail || code || `HTTP ${status}`);
		this.name = "RestError";
		this.status = status;
		this.code = code;
		this.detail = detail;
		this.hint = typeof body.hint === "string" ? body.hint : "";
		this.body = body;
	}
	/** Whether the engine refused the credential rather than the request. */
	get unauthorized() {
		return this.status === 401 || this.status === 403;
	}
	/**
	* Whether this is NOT the engine's own answer: nothing came back (status
	* 0), a body that could not be read (`unreadable_body` — a gateway's HTML
	* page, a 200 cut off part way through), or a status with no engine error
	* code in it. Every refusal the engine writes is JSON with an `error` code,
	* its auth guard's included and its router's own `no_route` and
	* `method_not_allowed` for a route or method it does not serve, so an answer
	* without one was written by something in front of it.
	*
	* WHAT A WRITE'S CALLER NEEDS BEFORE IT READS A REFUSAL AS ONE. A refusal
	* the engine wrote means nothing was done; this means nobody here knows. A
	* reverse proxy's default read timeout is a minute, which is exactly what
	* the engine allows a node gate past its judgement, so a slow eviction
	* reached the browser as a 504 — and read as a refusal, its operation id
	* was dropped with it.
	*/
	get unanswered() {
		return this.status === 0 || this.code === "" || this.code === "unreadable_body";
	}
};
/**
* A refusal that never reached the engine: DNS, a dropped connection, a proxy
* answering HTML. Status 0, so a caller testing `status === 409` cannot
* mistake it for an answer.
*/
function offline(err) {
	return new RestError(0, {
		error: "unreachable",
		detail: err instanceof Error ? err.message : "the engine could not be reached"
	});
}
/**
* How long a request may take before it is abandoned.
*
* A REQUEST THAT NEVER SETTLES NEVER SETTLES, and that is not a slow spinner
* here: every write in this UI runs behind a `busy` flag whose only reset is
* the `finally` of its own await, and every dialog disables its own exits
* while busy — Escape, the veil click and the Cancel button. An unresolved
* fetch was a modal with every way out switched off and a reload as the only
* escape.
*
* ANCHORED TO THE LONGEST ORDINARY PATH THIS API HAS: a setup submission
* seals a credential in the fleet's store, patches the company document,
* validates it whole and advances the epoch, each a round trip of its own.
* Thirty seconds is comfortably above that and comfortably below the point at
* which a person concludes the page is broken.
*
* It is the DEFAULT, not the only deadline: a call whose path is genuinely
* longer passes [RequestOptions.timeoutMs] rather than removing the deadline.
* The node gate is the one that does — the engine allows a gesture a minute
* from its first record to its last answer, so thirty seconds gave up on a
* gesture the node went on to finish, holding nothing to finish it with.
*/
var REQUEST_TIMEOUT_MS = 3e4;
/** Whether a rejection is the caller's own abort rather than a failure. */
function isAbort(err) {
	return typeof err === "object" && err !== null && err.name === "AbortError";
}
/** JSON is `application/json` and every `+json` type, merge patch included. */
function isJson(contentType) {
	const essence = contentType.split(";")[0].trim().toLowerCase();
	return essence === "application/json" || essence.endsWith("+json");
}
/** The path with the caller's query parameters merged into its own. */
function withQuery(path, query) {
	if (!query) return path;
	const at = path.indexOf("?");
	const params = new URLSearchParams(at < 0 ? "" : path.slice(at + 1));
	for (const [key, value] of Object.entries(query)) if (value !== void 0) params.set(key, String(value));
	const qs = params.toString();
	return (at < 0 ? path : path.slice(0, at)) + (qs ? `?${qs}` : "");
}
/**
* The one request path, answering the status and entity-tag as well as the
* body.
*
* NOT CACHED, EVER. Every answer here is either a write or a guarded read of
* something that changes under the reader (a configuration revision, the
* sealed store's names, an integration's requirements), and a heuristic cache
* hit on one of those is a screen showing the company as it was. A 304 still
* reaches the caller, when the caller sent the precondition that asks for it.
*/
async function request(method, path, options = {}) {
	const { body, headers = {}, query, signal, read = "json", timeoutMs = REQUEST_TIMEOUT_MS } = options;
	const contentType = options.contentType ?? (body === void 0 ? void 0 : "application/json");
	let encoded;
	if (body !== void 0) {
		if (contentType && !isJson(contentType)) {
			if (typeof body !== "string") throw new TypeError(`rest.request: a ${contentType} body must be a string`);
			encoded = body;
		} else encoded = JSON.stringify(body);
	}
	if (signal?.aborted) throw signal.reason;
	const token = apiToken();
	const init = {
		method,
		cache: "no-store",
		headers: {
			...token ? { Authorization: "Bearer " + token } : {},
			...contentType ? { "Content-Type": contentType } : {},
			...headers
		},
		...encoded === void 0 ? {} : { body: encoded }
	};
	const controller = new AbortController();
	let timedOut = false;
	const timer = setTimeout(() => {
		timedOut = true;
		controller.abort();
	}, timeoutMs);
	const forward = () => controller.abort(signal?.reason);
	signal?.addEventListener("abort", forward, { once: true });
	const unanswered = (err) => {
		if (signal?.aborted) return signal.reason;
		return timedOut ? new RestError(0, {
			error: "unreachable",
			detail: `the engine did not answer within ${timeoutMs / 1e3} seconds`
		}) : offline(err);
	};
	let response;
	let text;
	try {
		try {
			response = await fetch(location.origin + withQuery(path, query), {
				...init,
				signal: controller.signal
			});
		} catch (err) {
			throw unanswered(err);
		}
		try {
			text = await response.text();
		} catch (err) {
			throw unanswered(err);
		}
	} finally {
		clearTimeout(timer);
		signal?.removeEventListener("abort", forward);
	}
	if (read === "text" && response.ok) return {
		status: response.status,
		body: text,
		etag: response.headers.get("ETag")
	};
	let parsed = null;
	if (text !== "") try {
		parsed = JSON.parse(text);
	} catch {
		throw new RestError(response.ok ? 502 : response.status, {
			error: "unreadable_body",
			detail: response.ok ? "the answer was cut short, or is not the engine's JSON" : `a ${response.status} came back that is not the engine's JSON — something in front of it answered`
		});
	}
	if (!response.ok && response.status !== 304) {
		const refusal = parsed && typeof parsed === "object" ? parsed : {};
		throw new RestError(response.status, refusal);
	}
	return {
		status: response.status,
		body: parsed,
		etag: response.headers.get("ETag")
	};
}
/** The body alone, for the callers that need nothing else. */
async function bodyOf(method, path, options) {
	return (await request(method, path, options)).body;
}
var rest = {
	/**
	* The whole answer: status, entity-tag and body. For a caller that sends a
	* precondition, reads a tag, cancels, or branches on a success status.
	*/
	request,
	get: (path) => bodyOf("GET", path),
	post: (path, body, headers) => bodyOf("POST", path, {
		body: body ?? {},
		headers
	}),
	put: (path, body, headers) => bodyOf("PUT", path, {
		body: body ?? {},
		headers
	}),
	patch: (path, body, headers) => bodyOf("PATCH", path, {
		body: body ?? {},
		headers
	}),
	/**
	* THE BODY IS THE VALUE, not a document carrying one.
	*
	* `PUT /secrets/{name}` takes the credential as raw bytes, deliberately: a
	* credential is arbitrary text — a PEM key has newlines, a token can hold
	* anything — and an encoding step between the operator and the byte
	* sequence the vendor compares is a 401 nobody can explain. Sending it
	* through `put` would seal the JSON quotes into the credential.
	*/
	putText: (path, value) => bodyOf("PUT", path, {
		body: value,
		contentType: "text/plain; charset=utf-8"
	}),
	del: (path, body, headers) => bodyOf("DELETE", path, {
		body,
		headers
	})
};
//#endregion
//#region src/protocol/gate.ts
/**
* The node gate — evicting a node and readmitting one — as the wire knows it:
* the remedy vocabulary a gate answer speaks, the operation id a gesture is
* finished under, and how long a gesture may take to answer.
*
* Every constant here is a COPY of something the engine owns, because this is
* a separate build that cannot import a Go identifier — and each copy is held
* to the engine's by a gate on the engine side (`internal/api`'s
* `gate_client_test.go`), in both directions, so it cannot drift silently. The
* minter is held to a vector file both sides read.
*/
/**
* What an operator does about a log a gesture did not finish, or a gesture
* refused before anything was written — `statelog.GateActions`.
*
* AN ACTION, NEVER A FLAG. The engine used to send one sentence in the command
* line's words ("evict it with -force", "without -op-id"), and this dashboard
* rendered it beside a dialog that has none of those flags. The engine now
* says WHAT to do and each surface says HOW: here, as its own controls.
*/
var GATE_ACTIONS = [
	"retry_same_op",
	"new_gesture",
	"force",
	"other_node",
	"reanchor",
	"set_capacity",
	"restore",
	"wait"
];
/**
* The actions after which the gesture is finished under ITS OWN operation id —
* `statelog.GateActionsKeepingOperation`.
*
* A log refused `log_full` cannot be finished by sending the gesture again at
* once, and a surface that let go of the id then left the operator nothing
* but a FRESH gesture once the ceiling was raised — which writes every log
* that already held the first record again, re-dating each eviction and
* restarting its fence window. So the id is kept for all of these, and only
* `new_gesture` lets it go.
*/
var GATE_ACTIONS_KEEPING_OPERATION = [
	"retry_same_op",
	"other_node",
	"reanchor",
	"set_capacity"
];
/** Whether, once the operator has done `action`, the gesture is finished under
*  its own operation id. */
function keepsOperation(action) {
	return GATE_ACTIONS_KEEPING_OPERATION.includes(action);
}
/**
* How long one gesture's request may take before the dialog gives up on it.
*
* SEVENTY-FIVE SECONDS, the command line's own `gateRequestTimeout` and for its
* reason: the engine bounds a gesture at a minute from its first record to its
* last answer (`engine.GateBudget`), and the judgement before it and the round
* trip around it are a coordination read and a request. Waiting past the
* node's own bound is what makes its answer — every log's outcome — reach the
* operator rather than a client timeout that knows none of it. The default
* thirty seconds gave up on a gesture the node went on to finish.
*/
var GATE_REQUEST_TIMEOUT_MS = 75e3;
/**
* An operation id in the engine's grammar (`statelog.NewOpID`), from its parts.
*
* The UUIDv7 bit layout — the leading 48 bits the Unix millisecond, big-endian,
* then the ten tail bytes with the version nibble 7 written into byte 6 and the
* RFC 9562 variant into byte 8 — then `.` and the name, or the bare uuid for an
* empty one. The millisecond is clamped to the 48 bits the layout holds, as
* the engine's is.
*
* PURE, so the vector file the engine's own test reads
* (`internal/statelog/testdata/opid_vectors.json`) pins it byte for byte: a
* drift here is a failing test rather than a gesture refused `op_id_invalid`,
* or accepted at another instant than the one it was minted at.
*/
function layoutOpID(unixMs, tail, name) {
	if (tail.length !== 10) throw new RangeError("an operation id's tail is ten bytes");
	const ms = Math.min(Math.max(Math.trunc(unixMs), 0), 2 ** 48 - 1);
	const bytes = /* @__PURE__ */ new Uint8Array(16);
	let rest = ms;
	for (let i = 5; i >= 0; i--) {
		bytes[i] = rest % 256;
		rest = Math.floor(rest / 256);
	}
	bytes.set(tail, 6);
	bytes[6] = 112 | bytes[6] & 15;
	bytes[8] = 128 | bytes[8] & 63;
	const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
	const uuid = `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
	return name ? `${uuid}.${name}` : uuid;
}
/**
* A FRESH gesture's operation id, minted in the browser BEFORE the first
* request — exactly as `crewlet retention evict` mints one on the workstation.
*
* # Why the dialog mints it rather than letting the route
*
* The node finishes a gesture whatever happens to the connection, so a request
* that timed out or dropped has very likely done its work — and an id the
* route minted comes back only in the answer that never arrived. The only way
* on was then a second gesture under a fresh id, which writes every log the
* first one reached again. Minted here, the id is in hand before anything is
* sent, and "finish this gesture" is always the same request again.
*
* # Whose clock
*
* The browser's. The id carries its mint instant, which the engine reads to
* decide whether its operation ledger can vouch for a retry, so a browser clock
* far AHEAD of the fleet's could let a retry be re-decided across a ledger
* loss — the same assumption, with the same margin, the engine already makes
* of every node's clock and the command line of the workstation's.
*
* `crypto.getRandomValues` rather than `randomUUID`, because the dashboard is
* served over plain HTTP wherever the engine is, and `randomUUID` exists only
* in a secure context.
*/
function newGateOpID(verb, node, now = Date.now(), random = (bytes) => crypto.getRandomValues(bytes)) {
	return layoutOpID(now, random(/* @__PURE__ */ new Uint8Array(10)), `${verb}-${node}`);
}
//#endregion
export { GATE_ACTIONS, GATE_ACTIONS_KEEPING_OPERATION, GATE_REQUEST_TIMEOUT_MS, LiveSocket, MAX_EVENTS, REQUEST_TIMEOUT_MS, RestError, Store, api, apiToken, clearToken, isAbort, keepsOperation, layoutOpID, newGateOpID, onTokenChanged, onTokenRequested, queryErrorCode, requestToken, rest, storeToken };
