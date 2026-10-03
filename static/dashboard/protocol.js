//#region src/contract/wire.ts
/**
* How long a query waits for its answer ONCE SENT.
*
* The clock starts when the frame goes out, not when the query is made, so time
* spent waiting for a socket is not counted against the server. Ten seconds is
* far beyond the slowest query's normal latency and still short enough that a
* screen shows an error rather than an eternal skeleton.
*
* AND LONGER THAN THE ENGINE'S OWN READ OF THE BROKER GROUP
* (`engine.BrokerReadWait`), held there by `internal/api`'s
* `TestTheDashboardWaitsPastAReadOfTheGroup`: the `fleet_broker` query asks a
* member for the metadata group and may spend that long on it, and a screen
* that gave up first would report a slow answer as a failed one.
*/
var QUERY_TIMEOUT_MS = 1e4;
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
		org: null,
		tools: [],
		health: { status: "unknown" },
		tokens: null,
		budget: null,
		schedules: null,
		connected: false,
		authRejected: false
	};
}
var Store = class {
	state = emptyState();
	/**
	* The push kinds this build does not know, each with how many arrived.
	*
	* IGNORED AND COUNTED. A fleet part way through an upgrade has a node
	* pushing a kind this bundle was built before, and throwing on it — or
	* applying it to a slice by a guess — would break a screen over a frame it
	* has no use for. But the same fall-through is exactly what this build's
	* own engine sending a kind its own client forgot looks like, which is the
	* silent failure the e2e replay exists to catch: so it is kept here, and
	* the replay asserts it is empty. Not a slice, because nothing renders it
	* and a listener woken by a frame nobody can read would be woken for
	* nothing.
	*/
	unknownPushes = /* @__PURE__ */ new Map();
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
		this.state.budget = snap.budget ?? null;
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
		this.state.budget = budget ?? null;
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
	/** Count one frame whose `kind` this build does not dispatch. */
	noteUnknownPush(kind) {
		const name = typeof kind === "string" ? kind : JSON.stringify(kind ?? null);
		this.unknownPushes.set(name, (this.unknownPushes.get(name) ?? 0) + 1);
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
var TOKEN_KEY = {
	/** The API token this browser presents (`protocol/authToken.ts`). */
	apiToken: "crewlet_api_token",
	/** Light, dark or the system's (`lib/prefs.ts`). */
	theme: "crewlet_theme",
	/** Compact, normal or comfortable (`lib/prefs.ts`). */
	density: "crewlet_density",
	/** The zone timestamps are drawn in; empty is the browser's (`lib/prefs.ts`). */
	timezone: "crewlet_timezone",
	/** How a date is written (`lib/prefs.ts`). */
	dateFormat: "crewlet_date_format",
	/** Objects this reader opened, for the palette (`lib/recents.ts`). */
	recents: "crewlet_recents",
	/** Pages this reader starred, for the sidebar (`lib/starred.ts`). */
	starred: "crewlet_starred",
	/** The width the reader dragged the detail rail to (`app/frame/DetailRail.tsx`). */
	peekWidth: "crewlet.peek.width",
	/** An org draft not yet saved (`routes/org/builder/model/persistence.ts`). */
	orgDraft: "crewlet_org_draft",
	/** The folders open in the Knowledge tree, for this tab (`routes/knowledge/KnowledgeTree.tsx`). */
	knowledgeOpen: "crewlet.knowledge.open"
}.apiToken;
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
* The degraded-mode snapshot, over HTTP.
*
* The socket carries the projection and every question the query registry
* answers. This remains for exactly one case: a socket that cannot connect
* at all — usually a corporate proxy refusing the upgrade, or an engine
* restarting. While the socket is down the client polls this snapshot so the
* page keeps telling the truth, and it stops the moment the socket is back.
*
* It is not the dashboard's only HTTP. Writes and the guarded reads no query
* answers (`/secrets`, `/setup`, `/config`) go over REST through `rest.ts`,
* and a screen reads those through `lib/useRest.ts`. What this file keeps is
* its own separation: it had a second entry once, and that one is why the
* Fleet screen shipped dead — a screen reaching for its own transport takes
* its client from somewhere, and the somewhere it chose was a context field
* the shell never populated. Only `socket.ts` imports this file.
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
* The socket is the channel for the projection and for every question the
* query registry answers — not for everything. Writes and the guarded reads no
* query answers (`/secrets`, `/setup`, `/config`) go over REST through
* `rest.ts`, and this file makes two HTTP requests of its own: the degraded
* snapshot, which keeps the page honest while the socket is down (a proxy that
* refuses to upgrade, a restarting engine) and stops the moment the socket is
* back, and the refusal probe after a handshake that never opened.
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
/**
* A query the engine refused, with the wait it asked for.
*
* AN ERROR WHOSE MESSAGE IS THE CODE, so every caller that reads a refusal by
* [queryErrorCode] of its message keeps working, and a TYPE beside it because
* the wait is a number and the text of an error is no place to carry one: the
* `unavailable` frame carries `retry_after_seconds`, computed by the same
* engine helper as the REST 503's `Retry-After`, and a transport that dropped
* it left each screen to guess a wait of its own.
*/
var QueryError = class extends Error {
	/**
	* The engine's wait in seconds, on an `unavailable` refusal that named one,
	* and null everywhere else — the socket's own `timeout` and `closed`
	* included, which no engine said anything about.
	*/
	retryAfterSeconds;
	/**
	* The refusal's own sentence on a `bad_params` refusal — the parameter to
	* change and what it accepts — and null everywhere else. The engine writes
	* that one refusal FOR the caller and keeps every other failure's text in
	* its log, so this is never a path or a driver's message.
	*/
	detail;
	constructor(code, retryAfterSeconds = null, detail = null) {
		super(code);
		this.name = "QueryError";
		this.retryAfterSeconds = retryAfterSeconds;
		this.detail = detail;
	}
};
/** The engine's wait on a refusal, or null where it named none. */
function retryHint(value) {
	return typeof value === "number" && Number.isFinite(value) && value >= 0 ? value : null;
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
			entry.reject(new QueryError("timeout"));
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
			case "error":
				this.settle(msg.id, msg.error || "query_failed", null, retryHint(msg.retry_after_seconds), typeof msg.detail === "string" && msg.detail ? msg.detail : null);
				break;
			case "pong": break;
			default: this.store.noteUnknownPush(msg.kind);
		}
	}
	settle(id, error, data, retryAfterSeconds = null, detail = null) {
		if (id === void 0) return;
		const entry = this.inflight.get(id);
		if (!entry) return;
		this.inflight.delete(id);
		clearTimeout(entry.timer);
		if (error) entry.reject(new QueryError(error, retryAfterSeconds, detail));
		else entry.resolve(data);
	}
	failInflight(reason) {
		for (const entry of this.inflight.values()) {
			clearTimeout(entry.timer);
			entry.reject(new QueryError(reason));
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
* The socket remains the data channel for the projection and for every
* question in the query registry. This is not a second one: it carries the
* requests that are not questions about state at all, and the reads that no
* query answers. A screen does not call it for a read directly — it reads
* through `lib/useRest.ts`, the one loader, so that a superseded answer, an
* unmounted screen, a changed token and a server's `Retry-After` are each
* handled once. Writes never
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
	/**
	* How long the engine asked the caller to wait before asking again, in
	* seconds, from the refusal's `Retry-After` header — or null where it named
	* no wait.
	*
	* THE ONLY RETRY SIGNAL A REST READ HAS. A 503 is not one on its own: the
	* engine answers 503 both for a node draining or catching up, which a wait
	* clears and which carries the header, and for a node with no keyring,
	* which no wait clears and which does not. The header is the engine saying
	* which, so a loader retries exactly when it is present.
	*/
	retryAfterSeconds;
	constructor(status, body, retryAfter = null) {
		const code = typeof body.error === "string" ? body.error : "";
		const detail = typeof body.detail === "string" ? body.detail : "";
		super(detail || code || `HTTP ${status}`);
		this.name = "RestError";
		this.status = status;
		this.code = code;
		this.detail = detail;
		this.hint = typeof body.hint === "string" ? body.hint : "";
		this.body = body;
		this.retryAfterSeconds = retryAfterSeconds(retryAfter, Date.now());
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
	* reverse proxy's default read timeout is a minute, shorter than the minute
	* and a quarter the engine allows a node gate past its judgement (a minute
	* for the logs, a quarter of one for the estate map's part), so a slow
	* eviction reached the browser as a 504 — and read as a refusal, its
	* operation id was dropped with it.
	*/
	get unanswered() {
		return this.status === 0 || this.code === "" || this.code === "unreadable_body";
	}
};
/**
* A `Retry-After` header value as whole seconds from `now`, or null for a
* header that is absent or says nothing usable.
*
* BOTH FORMS RFC 9110 ALLOWS. The engine writes delay-seconds, but a proxy in
* front of it may answer for it with an HTTP-date, and reading that as "no
* hint" would drop the one instruction the refusal carried. A date already
* past is a wait of zero, never a negative one.
*/
function retryAfterSeconds(header, now) {
	const value = header?.trim() ?? "";
	if (value === "") return null;
	if (/^\d+$/.test(value)) return Number(value);
	if (!/[A-Za-z]/.test(value)) return null;
	const at = Date.parse(value);
	if (Number.isNaN(at)) return null;
	return Math.max(0, Math.ceil((at - now) / 1e3));
}
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
* Two do. A backup copies the whole store before it answers. And the node
* gate takes up to a minute and three quarters to answer a gesture (half a
* minute to judge it, a minute to write every log, a quarter of one for the
* estate map's part), so thirty seconds gave up on a gesture the node went on
* to finish, holding nothing to finish it with.
*/
var REQUEST_TIMEOUT_MS = 3e4;
/** A deadline as a person would say it: seconds under two minutes, else
*  minutes. */
function waitWords(ms) {
	const seconds = Math.round(ms / 1e3);
	return seconds < 120 ? `${seconds} seconds` : `${Math.round(seconds / 60)} minutes`;
}
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
			detail: `the engine did not answer within ${waitWords(timeoutMs)}`
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
		}, response.ok ? null : response.headers.get("Retry-After"));
	}
	if (!response.ok && response.status !== 304) {
		const refusal = parsed && typeof parsed === "object" ? parsed : {};
		throw new RestError(response.status, refusal, response.headers.get("Retry-After"));
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
/**
* How long a download may take, from its request to its last byte.
*
* FIFTEEN MINUTES, not [REQUEST_TIMEOUT_MS]: a project's largest file is a
* gibibyte, and that is fourteen minutes at ten megabits a second. A deadline
* sized for a JSON answer would abandon every large download part way.
*/
var DOWNLOAD_TIMEOUT_MS = 9e5;
/**
* A file's bytes, fetched with the operator's token.
*
* NOT A LINK. A plain `<a href>` carries no bearer token, so on an engine that
* guards its reads it would download the refusal instead of the file — the
* bytes are fetched here, where the token is, and handed to the caller as a
* blob. A refusal is read as the engine's JSON, like every other.
*/
async function blob(path, signal) {
	const token = apiToken();
	const controller = new AbortController();
	const timer = setTimeout(() => controller.abort(), DOWNLOAD_TIMEOUT_MS);
	const forward = () => controller.abort(signal?.reason);
	signal?.addEventListener("abort", forward, { once: true });
	try {
		let response;
		try {
			response = await fetch(location.origin + path, {
				method: "GET",
				cache: "no-store",
				headers: token ? { Authorization: "Bearer " + token } : {},
				signal: controller.signal
			});
		} catch (err) {
			if (signal?.aborted) throw signal.reason;
			throw offline(err);
		}
		if (!response.ok) {
			let body = {};
			try {
				body = await response.json();
			} catch {
				body = { error: "unreadable_body" };
			}
			throw new RestError(response.status, body);
		}
		try {
			return await response.blob();
		} catch (err) {
			throw offline(err);
		}
	} finally {
		clearTimeout(timer);
		signal?.removeEventListener("abort", forward);
	}
}
var rest = {
	/**
	* The whole answer: status, entity-tag and body. For a caller that sends a
	* precondition, reads a tag, cancels, or branches on a success status.
	*/
	request,
	/** A read's body. `signal` is the loader's: `lib/useRest.ts` aborts a read
	*  it superseded, and a screen reads through that rather than calling this. */
	get: (path, signal) => bodyOf("GET", path, { signal }),
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
	}),
	/** A file's bytes — see [blob]. */
	blob
};
//#endregion
//#region src/contract/gate.ts
/**
* The actions after which the gesture is finished under ITS OWN operation id —
* `statelog.GateActionsKeepingOperation`, held by
* `internal/api.TestTheDashboardKeepsTheGesturesOwnIDWhereTheEngineDoes`.
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
//#endregion
//#region src/protocol/gate.ts
/**
* The node gate — evicting a node and readmitting one — as the wire knows it:
* the remedy vocabulary a gate answer speaks, the operation id a gesture is
* finished under, and what a remedy means for that id.
*
* The engine's own values — the actions, which of them keep the id, and how
* long a gesture may take — are declared in `../contract/gate.ts`, the one
* home of what an engine gate holds (`internal/api`'s `gate_client_test.go`);
* what is DONE with them is here. RELATIVE, because this directory is also
* built alone as `protocol.js`, where the `~` alias does not exist. The
* minter is held to a vector file both sides read.
*/
/** Whether, once the operator has done `action`, the gesture is finished under
*  its own operation id ([GATE_ACTIONS_KEEPING_OPERATION]). */
function keepsOperation(action) {
	return GATE_ACTIONS_KEEPING_OPERATION.includes(action);
}
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
//#region src/contract/fleet.ts
/**
* The fleet's own maps and broker, as the engine bounds them: the kinds a
* node's broker can be and the disagreements named between the two records of
* its membership, the states of the estate map and of a partition's holders,
* how long a removal may take, and the lengths a hold is offered at.
*
* Each is a COPY of something the engine owns, because this is a separate
* build that cannot import a Go identifier — held to the engine's in both
* directions by `internal/api`'s `broker_client_test.go`,
* `estate_client_test.go` and `objects_test.go`. What is done with them is
* behaviour, and lives in `protocol/broker.ts`, `protocol/estate.ts` and the
* Settings screens.
*/
/**
* How a node's broker takes part in the fleet's — `placement.BrokerKind`, as
* `String()` renders it.
*
* `unknown` IS A VALUE, not an absence: a node running a build older than the
* field says nothing, and the engine renders that as `unknown` so the cell is
* never empty — an empty cell reads as nothing to look at, and this one is
* counted as a member wherever that is the safe reading.
*/
var BROKER_KINDS = [
	"member",
	"leaf",
	"client",
	"unknown"
];
/**
* The disagreements between the two records of the broker's membership —
* `engine.BrokerFindingKinds`.
*/
var BROKER_FINDING_KINDS = [
	"dead_member",
	"not_in_group",
	"unknown_kind"
];
/**
* How long a removal's request may take before the dialog gives up on it.
*
* THREE MINUTES AND FIVE SECONDS, the command line's own wait and for its
* reason: the node bounds the whole removal by one deadline
* (`engine.BrokerRemoveWait`, two minutes and five seconds) — the carrying
* member's own commit budget and one round trip for its answer — and a minute
* more covers the node listing the fleet and reaching the member. A dialog
* that gave up first would report a removal the group went on to commit as
* failed.
*/
var BROKER_REMOVE_TIMEOUT_MS = 185e3;
/**
* Which of the five things the estate was when the question was asked —
* `queries.EstateMapStates`. `whole` is layout 0: every data node holds the
* whole estate and there is no map.
*/
var ESTATE_MAP_STATES = [
	"unavailable",
	"whole",
	"no_map",
	"unreadable",
	"placed"
];
/** What the MAP says a holder is doing with a partition — `partmap.HolderStates`. */
var HOLDER_STATES = [
	"joining",
	"serving",
	"leaving"
];
/**
* What a node's own estate lease says it is doing with a partition —
* `partmap.PartitionStates`. Kept a string on the wire, so a state a newer
* node reports is shown rather than dropped.
*/
var PARTITION_STATES = [
	"adopting",
	"catching_up",
	"serving",
	"faulted",
	"draining",
	"released"
];
//#endregion
//#region src/contract/actions.ts
/**
* Every change the dashboard makes, as the tool the engine runs for it.
*
* The dashboard writes through ONE route — `POST /operator/act/{tool}`, as the
* person the presented token is bound to (ADR-0024) — and the tools behind it
* are the operator catalogue's, the same implementations a person's own
* assistant calls. This table is what the dashboard is allowed to send there:
* `protocol/act.ts` takes only a key of it, and only the arguments its row
* names.
*
* ONE ROW PER TOOL A CONTROL PRESSES, and no other: `app/source.test.ts` holds
* the keys to the `useAct("…")` literals outside the suites, both ways, so a
* row lands with the control that sends it rather than ahead of it.
*
* Each row is:
*
*  - `args` — the arguments a screen may pass. Every one is a property of the
*    tool's own schema, and every property the schema REQUIRES is here;
*  - `domain` — the log the write lands in, whose position raises this tab's
*    read floor for that domain (`protocol/session.ts`), or `null` for a
*    write that lands in no log a question reads;
*  - `refreshes` — questions OUTSIDE that domain's session set
*    (`contract/domains.ts`) that the write also moves, asked again without a
*    floor because they cannot take one;
*  - `scope` — `person` for a change any bound person makes to the company's
*    work in their own name, `company` for one that rewrites what every seat's
*    tracker means (a project's settings, the catalogue), which a screen
*    offers only where it says so.
*
* HELD AGAINST THE REAL CATALOGUE by `internal/api/operator`'s
* `TestEveryActionTheDashboardTakesIsOneTheActTransportServes`: every tool is
* served by the act transport and is not a read, every argument is one its
* schema takes, every required one is present, and every scope is the
* engine's. A renamed argument in Go is a red build here rather than a button
* that is refused `invalid` the first time somebody presses it.
*/
var ACTIONS = {
	create_work_item: {
		args: [
			"title",
			"body",
			"project",
			"assignee",
			"ask",
			"parent",
			"type",
			"status",
			"priority",
			"due",
			"labels"
		],
		domain: "tracker",
		refreshes: ["work_search"],
		scope: "person"
	},
	answer_knowledge: {
		args: ["q"],
		domain: null,
		refreshes: [],
		scope: "person"
	},
	update_work_item: {
		args: [
			"item",
			"assignee",
			"reason",
			"status",
			"priority",
			"if_match",
			"linked",
			"linked_pages",
			"title",
			"body",
			"labels",
			"due",
			"start",
			"estimate_minutes",
			"points",
			"fields",
			"checklist",
			"watch"
		],
		domain: "tracker",
		refreshes: ["work_search"],
		scope: "person"
	},
	restore_work_item: {
		args: ["item"],
		domain: "tracker",
		refreshes: ["work_search"],
		scope: "person"
	},
	write_project: {
		args: ["project", "target_date"],
		domain: "tracker",
		refreshes: [],
		scope: "company"
	},
	set_pins: {
		args: ["views", "favorites"],
		domain: "tracker",
		refreshes: [],
		scope: "person"
	},
	set_priorities: {
		args: [
			"handle",
			"items",
			"if_match"
		],
		domain: "tracker",
		refreshes: [],
		scope: "person"
	},
	comment_on_work_item: {
		args: [
			"item",
			"answers",
			"choice",
			"body",
			"reply_to",
			"ask",
			"decision"
		],
		domain: "tracker",
		refreshes: [],
		scope: "person"
	},
	answer_run: {
		args: ["turn_id", "answer"],
		domain: null,
		refreshes: ["sandbox_runs", "decisions"],
		scope: "person"
	},
	pause_seat: {
		args: [
			"handle",
			"reason",
			"stop_running"
		],
		domain: null,
		refreshes: [],
		scope: "person"
	},
	resume_seat: {
		args: ["handle"],
		domain: null,
		refreshes: [],
		scope: "person"
	},
	steer_turn: {
		args: ["turn_id", "note"],
		domain: null,
		refreshes: ["turn"],
		scope: "person"
	},
	write_page: {
		args: [
			"title",
			"body",
			"container",
			"parent"
		],
		domain: "pages",
		refreshes: ["knowledge"],
		scope: "person"
	},
	save_page: {
		args: [
			"page",
			"base_version",
			"body",
			"message"
		],
		domain: "pages",
		refreshes: ["knowledge"],
		scope: "person"
	},
	comment_on_page: {
		args: [
			"page",
			"body",
			"reply_to"
		],
		domain: "pages",
		refreshes: [],
		scope: "person"
	},
	place_work_item: {
		args: [
			"item",
			"before",
			"after",
			"status",
			"if_match"
		],
		domain: "tracker",
		refreshes: ["work_search"],
		scope: "person"
	},
	save_work_view: {
		args: [
			"container",
			"name",
			"type",
			"params",
			"owner"
		],
		domain: "tracker",
		refreshes: [],
		scope: "person"
	},
	mark_inbox: {
		args: [
			"read",
			"unread",
			"snooze",
			"unsnooze",
			"read_through"
		],
		domain: "tracker",
		refreshes: [],
		scope: "person"
	}
};
//#endregion
//#region src/contract/errors.ts
/**
* Every `error` a write through `POST /operator/act/{tool}` can come back
* with, and the sentence the dashboard shows for it.
*
* EXACTLY THE ENGINE'S TWO SETS, held both ways by `internal/api/operator`'s
* `TestTheDashboardKnowsExactlyTheActRefusals`: the transport's own codes
* (`operator.ActTransportCodes` — a token, a body, a drain) and the tool
* refusal classes (`mcp.Refusals` — what the tool itself refused). One union
* because a caller branching on `error` must never need to know which half a
* code came from, and the engine keeps them disjoint for that reason. A code
* missing here is a refusal rendered as "something went wrong"; one listed
* that nothing sends is a sentence nobody will ever read.
*
* THE SENTENCE IS SECOND PERSON, and it is ours — except where it is `null`,
* which means "show the tool's own `detail`". Those are the two classes whose
* sentence names the argument that was wrong (`invalid`) or the rule that
* forbade it (`forbidden`); every other class's sentence is written for a
* model reading a tool result, and a person reading it would be told about
* `if_match` and record ids.
*/
var ACT_ERRORS = {
	invalid_token: "The engine refused your API token. Set it again to make changes.",
	unbound: "This token is not bound to a person, so there is nobody to record the change under.",
	unknown_tool: "This engine does not make that change. It may be running a different version from this page — reload.",
	read_only_tool: "The dashboard sent a read as a change. Reload; if it persists, it is a bug.",
	unsupported_media_type: "The dashboard sent the change in a form the engine does not take.",
	invalid_request_id: "The dashboard sent the change without a usable request id.",
	invalid_body: "The dashboard sent a change the engine could not read.",
	body_too_large: "The change is larger than the engine accepts in one request.",
	unreadable_body: "The change could not be read in full by the engine.",
	draining: "This node is shutting down and takes no changes now. Try again in a moment.",
	internal_error: "The change failed inside the engine. Its log has the reason; nothing was refused on purpose.",
	invalid: null,
	not_found: "It is not there any more. Somebody may have removed or moved it.",
	forbidden: null,
	stale_version: "Changed by somebody else since you opened it. Look again, then retry.",
	conflict: "It collided with another change landing at the same moment. Look again, then retry.",
	exists: "That already exists.",
	already_answered: "Somebody has already answered it.",
	reassignment_budget: "It has been handed between seats too many times. A person has to pick it up now.",
	inbox_full: "Your inbox holds too many marks. Mark everything read up to here first.",
	not_running: "It is not running any more.",
	steer_unsupported: "That turn cannot take a note: it runs in a coding agent's own loop.",
	budget_exhausted: "The token budget for this window is spent. Raise it, or wait for the window to reset.",
	unavailable: "This node could not make the change just now. Try again in a moment.",
	peer_upgrading: "The node that would make this change is mid-upgrade. Try again in a moment."
};
//#endregion
//#region src/contract/domains.ts
/**
* Which questions a write can be read back through.
*
* A write through `/operator/act` answers with the POSITION its record landed
* at in its domain's log, and a read that names that position as its floor
* (`read_level=session&min_position=…`) waits until this node has applied it —
* so the screen that pressed the button never shows the state from before the
* press. That is only true of a question that READS a floor: one that ignores
* the key answers from whatever this node holds, and a refetch of it after a
* write is the optimistic guess this dashboard refuses to make.
*
* So the list is the ENGINE'S, per domain, and held both ways by
* `internal/api/queries`' `TestEverySessionQueryTakesAFreshnessFloor`: every
* kind here is asked there with a bare `read_level=session` (refused — no
* floor named) and with a floor (served at `session`), and every question the
* engine serves at a floor is listed here under the domain whose log it
* reads. A kind missing from here is a screen that stays stale after its own
* write; a kind listed that takes no floor is a screen that believes it waited
* and did not.
*/
var SESSION_QUERIES = {
	tracker: [
		"work_items",
		"work_item",
		"work_comments",
		"work_item_turns",
		"work_views",
		"work_saved_views",
		"work_catalogue",
		"work_person",
		"work_projects",
		"work_project",
		"work_workload",
		"work_activity",
		"work_my_work",
		"work_inbox",
		"work_routing",
		"work_flow",
		"company_feed",
		"decisions"
	],
	pages: [
		"pages",
		"page",
		"containers",
		"page_activity",
		"page_revision"
	]
};
//#endregion
//#region src/protocol/session.ts
/**
* This tab's read floors: the newest position each domain has been written at
* from here, which every later read of that domain waits for.
*
* WRITES ARE CONFIRMED, NEVER OPTIMISTIC. A change made through
* `protocol/act.ts` answers with the position its record landed at in its
* domain's log, and the node serving the next read may not have applied it
* yet — a board redrawn from that node shows the card where it was before the
* drag, which reads as "the change did not take". The engine's answer to that
* is a SESSION read: `read_level=session&min_position=<p>` waits until the
* node holds `p`, then answers. So a write raises this tab's floor for its
* domain, and every read of a question that takes a floor
* (`contract/domains.ts`) names it from then on — the refetch the write fires
* included. Nothing is drawn ahead of the engine, and nothing drawn after the
* write is older than it.
*
* PER TAB AND MONOTONIC. A floor only ever rises: a slow answer to an earlier
* write arriving after a later one must not lower what the tab has already
* seen. And it is this tab's alone — another tab's writes are its own
* business, reached by its own polls.
*
* `unknown` RAISES NOTHING. An outcome nobody can vouch for has no position
* worth waiting on, and a floor at a position that never lands would make
* every read of the domain wait for it.
*/
/**
* A position off the wire, or null for one this client cannot read.
*
* THE ENGINE'S OWN GRAMMAR (`tracker.ParseLogPosition`), so the value handed
* back as `min_position` is one the engine will accept: a stream name, then a
* generation and a sequence that are both non-negative integers.
*/
function parsePosition(raw) {
	const match = /^([^@\s]+)@(\d+):(\d+)$/.exec(raw ?? "");
	if (!match) return null;
	const generation = Number(match[2]);
	const seq = Number(match[3]);
	if (!Number.isSafeInteger(generation) || !Number.isSafeInteger(seq)) return null;
	return {
		stream: match[1],
		generation,
		seq
	};
}
/**
* Whether `next` is later than `held` on one log.
*
* GENERATION FIRST, as the engine orders them (`statelog.Later`): a log
* restored or re-anchored starts a new generation whose sequence may restart
* below the old one, and a comparison by sequence alone would keep the old
* floor for ever. A position on ANOTHER stream is taken as later — the only
* way the stream behind a domain changes is the engine replacing its log, and
* a floor on a log that no longer exists is one no read could ever meet.
*/
function isLater(next, held) {
	if (next.stream !== held.stream) return true;
	if (next.generation !== held.generation) return next.generation > held.generation;
	return next.seq > held.seq;
}
/**
* One tab's floors. A class rather than module state so a suite can hold its
* own; the dashboard uses [session], the one per tab.
*/
var SessionFloors = class {
	floors = /* @__PURE__ */ new Map();
	listeners = /* @__PURE__ */ new Set();
	/** The floor a read of `domain` names, or null before this tab wrote to it. */
	floor(domain) {
		return this.floors.get(domain)?.raw ?? null;
	}
	/**
	* Record a write this tab made to `domain` (null for one that lands in no
	* log a question reads), landed at `position`, and tell
	* every listener the domain moved — whether or not the floor rose, because
	* a write that appended nothing (`position` null) still answered, and the
	* questions it named are worth asking again.
	*
	* Returns whether the floor rose.
	*/
	written(domain, position, refreshes) {
		let rose = false;
		const at = parsePosition(position);
		if (domain !== null && at && position) {
			const held = this.floors.get(domain);
			if (!held || isLater(at, held.at)) {
				this.floors.set(domain, {
					raw: position,
					at
				});
				rose = true;
			}
		}
		for (const listener of this.listeners) listener(domain, refreshes);
		return rose;
	}
	/** Subscribe to writes. Returns the unsubscribe. */
	onWritten(listener) {
		this.listeners.add(listener);
		return () => this.listeners.delete(listener);
	}
	/**
	* Whether a write heard by a listener moves the answer to `kind`: a question
	* that reads the written domain at a floor, or one the write names as
	* moving beside it.
	*/
	static moves(kind, domain, refreshes) {
		return domain !== null && domainOf(kind) === domain || refreshes.includes(kind);
	}
	/**
	* The freshness a read of `kind` names, or null when it names none: the
	* question takes no floor, or this tab has not written its domain.
	*/
	freshness(kind) {
		const domain = domainOf(kind);
		if (domain === null) return null;
		const floor = this.floor(domain);
		return floor === null ? null : {
			read_level: "session",
			min_position: floor
		};
	}
};
/** The domain whose log a question reads at a floor, or null for one that takes none. */
function domainOf(kind) {
	for (const domain of Object.keys(SESSION_QUERIES)) if (SESSION_QUERIES[domain].includes(kind)) return domain;
	return null;
}
/** This tab's floors. */
var session = new SessionFloors();
//#endregion
//#region src/protocol/act.ts
/**
* The dashboard's one write: a change, made as the person the token is bound
* to (ADR-0024).
*
* EVERY BUTTON THAT CHANGES THE COMPANY COMES THROUGH HERE, and here goes to
* exactly one route — `POST /operator/act/{tool}` — where the engine runs the
* same operator tool a person's own assistant would, attributed to the token
* as author and to the seat it is bound to as the person. There is no second
* write path to keep in step with it: `app/source.test.ts` holds every `act(`
* to a literal tool, `protocol/transport.test.ts` holds every network read to
* `src/protocol/`, and `contract/actions.ts` is the whole vocabulary, held
* against the engine's catalogue by a Go gate.
*
* # What an answer can be, and what each tells a person
*
*  - `applied` — the record landed at `position`, and this tab's read floor
*    for its domain rose to it (`protocol/session.ts`), so every read after
*    this one includes it;
*  - `pending` — the engine accepted it and this node has not applied it
*    yet: the floor rises, and the reads wait for it;
*  - `unknown` — nobody can say whether it landed: the connection dropped
*    after the request left, a gateway gave up, or the engine itself said so.
*    NEVER RETRIED HERE. A retry is the person's decision, and when they make
*    it the same `requestId` goes again, which the engine derives the same
*    operations from — so a retry is the first attempt's write rather than a
*    second one;
*  - `refused` — the engine said no, with a code from `contract/errors.ts`
*    and the sentence a person is shown for it.
*
* AND IT NEVER THROWS for an answer, a refusal or a lost connection — those
* are all values a screen renders. It rejects only for the caller's own
* abort, which is not an answer at all.
*/
var ROWS = ACTIONS;
/**
* The codes the act transport itself answers with a 5xx, each of which says
* nothing was written: the drain gate refused before the handler ran, a tool
* refused before it appended (an interrupted call says `outcome: unknown`
* beside its class, and is read before this), or a tool failed without a
* class. Any other 5xx is somebody else's.
*/
var ENGINE_5XX = /* @__PURE__ */ new Set([
	"draining",
	"unavailable",
	"peer_upgrading",
	"internal_error"
]);
/** The refusal classes a later attempt may clear: the node, not the request. */
var RETRYABLE = /* @__PURE__ */ new Set([
	"draining",
	"unavailable",
	"peer_upgrading"
]);
/** Whether a code off the wire is one this build knows. */
function isActErrorCode(code) {
	return Object.hasOwn(ACT_ERRORS, code);
}
/**
* A fresh request id: a UUIDv7, stamped with the instant the gesture began.
*
* VERSION 7 BECAUSE THE ENGINE READS THE INSTANT. The act transport derives
* the call's operation from this id, and the operation's mint instant is the
* id's own — the instant the engine uses to decide whether its operation
* ledger can still vouch for a retry. An id carrying no instant is refused
* `invalid_request_id`. The layout is the engine's operation-id grammar with
* no name ([layoutOpID]), so there is one encoder rather than two.
*
* `crypto.getRandomValues`, never `crypto.randomUUID`: the second exists only
* in a secure context, and a node's dashboard reached at
* `http://10.0.0.4:8000` is not one — every write there would throw.
*/
function newRequestId(now = Date.now()) {
	return layoutOpID(now, crypto.getRandomValues(/* @__PURE__ */ new Uint8Array(10)), "");
}
/**
* Make one change, as the signed-in person.
*
* The body is `{request_id, args}` as JSON — the engine refuses any other
* content type, which is what keeps a cross-site form from reaching a write.
*/
async function act(tool, args, options = {}) {
	const requestId = options.requestId ?? newRequestId();
	const floors = options.floors ?? session;
	const unknown = (reason) => ({
		kind: "unknown",
		tool,
		requestId,
		reason
	});
	let body;
	try {
		({body} = await rest.request("POST", `/operator/act/${encodeURIComponent(tool)}`, {
			body: {
				request_id: requestId,
				args
			},
			contentType: "application/json",
			signal: options.signal
		}));
	} catch (err) {
		if (isAbort(err)) throw err;
		if (!(err instanceof RestError)) return unknown(String(err));
		return refusalOf(tool, requestId, err, floors);
	}
	const answer = body ?? {};
	const outcome = answer.outcome;
	if (outcome !== "applied" && outcome !== "pending") return unknown("The engine accepted the change and could not confirm it landed.");
	const position = typeof answer.position === "string" ? answer.position : null;
	const { domain, refreshes } = ROWS[tool];
	floors.written(domain, position, refreshes);
	return {
		kind: outcome,
		tool,
		requestId,
		position,
		domain,
		receipt: answer.receipt
	};
}
/**
* The refusals that say the screen the press was made from is OUT OF DATE:
* somebody else changed the object since it was drawn (`stale_version`,
* `conflict`), removed it (`not_found`), made it already (`exists`) or answered
* it (`already_answered`). Each asks again every question the write would have
* moved — without raising a floor, since nothing of this tab's landed — so the
* page redraws what IS there and the next press is made against it. Without
* that a task page kept sending the version it was drawn at and was refused
* `stale_version` on every press until its own poll happened to come round.
*/
var LOOK_AGAIN = /* @__PURE__ */ new Set([
	"stale_version",
	"conflict",
	"not_found",
	"exists",
	"already_answered"
]);
/** What a refusal, or a request that never got an answer, came to. */
function refusalOf(tool, requestId, err, floors) {
	if (err.body.outcome === "unknown") return {
		kind: "unknown",
		tool,
		requestId,
		reason: err.detail || err.code
	};
	const code = isActErrorCode(err.code) ? err.code : "";
	if (err.status === 0 || err.status >= 500 && !ENGINE_5XX.has(code)) return {
		kind: "unknown",
		tool,
		requestId,
		reason: err.detail || `The answer was lost (status ${err.status}).`
	};
	if (err.status === 401) requestToken();
	if (LOOK_AGAIN.has(code)) {
		const { domain, refreshes } = ROWS[tool];
		floors.written(domain, null, refreshes);
	}
	return {
		kind: "refused",
		tool,
		requestId,
		code,
		sentence: (code === "" ? null : ACT_ERRORS[code]) ?? (err.detail || `The engine refused the change (status ${err.status}).`),
		hint: err.hint,
		retryable: RETRYABLE.has(code)
	};
}
//#endregion
export { BROKER_FINDING_KINDS, BROKER_KINDS, BROKER_REMOVE_TIMEOUT_MS, ESTATE_MAP_STATES, HOLDER_STATES, LiveSocket, PARTITION_STATES, QueryError, REQUEST_TIMEOUT_MS, RestError, SessionFloors, Store, act, api, apiToken, clearToken, domainOf, isAbort, keepsOperation, layoutOpID, newGateOpID, onTokenChanged, onTokenRequested, queryErrorCode, requestToken, rest, retryAfterSeconds, storeToken };
