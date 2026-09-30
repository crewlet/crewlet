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
		authRejected: false,
		identityUnverifiable: false,
		accessRefused: null,
		inboxMoves: {}
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
	/**
	* Changed seat overlays, merged onto the roster rows by AGENT ID.
	*
	* By the id and nothing else. They were merged by ROLE NAME, which is prose:
	* two seats sharing a name both took every overlay either of them moved, so
	* each card rendered whatever the other was last doing. The handle is no
	* better a key — a rename moves it while the overlays already in flight were
	* cut before it — and the id, derived from the handle a seat was created
	* under, is the one value a rename leaves where it was.
	*
	* An overlay for a seat the roster does not carry is DROPPED rather than
	* appended as a row of its own. A seat reaches this list through the roster
	* (the snapshot, or a `seats` push after every published company), and the
	* server merges its live overlay into that roster row before sending it —
	* so nothing dropped here is lost, while an appended row would be a card
	* with no name, no handle and no page, for a seat the server may already
	* have removed.
	*/
	applyAgents(rows) {
		if (!Array.isArray(rows) || rows.length === 0) return;
		const byID = /* @__PURE__ */ new Map();
		for (const row of rows) if (row && typeof row.agent_id === "string" && row.agent_id !== "") byID.set(row.agent_id, row);
		let moved = false;
		this.state.agents = this.state.agents.map((a) => {
			const patch = byID.get(a.agent_id);
			if (!patch) return a;
			moved = true;
			return {
				...a,
				...patch
			};
		});
		if (moved) this.emit("agents");
	}
	/**
	* The complete seat list, replacing what is on screen.
	*
	* Distinct from `applyAgents`, which merges changed overlays: a merge cannot
	* express a deletion, so a revision that removes a seat would leave its card
	* rendered until the next reload.
	*/
	applySeats(rows) {
		if (!Array.isArray(rows)) return;
		const live = new Map(this.state.agents.map((a) => [a.agent_id, a]));
		this.state.agents = rows.map((row) => {
			const current = live.get(row.agent_id);
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
		if (!value) this.state.identityUnverifiable = false;
		if (!value) this.state.health = { status: "unknown" };
		this.emit("health");
	}
	/**
	* One seat's inbox moved. The SEAT is read off the payload's own `handle`
	* rather than trusted from anywhere else, and a frame without one moves
	* nothing — a counter under "" would be a seat no screen asks about.
	*/
	applyInboxChanged(change) {
		const handle = change?.handle;
		if (typeof handle !== "string" || handle === "") return;
		this.state.inboxMoves = {
			...this.state.inboxMoves,
			[handle]: (this.state.inboxMoves[handle] ?? 0) + 1
		};
		this.emit("inboxMoves");
	}
	applyIdentity(state) {
		const next = state?.state === "unverifiable";
		if (this.state.identityUnverifiable === next) return;
		this.state.identityUnverifiable = next;
		this.emit("health");
	}
	setAccessRefused(reason) {
		const next = reason === null ? null : reason || "refused";
		if (this.state.accessRefused === next) return;
		this.state.accessRefused = next;
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
};
//#endregion
//#region src/protocol/retry.ts
/**
* When to ask the engine again after it said it cannot answer yet.
*
* THE ENGINE SAYS WHEN, and every retry path in this dashboard reads that one
* way, here. A socket query's or a watch's `unavailable` frame carries
* `retry_after`, and a REST `503` the engine wrote carries a `Retry-After`
* header; both are whole seconds decided by the state log's own rule — the
* refusal's derived hint where it has one, the node's health tick where it has
* none — and ZERO where waiting will not change the answer: a log at its byte
* ceiling, a record this node cannot decode, a barrier its broker refused. The
* query layer used to ignore all of it and re-ask at a fixed five seconds, so a
* node draining a long backlog was asked a dozen times before it could have
* answered once, and a refusal only an operator could lift was asked for as
* long as the tab stayed open.
*
* PURE ARITHMETIC OVER A NUMBER, imported by the org builder's core as well as
* by the socket, so it imports nothing: the builder's model takes nothing from
* this directory at runtime but this file, because the rest of it is the socket
* and `fetch` (see `routes/org/builder/model/boundary.test.ts`).
*/
/**
* How soon an `unavailable` answer that carries NO hint is asked again, in
* milliseconds.
*
* ONLY THE FALLBACK. Every `unavailable` frame the engine sends carries
* `retry_after`, so an answer without one is one nobody decided a hint for —
* during a rolling upgrade the node behind this page's address may be on a
* build older than the field. FIVE SECONDS because that is what the engine
* itself says when it has nothing better: its shared health tick
* (`stream.HealthInterval`), the soonest a node's posture can change and the
* hint every `unavailable` it cannot say more about carries. Waiting it out
* is asking as the engine would have asked; sooner asks before anything could
* have changed, and later leaves a recovered node looking broken.
* `internal/api`'s `TestTheDashboardRetriesOnTheEnginesOwnHints` holds it to
* the engine's value.
*/
var UNAVAILABLE_RETRY_MS = 5e3;
/**
* The longest a hint is waited out before asking again anyway, in
* milliseconds.
*
* A BOUND ON THE ONE HINT THAT HAS NONE. Every hint the engine fixes — the
* health tick (5 s), a quorum election (4 s), the identity estate's two
* seconds, the reconcile poll that brings a company (15 s), a drain (30 s) —
* is at or under thirty seconds and is waited out exactly. The one this cuts is
* DERIVED: a node's backlog divided by the rate it has been draining at, which
* is minutes on a node that has just joined or restarted behind a busy log,
* and an estimate made from a rate that only rises as the node warms up. Past
* thirty seconds a screen waiting on it says "this fills in on its own" for
* longer than a person believes it, and the cost of asking sooner is one read
* the node refuses again. THIRTY because it is already this dashboard's
* ceiling on waiting for an engine to come back — the socket's reconnect
* backoff and the builder's check backoff stop there for the same reason — and
* `internal/api`'s `TestTheDashboardRetriesOnTheEnginesOwnHints` fails the day
* a hint the engine fixes grows past it.
*/
var RETRY_AFTER_MAX_MS = 3e4;
/**
* The wait a hint of `seconds` asks for, in milliseconds, or `null` for "do not
* ask again on a timer".
*
* ZERO IS THE ANSWER, never an omission: the engine is saying waiting will not
* change it, so nothing re-asks — the screen shows the refusal and what it
* names, and it is asked again only when something a person does could have
* changed it (a reconnect, a write, a reload). A caller whose answer carried no
* hint at all does not come here: what an absent hint means is the caller's —
* the socket's is {@link UNAVAILABLE_RETRY_MS}, a request that never reached
* the engine backs off.
*
* The hint is whole seconds and never negative — both parsers that read one
* admit nothing else — so anything at or under zero is the zero.
*/
function retryAfterMs(seconds) {
	if (!(seconds > 0)) return null;
	return Math.min(seconds * 1e3, RETRY_AFTER_MAX_MS);
}
//#endregion
//#region src/protocol/session.ts
var need = null;
var listeners = /* @__PURE__ */ new Set();
function announce() {
	for (const listener of listeners) listener();
}
/** The session's outstanding need, or null for one that needs nothing. */
function currentSessionNeed() {
	return need;
}
/**
* Record what the session lacks. Called by the transports, never by a screen:
* a screen that wants a person to sign in navigates there itself.
*/
function needSession(what) {
	if (need === what) return;
	need = what;
	announce();
}
/**
* The browser holds a whole session again — a sign-in, a redemption or an
* enrolment finished — so whatever a transport recorded before it no longer
* describes this browser.
*/
function sessionRestored() {
	if (need === null) return;
	need = null;
	announce();
}
/**
* The browser holds a session that may only enrol a second factor — a
* sign-in or a redemption answered `second_factor_enrolment_required`.
*
* THE SIGN-IN'S OWN ANSWER REPLACES WHATEVER A TRANSPORT RECORDED BEFORE IT,
* because it is the newest fact about this browser's session. The sign-in
* screen's own `GET /auth/session` and the socket's refusal probe both record
* `sign_in` for a browser holding nothing, which is the state every sign-in
* starts from; left in place, it routed the enrolment straight back to the
* sign-in form the moment the enrolment screen mounted.
*/
function sessionNeedsEnrolment() {
	needSession("second_factor");
}
/**
* Subscribe to a change of need. Returns the unsubscribe. The shape
* `useSyncExternalStore` takes, which is what reads it.
*/
function onSessionNeed(listener) {
	listeners.add(listener);
	return () => {
		listeners.delete(listener);
	};
}
var confirmer = null;
var confirming = null;
/**
* Install what confirms a step-up. Returns the uninstall, which leaves a
* later installation in place.
*
* ONE AT A TIME, because there is one person at the keyboard: a second
* installation replaces the first rather than queueing behind it.
*/
function setStepUpConfirmer(fn) {
	confirmer = fn;
	return () => {
		if (confirmer === fn) confirmer = null;
	};
}
/**
* Ask for a step-up, or join the one already being asked.
*
* FALSE WITH NOBODY TO ASK, so a refused request is reported as the refusal it
* was rather than waiting for a dialog that will never open; and false for a
* confirmer that failed, which is not a proof.
*
* ANY CONFIRMATION COVERS EVERY WINDOW: the password the dialog asks for
* proves inside both, so a request refused for the sensitive window joins one
* opened for the ordinary one rather than asking twice.
*/
function confirmStepUp(window) {
	if (!confirmer) return Promise.resolve(false);
	if (!confirming) confirming = confirmer(window).catch(() => false).finally(() => {
		confirming = null;
	});
	return confirming;
}
//#endregion
//#region src/protocol/rest.ts
/**
* The dashboard's one REST transport: every write, and the guarded reads the
* socket has no question for.
*
* The socket remains the data channel for state. This is not a second one: it
* carries the requests that are not questions about state at all. Writes never
* go over the socket, deliberately: a write is judged on its `Origin` by the
* engine's cross-site check before its handler runs, answers the three write
* outcomes as statuses with an op id to retry by, and may be refused for a
* step-up this module confirms and replays — none of which a frame on an open
* socket carries. And a handful of reads exist only as REST, `GET /secrets`
* above all, because no query in the registry answers them.
*
* ONE MODULE, for the reason `api.ts` states about itself: a screen reaching
* for its own transport takes its client from somewhere, and the somewhere the
* Fleet screen chose was a context field the shell never populated, so it
* shipped dead. Everything here is a plain function over `fetch` against
* `location.origin`, which is where the dashboard is served from and the only
* origin the engine answers on (it writes no CORS header at all).
*
* THE CREDENTIAL IS THE SESSION COOKIE, which the browser attaches to every
* same-origin request and this module never sees: it is `HttpOnly`, so no
* script on the page can read it, and there is no token in storage for one to
* take instead. A call that must present something else — the API token
* exchange at `POST /auth/token` — sets its own `Authorization` header for
* that one request, and nothing keeps it.
*/
/**
* What the engine said when it refused.
*
* `status` alone is not enough to act on: the setup surface distinguishes a
* revision that moved under the caller from a config slot holding a literal
* from a company with no active revision, and all three are a 409. The engine
* answers those with a `code`, and the screen branches on it.
*/
var RestError = class extends Error {
	status;
	code;
	detail;
	hint;
	/**
	* The engine's own sentence for the code — the envelope's `message`, which
	* every refusal it writes carries — or "" for an answer the engine did not
	* write. What a person is shown when a screen has nothing more specific to
	* say: the sign-in surface's one uniform refusal is exactly this sentence,
	* and a screen that wrote its own would be a second copy of the engine's
	* wording, the one that goes stale.
	*/
	sentence;
	/**
	* The `Retry-After` the answer carried, in whole seconds, or null for none.
	* A `429` always carries one and says how long the curve makes the next
	* attempt wait; a `503` carries one where waiting can clear the cause and
	* none where it cannot, which is a difference a screen has to render.
	*/
	retryAfter;
	/** Everything else the body carried, for a caller that needs a field. */
	body;
	constructor(status, body, retryAfter = null) {
		const code = typeof body.error === "string" ? body.error : "";
		const detail = typeof body.detail === "string" ? body.detail : "";
		super(detail || code || `HTTP ${status}`);
		this.name = "RestError";
		this.status = status;
		this.code = code;
		this.detail = detail;
		this.hint = typeof body.hint === "string" ? body.hint : "";
		this.sentence = typeof body.message === "string" ? body.message : "";
		this.retryAfter = retryAfter;
		this.body = body;
	}
	/**
	* Whether the engine refused on AUTHORITY rather than on the request.
	*
	* Both statuses, because a screen locks the same way for either and the
	* distinction is not one it can act on: 401 is "present a credential" and
	* 403 is "the one you presented does not carry this grant". What neither
	* is, any more, is a reason to send the reader to sign in again — a reader
	* holding a perfectly good session meets 403 the moment they open a screen
	* outside their grants, which is the ordinary case rather than the
	* exceptional one. Only a 401 says nobody is signed in (see [noteSession]).
	*/
	get unauthorized() {
		return this.status === 401 || this.status === 403;
	}
	/** The grants a refusal on authority named — see [refusedGrants]. */
	get grants() {
		return refusedGrants(this.body);
	}
	/**
	* This refusal in the shape `QueryState` renders from — the one the
	* socket's error frame carries, under the same keys: a 403's rule and the
	* grants that would have admitted the caller, and a 503's state-log code,
	* its own words and whether waiting changes it (no `Retry-After` is the
	* engine saying it will not). Null for anything else, a 401 included:
	* nobody being signed in is not a rule's refusal, and there are no grants
	* to name to nobody.
	*/
	get refusal() {
		if (this.status === 403) return {
			reason: typeof this.body.reason === "string" ? this.body.reason : this.code,
			grants: this.grants
		};
		const hint = this.retryHint;
		if (hint !== null) return {
			code: typeof this.body.refusal === "string" ? this.body.refusal : null,
			detail: this.detail || null,
			retryAfter: hint
		};
		return null;
	}
	/**
	* When the engine said to ask again, in whole seconds, or null where it said
	* nothing: a `503` it wrote says its `Retry-After`, and ZERO where it sent
	* none — its statement that waiting will not change the answer. Every other
	* answer, a `503` something in front of the engine wrote included, carries
	* no hint, because nobody at the engine decided one.
	*
	* What a retry loop waits out (`retryAfterMs` in `retry.ts`), and what
	* [refusal] hands `QueryState` for a `503`: the rule for which `503` is the
	* engine's lives here and nowhere else.
	*/
	get retryHint() {
		return this.status === 503 && !this.unanswered ? this.retryAfter ?? 0 : null;
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
* The grants a refusal on AUTHORITY named: any ONE of which would have
* admitted the caller. Empty when the answer named none — a 401 (nobody was
* recognised), or a rule that asks a relation no grant replaces.
*
* READ FROM THE ANSWER, because the engine's envelope carries them under
* `grants` precisely so no screen has to name a grant itself: a sentence
* typed into a screen is a second statement of the rule, and the one that
* goes stale the day the rule's grant moves. Takes the raw body rather than
* only a [RestError] because the org builder reads `GET /config` through its
* own answer type.
*/
function refusedGrants(body) {
	if (typeof body !== "object" || body === null) return [];
	const grants = body.grants;
	return Array.isArray(grants) ? grants.filter((g) => typeof g === "string") : [];
}
/**
* The codes a `401` carries when it is an answer about WHAT WAS TYPED rather
* than about the browser's credential: a sign-in whose details were not
* accepted, and one whose password proved itself and now wants the second
* factor. Every other `401` means this browser holds nothing the engine
* accepts, and the session needs a sign-in.
*/
var TYPED_REFUSALS = /* @__PURE__ */ new Set(["sign_in_refused", "second_factor_required"]);
/**
* What a refusal says about the browser's SESSION, noted where every screen's
* request passes — so no screen has to recognise a lost session itself, and
* none can forget to.
*
* A `401` that is not an answer about typed details is a browser signed in as
* nobody the engine accepts: never signed in, its session ended, expired or
* revoked. A `403 second_factor_enrolment_required` is a session that may do
* nothing but enrol the second factor the deployment requires. Every other
* refusal is about the REQUEST, and the session is fine.
*/
function noteSession(refusal) {
	if (refusal.status === 401 && !TYPED_REFUSALS.has(refusal.code)) needSession("sign_in");
	if (refusal.status === 403 && refusal.code === "second_factor_enrolment_required") needSession("second_factor");
}
/**
* When a REST read that failed with `err` is asked again, in milliseconds, or
* `null` for "not on a timer" — the REST twin of the socket's
* `unavailableRetryMs`, for a screen that reads over REST and asks again on
* its own.
*
* A `503` the engine wrote says when ([RestError.retryHint]), read through
* `retryAfterMs`: waited out exactly, bounded, and its ZERO — a `503` with no
* `Retry-After` — never on a timer, because the engine is saying waiting will
* not change the answer. Every other failure carries no hint, since nobody at
* the engine decided one, and waits `otherwise`: the screen's own cadence, or
* `null` where it has none.
*/
function restRetryMs(err, otherwise) {
	const hint = err instanceof RestError ? err.retryHint : null;
	return hint === null ? otherwise : retryAfterMs(hint);
}
/**
* The seconds a `Retry-After` header names, or null for none. The engine
* writes whole seconds and never an HTTP date; anything else is not its
* answer and is read as none.
*
* Exported for the one read that does not go through [request] — the
* degraded-mode snapshot (`api.ts`) — so both read the header one way.
*/
function retryAfterOf(response) {
	const raw = response.headers.get("Retry-After")?.trim() ?? "";
	return /^\d+$/.test(raw) ? Number(raw) : null;
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
* The route a step-up is given at. It answers `step_up_required` itself when
* the caller is a credential nobody present can confirm — and asking to
* confirm the confirmation would be a dialog that reopens for ever.
*/
var STEP_UP_PATH = "/auth/step-up";
/** Which window a step-up refusal names, from the envelope's own key. */
function windowOf(refusal) {
	const window = refusal.body.window;
	return typeof window === "string" && window !== "" ? window : "step_up";
}
/**
* `waiting`, or the caller's own abort if that comes first. A person can sit
* at the confirmation for as long as they like, and a screen that gave up on
* its request meanwhile must not be held to an answer it no longer wants.
*/
function unlessAborted(waiting, signal) {
	if (!signal) return waiting;
	if (signal.aborted) return Promise.reject(signal.reason);
	return new Promise((resolve, reject) => {
		const abort = () => reject(signal.reason);
		signal.addEventListener("abort", abort, { once: true });
		waiting.then((value) => {
			signal.removeEventListener("abort", abort);
			resolve(value);
		}, (err) => {
			signal.removeEventListener("abort", abort);
			reject(err);
		});
	});
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
*
* # A step-up is confirmed HERE, and the refused request sent again
*
* A gesture refused `403 step_up_required` is asked of the person once —
* through whatever confirms a step-up (`session.ts`), however many requests
* were refused together — and then REPLAYED: the same method, path, body and
* headers, so a form that was being saved is saved, rather than lost to a
* refusal its screen could only report. Once: a replay refused again is the
* refusal. Every screen gets this by sending its writes through here, and no
* screen implements it, which is what makes it one ceremony rather than a
* dozen that disagree.
*
* The deadline is each ATTEMPT's, not the gesture's: the time a person spends
* typing their password is not the engine taking too long.
*/
async function request(method, path, options = {}) {
	try {
		return await attempt(method, path, options);
	} catch (err) {
		if (!(err instanceof RestError && err.status === 403 && err.code === "step_up_required") || path.split("?")[0] === STEP_UP_PATH) throw err;
		if (!await unlessAborted(confirmStepUp(windowOf(err)), options.signal)) throw err;
		return attempt(method, path, options);
	}
}
/** One round trip — see [request] for what surrounds it. */
async function attempt(method, path, options) {
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
	const init = {
		method,
		cache: "no-store",
		credentials: "same-origin",
		headers: {
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
		const body = parsed && typeof parsed === "object" ? parsed : {};
		const refusal = new RestError(response.status, body, retryAfterOf(response));
		noteSession(refusal);
		throw refusal;
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
/** The degraded-mode snapshot, or why not and when to ask again. */
async snapshot() {
	try {
		const response = await fetch(location.origin + "/stream/snapshot", { credentials: "same-origin" });
		if (!response.ok) {
			const body = await response.json().catch(() => null);
			const envelope = body !== null && typeof body === "object" ? body : {};
			return {
				state: "unread",
				retryAfter: new RestError(response.status, envelope, retryAfterOf(response)).retryHint
			};
		}
		return {
			state: "read",
			snapshot: await response.json()
		};
	} catch {
		return {
			state: "unread",
			retryAfter: null
		};
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
* One push is addressed to a SEAT rather than to every tab: `inbox_changed`,
* sent only to a socket that asked to `watch` that seat and was allowed to. It
* is how a person learns they have work without waiting for a poll.
*
* There are no HTTP fetches in normal operation. The REST snapshot is used for
* exactly one thing: keeping the page honest while the socket is down (a proxy
* that refuses to upgrade, a restarting engine), and it stops the moment the
* socket is back.
*/
/**
* A rejected question, carrying — when the engine refused it on AUTHORITY —
* the reason and the grants its error frame named, or — when it answered
* `unavailable` — the state log's refusal behind that and whether asking again
* can change it ({@link LogRefusal}).
*
* `message` IS STILL THE CODE, which every existing reader tests with
* {@link queryErrorCode}; the refusal rides beside it rather than replacing it,
* so a screen that only branches on the code is unchanged and one that can say
* what would admit the reader has it to say.
*/
var QueryRefusedError = class extends Error {
	refusal;
	constructor(code, refusal) {
		super(code);
		this.refusal = refusal;
		this.name = "QueryRefusedError";
	}
};
/**
* A failed `query` as the pair `QueryState` renders from.
*
* ONE READING of a rejection, for every surface that asks outside `useQuery` —
* a page of older rows, the shared health read. Read inline at each, the
* refusal was the half a surface forgot: its screen said a read was refused
* and not which grant would have admitted the reader, although the answer had
* named it.
*/
function queryFailure(err) {
	return {
		error: err instanceof Error ? err.message : "query_failed",
		refusal: err instanceof QueryRefusedError ? err.refusal : null
	};
}
/**
* Whether a refusal is the state log's, carried by an `unavailable` answer,
* rather than one on authority. The two ride the same field of an answer
* because a screen hands both to `QueryState` the same way.
*/
function isLogRefusal(refusal) {
	return "retryAfter" in refusal;
}
var PATH = "/ws/stream";
/**
* The credential this socket was opened with names nobody any more — the
* session ended, expired or was revoked. The engine re-checks an open socket
* every minute and closes it with this when that happens. NOT a refusal on its
* own: the browser may hold a newer cookie than the one this socket was opened
* with, so the ordinary reconnect is the repair, and only a handshake that is
* then refused (see `probeRefusal`) asks the reader for anything.
*/
var CLOSE_UNAUTHENTICATED = 4401;
/**
* The credential still names somebody who may not have this surface: their
* seat is gone from the chart, or the grant the socket needs was withdrawn.
* Reconnecting reaches the same person with the same access, so the socket
* STOPS and the page says why.
*/
var CLOSE_FORBIDDEN = 4403;
/**
* Reconnect backoff ceiling. Long enough that a dashboard left open against a
* stopped engine is not hammering it, short enough that bringing the engine
* back feels immediate.
*/
var MAX_BACKOFF_MS = 3e4;
/** Application-level keepalive, comfortably inside the 60 s idle timeout most reverse proxies apply. */
var PING_MS = 25e3;
/**
* Degraded-mode poll — only ever runs while the socket is down. A `503` the
* engine wrote replaces its next tick with the answer's own `Retry-After`
* (see `fallbackFetch`).
*/
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
* How soon something the engine answered `unavailable` is asked again — a
* query (see `useQuery`), the shared health read, or a watch — in
* milliseconds, or `null` for "not on a timer".
*
* `unavailable` is the engine saying it cannot answer HERE, and its frame says
* when that may change: `retry_after`, read through {@link retryAfterMs} —
* waited out, bounded, and ZERO meaning waiting will not change it, so nothing
* re-asks. An answer carrying no hint waits {@link UNAVAILABLE_RETRY_MS}, what
* the engine says when it has nothing better; so does a refusal that is not
* the state log's, which no `unavailable` answer carries.
*
* ONE READING for all three, because the engine's answer is one: a query, the
* health read and a watch refused `unavailable` by the same node are waiting
* on the same thing. The fixed five seconds every one of them re-asked at
* whatever the frame said is what this replaced.
*/
function unavailableRetryMs(refusal) {
	if (refusal === null || !isLogRefusal(refusal)) return UNAVAILABLE_RETRY_MS;
	return retryAfterMs(refusal.retryAfter);
}
/**
* What a watch refusal's error frame names in `what` — the engine's
* `watchWhat`. A watch carries no query id, so this is how an error frame about
* one is told from a query's.
*/
var WATCH_WHAT = "watch";
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
* The refusal an error frame carries, or null — for a frame that is neither a
* refusal on authority nor an `unavailable` answer, or a node too old to say
* why.
*/
function refusalOf(msg) {
	if (msg.error === "unavailable") {
		if (typeof msg.retry_after !== "number" || !(msg.retry_after >= 0)) return null;
		return {
			code: typeof msg.refusal === "string" ? msg.refusal : null,
			detail: typeof msg.detail === "string" ? msg.detail : null,
			retryAfter: msg.retry_after
		};
	}
	if (msg.error !== "unauthorized" || typeof msg.reason !== "string") return null;
	return {
		reason: msg.reason,
		grants: Array.isArray(msg.grants) ? msg.grants.filter((g) => typeof g === "string") : []
	};
}
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
	/**
	* The degraded-mode poll now running, or 0 for none. A NUMBER PER RUN rather
	* than a flag, because a read can be in flight when its run is stopped, and
	* one that landed after a new run began would schedule a second chain of
	* reads beside the new one's.
	*/
	fallbackRun = 0;
	fallbackRuns = 0;
	isClosed = false;
	/**
	* Whether the engine refused this browser the surface (see `accessRefused`).
	* A latch rather than a cancelled timer, because the refusal can arrive from
	* an async probe while a reconnect is already in flight, whose own close
	* would otherwise schedule the next one.
	*/
	refused = false;
	nextQueryId = 1;
	inflight = /* @__PURE__ */ new Map();
	/**
	* The seat this tab watches for `inbox_changed` frames, or "". Held HERE
	* rather than only sent, because a watch lives in the engine's routing index
	* for ONE socket: every new socket starts watching nothing, so the tab has to
	* say it again on every open.
	*/
	watched = "";
	watchRetry = 0;
	constructor(store) {
		this.store = store;
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
		this.refused = false;
		this.store.setAuthRejected(false);
		if (this.sock) this.sock.close();
		else this.connect();
	}
	stop() {
		this.isClosed = true;
		clearTimeout(this.reconnectTimer);
		clearTimeout(this.watchRetry);
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
	/**
	* Ask for one seat's `inbox_changed` frames on this socket and every socket
	* after it, or stop with "".
	*
	* ONE SEAT, because the engine keeps one per socket: a watch is a screen
	* saying where it now is, so a second call replaces the first rather than
	* adding to it. The engine decides a watch the way it decides the inbox
	* question about the same seat — its holder, whoever leads it, or the admin
	* grant — and answers a refusal on the socket rather than closing it; see
	* `watchAnswered`.
	*/
	watch(seat) {
		const next = seat.trim();
		clearTimeout(this.watchRetry);
		this.watchRetry = 0;
		const clearing = next === "" && this.watched !== "";
		this.watched = next;
		if (next !== "" || clearing) this.sendWatch();
	}
	sendWatch() {
		if (!this.connected || !this.sock) return;
		try {
			this.sock.send(JSON.stringify({
				kind: "watch",
				seat: this.watched
			}));
		} catch {}
	}
	/**
	* The engine refused a watch — the one it was just sent, or the one it
	* re-decided when it re-checked this socket's credential.
	*
	* `unavailable` means this node could not read the chart that decides it,
	* or the directory a login resolves through, so it is asked again when the
	* frame's `retry_after` says that may have changed ({@link
	* unavailableRetryMs}) — which this used to claim and did not do: it re-asked
	* at a fixed five seconds whatever the frame said. A ZERO is a read no wait
	* clears, and ANY OTHER CODE IS A DECISION; either way asking again on a
	* timer would only be answered the same: the screen's poll carries on as it
	* did before there was a push at all, and the next socket asks once more in
	* case the answer moved.
	*/
	watchAnswered(msg) {
		clearTimeout(this.watchRetry);
		this.watchRetry = 0;
		if (msg.error !== "unavailable" || this.watched === "") return;
		const wait = unavailableRetryMs(refusalOf(msg));
		if (wait === null) return;
		this.watchRetry = setTimeout(() => {
			this.watchRetry = 0;
			this.sendWatch();
		}, wait);
	}
	connect() {
		if (this.sock && (this.sock.readyState === WebSocket.OPEN || this.sock.readyState === WebSocket.CONNECTING)) return;
		const proto = location.protocol === "https:" ? "wss" : "ws";
		let sock;
		try {
			sock = new WebSocket(`${proto}://${location.host}${PATH}`);
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
			this.store.setAccessRefused(null);
			this.store.setConnected(true);
			clearTimeout(this.reconnectTimer);
			this.stopFallback();
			this.startPing();
			this.flushQueries();
			if (this.watched !== "") this.sendWatch();
		};
		sock.onmessage = (e) => this.onMessage(String(e.data));
		sock.onclose = (e) => {
			this.stopPing();
			this.sock = null;
			clearTimeout(this.watchRetry);
			this.watchRetry = 0;
			for (const entry of this.inflight.values()) {
				clearTimeout(entry.timer);
				entry.timer = 0;
			}
			this.store.setConnected(false);
			if (e && e.code === CLOSE_FORBIDDEN) {
				this.accessRefused(e.reason);
				return;
			}
			this.scheduleReconnect();
			this.startFallback();
			if (!handshakeCompleted && (!e || e.code !== CLOSE_UNAUTHENTICATED)) this.probeRefusal();
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
	* This client believed otherwise once, and the whole repair path hung off a
	* code that never arrived: a refused credential produced a dashboard that
	* reconnected for ever, said "retrying", and offered no way to correct the
	* one thing that was wrong.
	*
	* So the status is fetched where a browser will hand it over. A plain GET of
	* the same path runs the same guard, with the same cookie, and stops one line
	* short of the upgrade: 401 is nobody signed in, 426 (Upgrade Required)
	* means the session was accepted and only the missing header stopped it. A
	* throw is the network, which is not an auth problem and must not send
	* anybody to sign in.
	*/
	async probeRefusal() {
		if (this.isClosed) return;
		try {
			const res = await fetch(PATH, {
				credentials: "same-origin",
				cache: "no-store"
			});
			if (res.status === 401) this.authRejected();
			else if (res.status === 403) {
				const body = await res.json().catch(() => null);
				if (body?.error === "second_factor_enrolment_required") this.enrolmentRequired();
				else this.accessRefused(body?.detail ?? "");
			}
		} catch {}
	}
	/**
	* The engine knows who this browser is and will not serve it this surface.
	*
	* The reconnect loop and the REST fallback both STOP: each would be refused
	* by the same decision every thirty seconds for as long as the tab stayed
	* open, and a page that says "reconnecting" to somebody whose access was
	* withdrawn is telling them to wait for something that will not happen.
	* `reconnect()` is the way back, once an administrator has restored it.
	*/
	accessRefused(reason) {
		this.stopDialling();
		this.store.setAccessRefused(reason);
	}
	/**
	* The engine accepts this browser's session for nothing but enrolling the
	* second factor the deployment requires. Every dial would be refused the
	* same way until it has, so the loop stops; the enrolment ends by calling
	* `reconnect()` with the whole session it opened.
	*/
	enrolmentRequired() {
		this.stopDialling();
		needSession("second_factor");
	}
	/** Stops the reconnect loop and the REST fallback, until `reconnect()`. */
	stopDialling() {
		this.refused = true;
		clearTimeout(this.reconnectTimer);
		this.reconnectTimer = 0;
		this.stopFallback();
	}
	/**
	* The engine resolved nobody from this browser's cookie.
	*
	* Two things happen, and both are needed. Asking for a sign-in is the repair
	* — the dashboard is served unauthenticated by design (the page that signs a
	* person in cannot itself require them to be), so the browser has no other
	* moment to learn it needs one. The store flag is what the chrome reads while
	* the loop goes on dialling: a sign-in in another tab gives this one the
	* cookie too, and the next dial is what notices.
	*
	* The socket does not own the screen. It cannot: the sign-in is a route, and
	* a transport that reaches into the router is a transport that cannot be
	* tested without one — so it raises the session need the app follows.
	*/
	authRejected() {
		this.store.setAuthRejected(true);
		needSession("sign_in");
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
			case "inbox_changed":
				this.store.applyInboxChanged(msg.data);
				break;
			case "identity":
				this.store.applyIdentity(msg.data);
				break;
			case "result":
				this.settle(msg.id, null, msg.data);
				break;
			case "error":
				if (msg.what === WATCH_WHAT && msg.id === void 0) {
					this.watchAnswered(msg);
					break;
				}
				this.settle(msg.id, msg.error || "query_failed", null, refusalOf(msg));
		}
	}
	settle(id, error, data, refusal = null) {
		if (id === void 0) return;
		const entry = this.inflight.get(id);
		if (!entry) return;
		this.inflight.delete(id);
		clearTimeout(entry.timer);
		if (error) entry.reject(new QueryRefusedError(error, refusal));
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
		if (this.isClosed || this.refused) return;
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
		if (this.isClosed || this.refused || this.fallbackRun !== 0) return;
		this.fallbackRun = ++this.fallbackRuns;
		this.fallbackFetch(this.fallbackRun);
	}
	stopFallback() {
		clearTimeout(this.fallbackTimer);
		this.fallbackTimer = 0;
		this.fallbackRun = 0;
	}
	async fallbackFetch(run) {
		this.fallbackTimer = 0;
		const read = await api.snapshot();
		if (this.fallbackRun !== run || this.connected) return;
		if (read.state === "read") this.store.applySnapshot(read.snapshot);
		const next = read.state === "unread" && read.retryAfter !== null ? retryAfterMs(read.retryAfter) : FALLBACK_MS;
		if (next === null) return;
		this.fallbackTimer = setTimeout(() => void this.fallbackFetch(run), next);
	}
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
* `engine.GateClientWait`, which the command line waits too: the engine bounds
* a gesture from its first record to its last answer at `engine.GateBudget` —
* one gate record per identity-claiming log, two five-second resolutions each,
* at a margin of three, so two minutes for this build's four logs — and a
* client waits one more resolution at that margin for the judgement before
* the first record and the round trip around the gesture. Waiting past the
* node's own bound is what makes its answer — every log's outcome — reach the
* operator rather than a client timeout that knows none of it. The default
* thirty seconds gave up on a gesture the node went on to finish.
*
* HELD EXACTLY to the engine's value, not merely above its budget, because the
* budget is counted from the register: a copy that was only larger went on
* passing the day a new identity log raised the budget past it.
*/
var GATE_REQUEST_TIMEOUT_MS = 135e3;
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
//#region src/protocol/auth.ts
/**
* The sign-in surface, `/auth`, as the functions the screens call.
*
* ONE MODULE over `rest.ts`, for the reason `rest.ts` gives about itself: a
* screen that composed its own path, header and body for each of these would
* be a second place for the invitation's secret to end up in a URL, and that
* is the one mistake this surface exists to make impossible.
*
* WHAT NONE OF THESE DOES is keep a credential. Every answer that signs a
* person in sets a cookie the browser holds and no script can read, and the
* body carries no bearer at all; the one credential a function here is
* handed — a Tier A token being exchanged — is sent once, in the header, and
* dropped. There is nothing for this page to store, so it stores nothing.
*/
/**
* The header an invitation's secret travels in to the view.
*
* A HEADER, because the view is a GET and a GET has no body — and never the
* query string, which every access log between the browser and the engine
* records. The link carries the secret in its FRAGMENT precisely so that no
* server sees it on the way to this page.
*/
var INVITE_SECRET_HEADER = "X-Crewlet-Invite-Secret";
/** The invitation route for one id, its segment encoded. */
function invitePath(id) {
	return `/auth/invite/${encodeURIComponent(id)}`;
}
var auth = {
	/** What a sign-in page may know before anybody has signed in. */
	config: async () => await rest.get("/auth/config"),
	/**
	* Sign in with a login or an address and a password — and, once the engine
	* has answered `second_factor_required`, the code.
	*/
	login: async (body) => await rest.post("/auth/login", body),
	/**
	* Exchange a Tier A token for a one-hour session.
	*
	* THE TOKEN IS THE BEARER OF THIS ONE REQUEST and nothing else: it goes in
	* the `Authorization` header, which is how the engine takes it, and the
	* session that comes back is a cookie. Nothing here holds it afterwards.
	*/
	exchangeToken: async (token) => await rest.post("/auth/token", {}, { Authorization: `Bearer ${token}` }),
	/** Render an invitation without spending it. */
	viewInvite: async (id, secret) => (await rest.request("GET", invitePath(id), { headers: { [INVITE_SECRET_HEADER]: secret } })).body,
	/** Redeem an invitation, which creates the person and signs them in. */
	redeemInvite: async (id, body) => await rest.post(invitePath(id), body),
	/**
	* Enrolment's first leg: a seed, and nothing stored. A person who never
	* completes the second leg has enrolled nothing.
	*/
	secondFactorSeed: async () => await rest.post("/auth/totp", {}),
	/**
	* Enrolment's second leg: the seed back, with a code derived from it — the
	* only evidence the authenticator on the other side works.
	*/
	enrolSecondFactor: async (secret, code) => await rest.post("/auth/totp", {
		secret,
		code
	}),
	/** Ten fresh single-use codes, retiring the old set, shown this once. */
	recoveryCodes: async () => await rest.post("/auth/totp/recovery", {}),
	/**
	* Confirm who you are on a session that is already valid: the password,
	* and the code where a second factor is held. The engine answers a fresh
	* session cookie and ends the one it replaces.
	*/
	stepUp: async (body) => await rest.post("/auth/step-up", body),
	/** Who this browser is signed in as. */
	session: async () => await rest.get("/auth/session"),
	/**
	* End this browser's session. The engine clears the cookie whatever its
	* own write did, so an answer at all means this browser holds no session.
	*/
	logout: async () => {
		await rest.post("/auth/logout", {});
	},
	/**
	* End every session the caller holds, on every device, by moving their
	* revocation epoch — this browser's included.
	*/
	logoutEverywhere: async () => {
		await rest.post("/auth/logout/all", {});
	}
};
//#endregion
export { GATE_ACTIONS, GATE_ACTIONS_KEEPING_OPERATION, GATE_REQUEST_TIMEOUT_MS, LiveSocket, MAX_EVENTS, QueryRefusedError, REQUEST_TIMEOUT_MS, RETRY_AFTER_MAX_MS, RestError, Store, UNAVAILABLE_RETRY_MS, api, auth, confirmStepUp, currentSessionNeed, isAbort, isLogRefusal, keepsOperation, layoutOpID, needSession, newGateOpID, onSessionNeed, queryErrorCode, queryFailure, refusedGrants, rest, restRetryMs, retryAfterMs, sessionNeedsEnrolment, sessionRestored, setStepUpConfirmer, unavailableRetryMs };
