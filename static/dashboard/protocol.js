//#region src/protocol/share.ts
/**
* An answer that did not change keeps the objects it was drawn from.
*
* # Why identity is the thing worth keeping
*
* Every list in this product is drawn by the one grid
* (`app/frame/DataGrid.tsx`), whose rows are memoised on the OBJECT each row
* is drawn from — and never on its place: a row whose object is the one it was
* drawn from last time is not drawn again, wherever it now sits. A poll
* defeats that by construction — every answer is parsed afresh off the wire,
* so a poll that brought back exactly what the screen already held handed the
* grid two hundred new objects, and the grid drew two hundred rows to change
* no pixel. Measured on the turns list and the audit under the development
* build: every row of both, on every poll, whatever the answer said.
*
* So an answer is SHARED with the one it replaces before anything reads it:
* every part of the new answer that is deep-equal to a part of the old one is
* replaced by the old part. An unchanged answer comes back as the very object
* the screen already holds, which renders nothing at all; an answer in which
* one row moved comes back as a new list holding the old objects for every
* row but that one, which draws that one row.
*
* # A row is matched by its content, not only by its place
*
* The obvious walk — compare each element with the one at the same index —
* is what most implementations do, and it is wrong for the lists this product
* polls most: a feed ordered newest-first. One new turn at the top moves every
* other row down a place, so not one of them equals the element at its own
* index, and every row is drawn again for an answer that changed by one. So
* an element that does not equal the one at its own place is looked up among
* EVERY element of the old list — bucketed by a short print of its leading
* fields, then confirmed by the same deep comparison — and reused from
* wherever it was.
*
* # Only data is walked
*
* An array and an object literal are looked inside; anything else — a `Map`,
* a `Date`, a class instance, a function, and above all a React element, which
* is an object literal whose `_owner` is a fiber that reaches the whole tree —
* is kept only where it is the SAME value, never compared by its contents.
* Answers off the socket are JSON and are all data; what a screen derives
* from one may not be, and walking into a fiber to compare two of them would
* be a walk of the application.
*
* Nothing here mutates either argument, and the result is always deep-equal to
* `next`: sharing changes which objects an answer is made of, never what it
* says.
*
* # Why it is in the protocol layer
*
* Every way an answer reaches a screen goes through it: a question
* (`lib/useQuery.ts`), a REST read (`lib/restRead.ts`) and every push the
* store replaces a slice with (`./store.ts`) — the engine's health among them,
* which is pushed whole rather than polled. The store is here, and nothing here may import React — the
* hook that shares a value a SCREEN derives is `lib/share.ts`.
*/
/**
* `next`, with every part deep-equal to a part of `prev` replaced by that part.
*
* Returns `prev` itself when the two are deep-equal.
*/
function share(prev, next) {
	return shareValue(prev, next);
}
/** Whether the walk may look inside a value: an array or an object literal. */
function walkable(value) {
	if (value === null || typeof value !== "object") return false;
	if (Array.isArray(value)) return true;
	const proto = Object.getPrototypeOf(value);
	if (proto !== Object.prototype && proto !== null) return false;
	return !("$$typeof" in value);
}
function shareValue(prev, next) {
	if (Object.is(prev, next)) return prev;
	if (Array.isArray(prev) && Array.isArray(next)) return shareList(prev, next);
	if (walkable(prev) && walkable(next) && !Array.isArray(prev) && !Array.isArray(next)) return shareRecord(prev, next);
	return next;
}
function shareRecord(prev, next) {
	const keys = Object.keys(next);
	let same = keys.length === Object.keys(prev).length;
	const out = {};
	for (const key of keys) {
		const had = Object.prototype.hasOwnProperty.call(prev, key);
		const value = had ? shareValue(prev[key], next[key]) : next[key];
		out[key] = value;
		if (!had || value !== prev[key]) same = false;
	}
	return same ? prev : out;
}
function shareList(prev, next) {
	let same = prev.length === next.length;
	const out = new Array(next.length);
	let elsewhere = null;
	for (let i = 0; i < next.length; i++) {
		const value = next[i];
		const kept = i < prev.length ? shareValue(prev[i], value) : value;
		out[i] = kept;
		if (i < prev.length && kept === prev[i]) continue;
		same = false;
		if (!walkable(value)) continue;
		elsewhere ??= buckets(prev);
		for (const candidate of elsewhere.get(print(value)) ?? []) if (shareValue(candidate, value) === candidate) {
			out[i] = candidate;
			break;
		}
	}
	return same ? prev : out;
}
/** Every walkable element of a list, by its [print]. */
function buckets(list) {
	const out = /* @__PURE__ */ new Map();
	for (const item of list) {
		if (!walkable(item)) continue;
		const key = print(item);
		const bucket = out.get(key);
		if (bucket) bucket.push(item);
		else out.set(key, [item]);
	}
	return out;
}
/**
* How many of a row's own fields its [print] reads, and how much of each.
*
* Enough to tell the rows of one list apart, which is all a bucket is for: a
* row off the wire leads with what identifies it — an id, a key, an instant,
* each well under sixty-four characters (a uuid is 36) — and eight fields is
* past every row's identity in this protocol. A collision costs one deep
* comparison, never a wrong answer.
*/
var PRINT_FIELDS = 8;
var PRINT_CHARS = 64;
/**
* Which bucket a value is looked for in: a SHALLOW print of it.
*
* A BUCKET, NOT A VERDICT: two deep-equal values whose keys run in one order
* print alike — every row one encoder wrote and every row one derivation built
* — and a match is confirmed by the deep comparison, so two values that print
* alike and differ are told apart there. Two that differ only in the ORDER of
* their keys are deep-equal and print apart, which costs that row a drawing
* and never a wrong answer. SHALLOW AND SHORT because it is taken of
* every row of a list each time anything in that list moves, and a row may be
* large — a phase record carries its whole prompts and response, and a full
* print of each would be megabytes of string built to find one new row at the
* top. Not `JSON.stringify` for the same reason, and because that reads a
* `Map` as `{}` and walks into a React element's fiber until it meets a cycle.
*/
function print(value) {
	if (Array.isArray(value)) return `[${value.length}`;
	const parts = [];
	for (const key of Object.keys(value).slice(0, PRINT_FIELDS)) {
		const field = value[key];
		let shown;
		if (field !== null && typeof field === "object") shown = Array.isArray(field) ? `[${field.length}` : "{";
		else shown = `${(typeof field).charAt(0)}${String(field).slice(0, PRINT_CHARS)}`;
		parts.push(`${JSON.stringify(key)}=${shown}`);
	}
	return parts.join(",");
}
//#endregion
//#region src/contract/wire.ts
/**
* Longest activity feed a tab keeps.
*
* EXACTLY THE SERVER'S OWN (`livestate.EventFeedLimit`), held there by
* `internal/api/livestate`'s feed gate, so a reconnect's snapshot neither
* truncates the feed nor leaves rows the server cannot resend. It is also the
* limit of what anything derived from the feed can HONESTLY claim to know: a
* busy company fills it in minutes, and a panel covering an hour has to say
* where the record actually starts rather than drawing the gap as quiet.
*/
var MAX_EVENTS = 400;
//#endregion
//#region src/protocol/store.ts
/**
* How many completed-phase envelopes a tab keeps, PAYLOAD AND ALL.
*
* This is the one slice retained for its payload rather than for its row, and
* it exists because a live phase has no durable half until one arrives: the
* projection clears `live_call` the instant a phase completes, and the query
* that answers a seat's history was answered ONCE, at mount. Without this the
* turn a reader is watching vanishes the moment its last phase lands — most
* visibly on a seat's FIRST turn, where the mount-time history is empty and the
* page is left claiming the seat has never run.
*
* 200, and the bound is on the PAYLOADS rather than on the rows: a phase carries
* its verbatim system prompt, its response and every tool result, which is why
* the server caps one page of these same rows at 60 on row size alone
* (`store.MaxPhasePage`) and a seat's history at 50 (`store.AgentPhaseLimit`).
* 200 is above every one of those and above the ~40 phases a turn reaches when
* it self-iterates to the default cap of 3 with a full 8-task delegate fan-out
* each round — so a tab watching one turn keeps all of it — while staying inside
* what a browser should hold in payloads of this size.
*
* Eviction is drop-oldest and the buffer is COMPANY-WIDE, because one socket
* serves every screen. So this is not a guarantee: a fleet completing more than
* 200 phases while a tab sits open can evict a record that tab still wants, and
* a turn then renders with a phase missing rather than with all of them. What
* bounds the damage is that these only ever SUPPLEMENT a query answer — every
* screen re-asks on reconnect, and a reload is authoritative — so the loss is a
* card that is late, never a turn that is gone.
*/
var MAX_PHASES = 200;
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
		authRejected: false,
		identityUnverifiable: false,
		accessRefused: null,
		inboxMoves: {},
		orgPushes: 0
	};
}
var Store = class {
	state = emptyState();
	/**
	* The push kinds this build does not know, each with how many arrived.
	*
	* IGNORED AND COUNTED. A node on another build may push a kind this bundle
	* was built before, and throwing on it — or applying it to a slice by a
	* guess — would break a screen over a frame it has no use for. But the same
	* fall-through is exactly what this build's own engine sending a kind its
	* own client forgot looks like, which is the silent failure the e2e replay
	* exists to catch: so it is kept here, and the replay asserts it is empty.
	* Not a slice, because nothing renders it and a listener woken by a frame
	* nobody can read would be woken for nothing.
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
	/**
	* Replace one slice with what was pushed, SHARED with what it held
	* (`./share.ts`), and say whether anything moved.
	*
	* A push is an answer like any other — parsed afresh off the wire — so a
	* spend rollup pushed after every phase handed the spend tables a new object
	* for every seat and every turn, and every row of both was drawn again for
	* the one turn that finished. Shared, a push that changed nothing moves no
	* version and wakes nobody, and one that changed something keeps the objects
	* of everything it did not change.
	*/
	replace(slice, next) {
		const kept = share(this.state[slice], next);
		if (kept === this.state[slice]) return false;
		this.state[slice] = kept;
		return true;
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
		const moved = [];
		const put = (slice, next) => {
			if (this.replace(slice, next)) moved.push(slice);
		};
		put("agents", snap.agents ?? []);
		put("events", (snap.events ?? []).slice(0, 400));
		put("sandboxes", snap.sandboxes ?? []);
		put("org", snap.org ?? {});
		put("tools", snap.tools ?? []);
		if (snap.tokens && snap.tokens.totals) put("tokens", snap.tokens);
		put("budget", snap.budget ?? null);
		if (snap.schedules) put("schedules", snap.schedules);
		if (snap.health) put("health", snap.health);
		if (moved.length > 0) this.emit(...moved);
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
		const merged = this.state.agents.map((a) => {
			const patch = byID.get(a.agent_id);
			return patch ? {
				...a,
				...patch
			} : a;
		});
		if (this.replace("agents", merged)) this.emit("agents");
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
		const roster = rows.map((row) => {
			const current = live.get(row.agent_id);
			return current ? {
				...current,
				...row
			} : row;
		});
		if (this.replace("agents", roster)) this.emit("agents");
	}
	applySandboxes(list) {
		if (this.replace("sandboxes", list ?? [])) this.emit("sandboxes", "agents");
	}
	applyTokens(rollup) {
		if (!rollup) return;
		if (this.replace("tokens", rollup)) this.emit("tokens");
	}
	applyBudget(budget) {
		if (this.replace("budget", budget ?? null)) this.emit("budget");
	}
	applySchedules(payload) {
		if (!payload) return;
		if (payload.schedules && this.replace("schedules", payload.schedules)) this.emit("schedules");
	}
	applyOrg(org) {
		this.state.orgPushes += 1;
		if (this.replace("org", org ?? {})) this.emit("org", "orgPushes");
		else this.emit("orgPushes");
	}
	applyTools(tools) {
		if (this.replace("tools", tools ?? [])) this.emit("tools");
	}
	applyHealth(health) {
		const connected = !!health && health.status !== "unknown";
		if (!this.replace("health", health ?? { status: "unknown" }) && connected === this.state.connected) return;
		this.state.connected = connected;
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
	/** Count one frame whose `kind` this build does not dispatch. */
	noteUnknownPush(kind) {
		const name = typeof kind === "string" ? kind : JSON.stringify(kind ?? null);
		this.unknownPushes.set(name, (this.unknownPushes.get(name) ?? 0) + 1);
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
//#region src/contract/retry.ts
/**
* The two waits the dashboard's retries are bounded by that the ENGINE owns:
* what it says when it has nothing better, and the longest hint it fixes.
*
* Each is a COPY of an engine value, held to it by `internal/api`'s
* `TestTheDashboardRetriesOnTheEnginesOwnHints`. What is done with them — the
* arithmetic over a hint, the backoff for a request nobody answered — is
* behaviour, and lives in `protocol/retry.ts`.
*/
/**
* How soon an `unavailable` answer that carries NO hint is asked again, in
* milliseconds.
*
* ONLY THE FALLBACK. Every `unavailable` frame the engine sends carries
* `retry_after`, so an answer without one is one nobody decided a hint for.
* FIVE SECONDS because that is what the engine itself says when it has nothing
* better: its shared health tick (`stream.HealthInterval`), the soonest a
* node's posture can change and the hint every `unavailable` it cannot say
* more about carries. Waiting it out is asking as the engine would have asked;
* sooner asks before anything could have changed, and later leaves a recovered
* node looking broken.
*/
var UNAVAILABLE_RETRY_MS = 5e3;
/**
* The longest a hint is waited out before asking again anyway, in
* milliseconds.
*
* A BOUND ON THE ONE HINT THAT HAS NONE. Every hint the engine fixes — the
* health tick (5 s), a quorum election (4 s), the identity estate's and an
* undecidable authority's two seconds, a surface another writer holds (3 s),
* the reconcile poll that brings a company and the floor's heartbeat (15 s), a
* drain (30 s) — is at or under thirty seconds and is waited out exactly. The
* one this cuts is DERIVED: a node's backlog divided by the rate it has been
* draining at, which is minutes on a node that has just joined or restarted
* behind a busy log, and an estimate made from a rate that only rises as the
* node warms up. Past thirty seconds a screen waiting on it says "this fills
* in on its own" for longer than a person believes it, and the cost of asking
* sooner is one read the node refuses again. THIRTY because it is already this
* dashboard's ceiling on waiting for an engine to come back — the socket's
* reconnect backoff and the builder's check backoff stop there for the same
* reason — and the gate fails the day a hint the engine fixes grows past it.
*/
var RETRY_AFTER_MAX_MS = 3e4;
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
* by the socket, so it imports nothing but the contract module that declares
* the engine's two waits — itself data and nothing else, which the contract's
* own suite holds: the builder's model takes nothing from this directory at
* runtime but this file, because the rest of it is the socket and `fetch`
* (see `routes/org/builder/model/boundary.test.ts`).
*
* What it waits is bounded by the engine's own two values — the fallback
* `UNAVAILABLE_RETRY_MS` and the ceiling {@link RETRY_AFTER_MAX_MS} —
* declared in `contract/retry.ts`.
*/
/**
* The wait a hint of `seconds` asks for, in milliseconds, or `null` for "do not
* ask again on a timer".
*
* ZERO IS THE ANSWER, never an omission: the engine is saying waiting will not
* change it, so nothing re-asks — the screen shows the refusal and what it
* names, and it is asked again only when something a person does could have
* changed it (a reconnect, a write, a reload). A caller whose answer carried no
* hint at all does not come here: what an absent hint means is the caller's —
* the socket's is `UNAVAILABLE_RETRY_MS` (`contract/retry.ts`), and a request
* that never reached the engine backs off ({@link unansweredRetryMs}).
*
* The hint is whole seconds and never negative — both parsers that read one
* admit nothing else — so anything at or under zero is the zero.
*/
function retryAfterMs(seconds) {
	if (!(seconds > 0)) return null;
	return Math.min(seconds * 1e3, RETRY_AFTER_MAX_MS);
}
/**
* The wait before the first retry of a request NOBODY ANSWERED, in
* milliseconds: one that never came back (its deadline passed, the connection
* dropped), or one something in front of the engine answered instead.
*
* THERE IS NO HINT TO WAIT OUT, because the engine said nothing — and that is
* not the engine saying waiting will not change it, which is the zero above.
* So the wait is the client's own, and it BACKS OFF: asking at once would
* hammer an engine that is restarting, or a network that is down, with
* requests that each wait out the transport's deadline. One second is long
* enough not to spin against a refused connection and short enough to notice
* a restarted engine the moment it accepts one.
*/
var UNANSWERED_RETRY_BASE_MS = 1e3;
/**
* The longest wait between retries of a request nobody answered, in
* milliseconds: `REQUEST_TIMEOUT_MS` in `rest.ts`, the longest one attempt may
* itself take, so an engine that recovers is never noticed later than one more
* attempt would have taken to fail. `retry.test.ts` holds the two equal; this
* file imports nothing from this directory, so it cannot name the other.
*/
var UNANSWERED_RETRY_MAX_MS = 3e4;
/**
* The wait before retry `failures` of a request nobody answered — 1 for the
* first — doubling from {@link UNANSWERED_RETRY_BASE_MS} up to
* {@link UNANSWERED_RETRY_MAX_MS}.
*
* ONE BACKOFF for every such request: the org builder's check and a screen's
* REST read are the same question put to the same engine, and two copies of
* the arithmetic would be two answers to how hard this page leans on a node
* that is not answering.
*/
function unansweredRetryMs(failures) {
	const exponent = Math.max(0, failures - 1);
	return Math.min(UNANSWERED_RETRY_MAX_MS, UNANSWERED_RETRY_BASE_MS * 2 ** Math.min(exponent, 30));
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
	* The `Retry-After` the answer carried, in whole seconds, or null for none —
	* either form the RFC allows ([retryAfterSeconds]). A `429` always carries
	* one and says how long the curve makes the next attempt wait; a `503` the
	* engine wrote carries one where waiting can clear the cause and none where
	* it cannot, which is a difference a screen has to render. A refusal
	* something in front of the engine wrote keeps its header here too, though
	* [retryHint] never reads it as the engine's.
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
* its own. Decided on the code [restFailure] draws the failure as, so the
* banner and the timer can never disagree about which failure this is.
*
* - A `503` the engine wrote says when ([RestError.retryHint]), read through
*   `retryAfterMs`: waited out exactly, bounded, and its ZERO — a `503` with
*   no `Retry-After` — never on a timer, because the engine is saying waiting
*   will not change the answer.
* - A read NOBODY ANSWERED backs off (`unansweredRetryMs`), from a second to
*   thirty. It is the one failure with nothing that would ever ask again
*   otherwise: the live socket can be up the whole time — a request past its
*   deadline on a slow engine, one dropped on the way — so its coming back
*   never happens, and a screen with no poll of its own held the banner until
*   somebody reloaded. A `Retry-After` something in front of the engine wrote
*   — a proxy's `503` page — is not waited out: nobody at the engine decided
*   it, and the backoff is already this tab's whole answer to a node it
*   cannot hear.
* - Every other failure carries no hint, since nobody at the engine decided
*   one, and waits the screen's own `cadence`.
*/
function restRetryMs(err, context) {
	switch (restFailure(err).error) {
		case "unavailable": return retryAfterMs(err.retryHint ?? 0);
		case "unanswered": return unansweredRetryMs(context.unanswered);
		default: return context.cadence;
	}
}
/**
* A failed REST read in `QueryState`'s terms: the code its banner is chosen
* by, and the refusal that lets the banner say what would change the answer.
*
* THE REST TWIN OF `queryFailure`, for a screen that reads over REST and draws
* its failure the way a socket question's is drawn. Each such screen used to
* map a failure for itself, and each forgot a different case: the credential
* listing drew an engine `503` as a fault on the node, the Integrations
* listing drew nothing for any failure but a refusal and a `503`, and the pass
* history drew a first read that failed as "No pass has run on this node" — an
* answer about the integration that nobody gave.
*
* - A refusal on AUTHORITY (`401`, `403`) is `unauthorized`, carrying the rule
*   and the grants it named.
* - A `503` the engine wrote ([RestError.retryHint]) is `unavailable`, with
*   its state-log code and hint: the banner says the screen asks again on its
*   own, or — at zero — that asking will not change it.
* - A read NO ANSWER FROM THE ENGINE CAME BACK TO ([RestError.unanswered]) is
*   `unanswered`: status 0 — a request past its thirty-second deadline, one
*   dropped on the way — or a status something in front of the engine wrote
*   (a gateway's `502` or `504`, a body cut off part way). Nothing refused it
*   and nothing here knows what the engine would have said, and its banner
*   says the screen asks again on its own, which [restRetryMs]'s backoff
*   makes true. It was `closed`, whose banner says the SOCKET went away and
*   the screen reads again once it is back: while the socket stayed up — the
*   ordinary case for one slow request — neither was true, and the screen
*   read again only on a reload. And a gateway's answer was `query_failed`,
*   "the engine tried to answer and failed", about an answer the engine never
*   wrote.
* - Anything else is `query_failed`, a fault on the node: a `500` it wrote.
*
* A `404` is the CALLER'S to read first, because what it means is the route's
* — the credential surface unregistered on this process, or one pass nobody
* remembers — and reading it here would say one of those about the other.
*/
function restFailure(err) {
	if (!(err instanceof RestError)) return {
		error: "query_failed",
		refusal: null
	};
	if (err.unauthorized) return {
		error: "unauthorized",
		refusal: err.refusal
	};
	if (err.retryHint !== null) return {
		error: "unavailable",
		refusal: err.refusal
	};
	if (err.unanswered) return {
		error: "unanswered",
		refusal: null
	};
	return {
		error: "query_failed",
		refusal: null
	};
}
/**
* A `Retry-After` header value as whole seconds from `now`, or null for a
* header that is absent or says nothing usable.
*
* BOTH FORMS RFC 9110 ALLOWS. The engine writes delay-seconds, but a proxy in
* front of it may answer for it with an HTTP-date, and reading that as "no
* hint" would drop the one instruction the refusal carried. A date already
* past is a wait of zero, never a negative one. Which of the two WROTE it —
* and so whether a zero means "waiting will not change it" — is not this
* function's question: that is [RestError.retryHint]'s rule.
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
/** The seconds a response's `Retry-After` names, or null for none. */
function retryAfterOf(response) {
	return retryAfterSeconds(response.headers.get("Retry-After"), Date.now());
}
/**
* [RestError.retryHint] for a refused response that did not come through
* [request] — the degraded-mode snapshot (`api.ts`) and the socket's
* plain-HTTP re-ask of a refused handshake (`socket.ts`), each of which reads
* the response itself.
*
* ONE READING for both, because each needs the same three steps — the body
* read as an envelope whatever it holds, the header read as whole seconds, and
* the rule for which `503` is the engine's — and each spelled them out for
* itself, which is how two copies come to disagree about a proxy's `503`.
* Consumes the body.
*/
async function retryHintOf(response) {
	const body = await response.json().catch(() => null);
	const envelope = body !== null && typeof body === "object" ? body : {};
	return new RestError(response.status, envelope, retryAfterOf(response)).retryHint;
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
* gate is allowed two minutes from its first record to its last answer
* (`contract/gate.ts`), so thirty seconds gave up on a gesture the node went
* on to finish, holding nothing to finish it with.
*/
var REQUEST_TIMEOUT_MS = 3e4;
/**
* A deadline as a person would say it: in minutes where it is a whole number
* of them past the first, and in seconds otherwise — EXACT either way, because
* the sentence names the deadline that ran out, and the node gate's two
* minutes and fifteen seconds rounded to "2 minutes" would be a deadline
* nobody set.
*/
function waitWords(ms) {
	const seconds = Math.round(ms / 1e3);
	return seconds >= 120 && seconds % 60 === 0 ? `${seconds / 60} minutes` : `${seconds} seconds`;
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
		}, response.ok ? null : retryAfterOf(response));
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
	/**
	* A read, ended by `signal` where the caller passes one: a read whose
	* screen went, or whose answer a newer read superseded, has nobody left to
	* hand its answer to (`lib/restRead.ts`).
	*/
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
	})
};
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
* and a screen reads those through `lib/restRead.ts`. What this file keeps is
* its own separation: it had a second entry once, and that one is why the
* Fleet screen shipped dead — a screen reaching for its own transport takes
* its client from somewhere, and the somewhere it chose was a context field
* the shell never populated. Only `socket.ts` imports this file.
*/
var api = { 
/** The degraded-mode snapshot, or why not and when to ask again. */
async snapshot() {
	try {
		const response = await fetch(location.origin + "/stream/snapshot", { credentials: "same-origin" });
		if (!response.ok) return {
			state: "unread",
			retryAfter: await retryHintOf(response)
		};
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
* The socket is the channel for the projection and for every question the
* query registry answers — not for everything. Writes and the reads no
* question answers go over REST through `rest.ts` and `act.ts`, and this file
* makes two HTTP requests of its own: the degraded snapshot, which keeps the
* page honest while the socket is down (a proxy that refuses to upgrade, a
* restarting engine) and stops the moment the socket is back, and the refusal
* probe after a handshake that never opened.
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
*
* AND A `bad_params` REFUSAL'S SENTENCE rides beside it as `detail` — the one
* refusal the engine writes FOR the caller, naming the parameter to change and
* what it accepts; every other failure's text stays in the node's log, so this
* is never a path or a driver's message. A rejection carrying only the code
* left a screen to say "something was missing" about a window the reader
* chose. An `unavailable` answer's words are its {@link LogRefusal}'s.
*/
var QueryRefusedError = class extends Error {
	refusal;
	detail;
	constructor(code, refusal, detail = null) {
		super(code);
		this.refusal = refusal;
		this.detail = detail;
		this.name = "QueryRefusedError";
	}
};
/**
* A failed `query` as the pair `QueryState` renders from.
*
* ONE READING of a rejection, for every surface that asks outside `useQuery` —
* a page of older rows, a question asked once on a press. Read inline at
* each, the refusal was the half a surface forgot: its screen said a read was
* refused and not which grant would have admitted the reader, although the
* answer had named it.
*/
function queryFailure(err) {
	const refused = err instanceof QueryRefusedError ? err : null;
	return {
		error: err instanceof Error ? err.message : "query_failed",
		refusal: refused?.refusal ?? null,
		detail: refused?.detail ?? null
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
* query (see `useQuery`) or a watch — in milliseconds, or `null` for "not on a
* timer".
*
* `unavailable` is the engine saying it cannot answer HERE, and its frame says
* when that may change: `retry_after`, read through {@link retryAfterMs} —
* waited out, bounded, and ZERO meaning waiting will not change it, so nothing
* re-asks. An answer carrying no hint waits {@link UNAVAILABLE_RETRY_MS}, what
* the engine says when it has nothing better; so does a refusal that is not
* the state log's, which no `unavailable` answer carries.
*
* ONE READING for both, because the engine's answer is one: a query and a
* watch refused `unavailable` by the same node are waiting on the same thing.
* The fixed five seconds each of them re-asked at whatever the frame said is
* what this replaced.
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
* A `bad_params` frame's sentence, or null for any other frame. The engine
* writes that one refusal for the caller; an `unavailable` frame's words are
* its refusal's ({@link refusalOf}), and every other frame carries none.
*/
function badParamsDetail(msg) {
	return msg.error === "bad_params" && typeof msg.detail === "string" && msg.detail !== "" ? msg.detail : null;
}
/**
* The refusal an error frame carries, or null — for a frame that is neither a
* refusal on authority nor an `unavailable` answer, or a node too old to say
* why.
*/
function refusalOf$1(msg) {
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
		const wait = unavailableRetryMs(refusalOf$1(msg));
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
			if (e && e.code === 4403) {
				this.accessRefused(e.reason);
				return;
			}
			this.scheduleReconnect();
			this.startFallback();
			if (!handshakeCompleted && (!e || e.code !== 4401)) this.probeRefusal();
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
	* means the session was accepted and only the missing header stopped it, and
	* a `503` the engine wrote is a node that could not decide the handshake yet
	* — its identity estate unreadable for a moment — which says in its
	* `Retry-After` when to dial again (see `redialWhenSaid`). A throw is the
	* network, which is not an auth problem and must not send anybody to sign
	* in.
	*/
	async probeRefusal() {
		if (this.isClosed) return;
		try {
			const res = await fetch(PATH, {
				credentials: "same-origin",
				cache: "no-store"
			});
			if (res.status === 401) this.authRejected();
			else if (res.status === 503) await this.redialWhenSaid(res);
			else if (res.status === 403) {
				const body = await res.json().catch(() => null);
				if (body?.error === "second_factor_enrolment_required") this.enrolmentRequired();
				else this.accessRefused(body?.detail ?? "");
			}
		} catch {}
	}
	/**
	* A refused handshake's `503`: dial again when the engine said, in place of
	* the backoff.
	*
	* THE HINT WAS ON THE WIRE AND THE LOOP NEVER READ IT. A node that cannot
	* read its identity estate answers every guarded route, this handshake
	* included, `503` with a `Retry-After` of two seconds — the estate's own
	* catch-up scale — and the loop went on doubling its own wait: a few refused
	* dials in, a tab was sitting out sixteen or thirty seconds where its node
	* had said two, and on the first refusal it dialled at one second, before the
	* node had said it could answer. Waited out through `retryAfterMs`, like
	* every other hint: exactly, and bounded at `RETRY_AFTER_MAX_MS`.
	*
	* ONLY THE ENGINE'S `503`, by the rule `RestError.retryHint` keeps — a proxy
	* in front of a node that is down writes one too, and that is the backoff's
	* case. The probe runs beside the reconnect rather than before it, so the
	* hint can land with the next dial already out; the dial it schedules then
	* finds one in flight and does nothing (`connect`), and that dial's own
	* close schedules what follows it.
	*
	* A ZERO KEEPS THE BACKOFF rather than stopping the loop, unlike every other
	* re-ask in this dashboard, because a dial is not a re-ask of this one node:
	* behind a balancer the next one may reach another — which is what a zero
	* tells a client to do — and the loop is this tab's only way back to any
	* engine. Its backoff already caps it at one dial every thirty seconds. No
	* handshake refusal the engine writes today carries a zero; this says what a
	* future one would get.
	*/
	async redialWhenSaid(res) {
		const hint = await retryHintOf(res);
		const wait = hint === null ? null : retryAfterMs(hint);
		if (wait !== null) this.scheduleReconnect(wait);
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
				this.settle(msg.id, msg.error || "query_failed", null, refusalOf$1(msg), badParamsDetail(msg));
				break;
			case "pong": break;
			default: this.store.noteUnknownPush(msg.kind);
		}
	}
	settle(id, error, data, refusal = null, detail = null) {
		if (id === void 0) return;
		const entry = this.inflight.get(id);
		if (!entry) return;
		this.inflight.delete(id);
		clearTimeout(entry.timer);
		if (error) entry.reject(new QueryRefusedError(error, refusal, detail));
		else entry.resolve(data);
	}
	failInflight(reason) {
		for (const entry of this.inflight.values()) {
			clearTimeout(entry.timer);
			entry.reject(new Error(reason));
		}
		this.inflight.clear();
	}
	/**
	* Dial again after the backoff — or after `after` ms, where the engine said
	* when (`redialWhenSaid`), which replaces the dial already scheduled and
	* leaves the backoff's count where it was.
	*/
	scheduleReconnect(after) {
		if (this.isClosed || this.refused) return;
		clearTimeout(this.reconnectTimer);
		let delay = after;
		if (delay === void 0) {
			delay = Math.min(1e3 * 2 ** Math.min(this.attempt, 10), MAX_BACKOFF_MS);
			this.attempt++;
		}
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
/**
* How long one gesture's request may take before the dialog gives up on it —
* `engine.GateClientWait`, held EXACTLY by
* `internal/api.TestTheDashboardWaitsAsLongAsTheEngineSaysAClientShould`.
*
* The command line waits it too: the engine bounds a gesture from its first
* record to its last answer at `engine.GateBudget` — one gate record per
* identity-claiming log, two five-second resolutions each, at a margin of
* three, so two minutes for this build's four logs — and a client waits one
* more resolution at that margin for the judgement before the first record and
* the round trip around the gesture. Waiting past the node's own bound is what
* makes its answer — every log's outcome — reach the operator rather than a
* client timeout that knows none of it. The default thirty seconds gave up on a
* gesture the node went on to finish.
*
* HELD EXACTLY, not merely above the budget, because the budget is counted
* from the register: a copy that was only larger went on passing the day a new
* identity log raised the budget past it.
*/
var GATE_REQUEST_TIMEOUT_MS = 135e3;
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
	viewInvite: async (id, secret) => (await rest.request("GET", `/auth/invite/${encodeURIComponent(id)}`, { headers: { [INVITE_SECRET_HEADER]: secret } })).body,
	/** Redeem an invitation, which creates the person and signs them in. */
	redeemInvite: async (id, body) => await rest.post(`/auth/invite/${encodeURIComponent(id)}`, body),
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
	/**
	* Who this browser is signed in as — ended by `signal` where the caller
	* passes one, for a read a newer one has superseded.
	*/
	session: async (signal) => await rest.get("/auth/session", signal),
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
//#region src/contract/actions.ts
/**
* Every change the dashboard makes, as the tool the engine runs for it.
*
* The dashboard writes through ONE route — `POST /operator/act/{tool}`, as the
* principal the request resolves to (ADR-0024): a person the identity
* directory binds to a seat writes as that seat, a credential nobody is bound
* through under its own login, and nobody is refused for being unbound — what
* each may do is the authority table's, as on every surface. The tools behind
* it are the operator catalogue's, the same implementations a person's own
* assistant calls. This table is what the dashboard is allowed to send there:
* `protocol/act.ts` takes only a key of it, and only the arguments its row
* names, as the body's `args`; the operation is the request's
* `Idempotency-Key`, minted once per gesture and sent again on its retry,
* never an `op_id` argument, which the route refuses.
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
*    read floor for that domain (`protocol/floors.ts`), or `null` for a
*    write that lands in no log a question reads;
*  - `refreshes` — questions OUTSIDE that domain's session set
*    (`contract/domains.ts`) that the write also moves, asked again without a
*    floor because they cannot take one;
*  - `scope` — `person` for a change somebody makes to the company's work in
*    their own name, `company` for one that rewrites what every seat's tracker
*    means (a project's settings, the catalogue), which a screen offers only
*    where it says so. It is the authority table's: a write decided by a
*    container's lead or by a deployment grant is `company`.
*
* HELD AGAINST THE REAL CATALOGUE by `internal/api/operator`'s
* `TestEveryActionTheDashboardTakesIsOneTheActTransportServes`: every tool is
* served by the act transport and is not a read, every argument is one its
* schema takes, every required one is present, and every scope is the one its
* authority class gives it. A renamed argument in Go is a red build here
* rather than a button the tools' own gate refuses `invalid_body` — an
* argument the tool does not declare — the first time somebody presses it.
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
			"personal"
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
* (`operator.ActTransportCodes` — the request guard's, the authority table's,
* the operation key's, a body, a drain) and the tool refusal classes
* (`mcp.Refusals` — what the tool itself refused, or a fault of the node it
* met). One union because a caller branching on `error` must never need to
* know which half a code came from, and the engine keeps them disjoint for
* that reason. A code missing here is a refusal rendered as "something went
* wrong"; one listed that nothing sends is a sentence nobody will ever read.
*
* NO SENTENCE MENTIONS A TOKEN. The dashboard holds no credential a script can
* read — the browser's session cookie is the whole of it — so a person is
* told to sign in again, never to set anything.
*
* THE SENTENCE IS SECOND PERSON, and it is ours — except where it is `null`,
* which means "show the tool's own `detail`". Those are the two classes whose
* sentence names the argument that was wrong (`invalid`) or the rule that
* forbade it (`forbidden`); every other class's sentence is written for a
* model reading a tool result, and a person reading it would be told about
* `if_match` and record ids.
*/
var ACT_ERRORS = {
	invalid_token: "Your sign-in has ended. Sign in again, then make the change.",
	identity_unavailable: "This node cannot confirm who you are just now. Try again in a moment; your sign-in is fine.",
	seat_unavailable: "The seat you act as is no longer in the org chart, so nothing can be made in its name.",
	second_factor_enrolment_required: "This deployment requires a second factor. Enrol one before you make changes.",
	csrf_origin: "The engine refused a change sent from another site. Make it from this page's own address.",
	unauthorized: "You do not hold what this change needs.",
	step_up_required: "This change needs you to confirm who you are first.",
	unknown_tool: "This engine does not make that change. It may be running a different version from this page — reload.",
	read_only_tool: "The dashboard sent a read as a change. Reload; if it persists, it is a bug.",
	unsupported_media_type: "The dashboard sent the change in a form the engine does not take.",
	op_id_invalid: "The dashboard sent the change without a usable operation key. Reload; if it persists, it is a bug.",
	invalid_input: "What this change was about moved since you opened it. Look again, then make the change.",
	invalid_body: "The dashboard sent a change the engine could not read.",
	body_too_large: "The change is larger than the engine accepts in one request.",
	unreadable_body: "The change could not be read in full by the engine.",
	no_active_revision: "This node has not been handed a company yet, so there is nothing to change. Try again once it has.",
	draining: "This node is shutting down and takes no changes now. Try again in a moment.",
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
	peer_upgrading: "The node that would make this change is mid-upgrade. Try again in a moment.",
	internal_error: "The change failed inside the engine, and trying again will not fix it. Its log has the reason; nothing was refused on purpose."
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
//#region src/protocol/floors.ts
/**
* This tab's read floors: the newest position each domain has been written at
* from here, which every later read of that domain waits for.
*
* NOT `session.ts`, which is the browser's SIGN-IN session — the signed cookie
* and what it lacks. "Session" here is the engine's READ LEVEL
* (`read_level=session`): a read that waits for what this tab wrote. The two
* share a word and nothing else, so they share no module.
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
* own; the dashboard uses [tabFloors], the one per tab.
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
/** This tab's floors: every write through `act.ts` raises them, and every read
*  of a question that takes a floor names them. */
var tabFloors = new SessionFloors();
//#endregion
//#region src/protocol/act.ts
/**
* The dashboard's one write: a change, made as the principal this browser's
* session resolves to (ADR-0024).
*
* EVERY BUTTON THAT CHANGES THE COMPANY COMES THROUGH HERE, and here goes to
* exactly one route — `POST /operator/act/{tool}` — where the engine runs the
* same operator tool a person's own assistant would, decided by the one
* authority table and attributed the way every surface attributes a write: a
* person the identity directory binds to a seat writes as that seat, and a
* credential nobody is bound through writes under its own login. There is no
* second write path to keep in step with it: `app/source.test.ts` holds every
* `act(` to a literal tool, `protocol/transport.test.ts` holds every network
* read to `src/protocol/`, and `contract/actions.ts` is the whole vocabulary,
* held against the engine's catalogue by a Go gate.
*
* THE CREDENTIAL IS THE SESSION COOKIE, as for every request `rest.ts` makes:
* this module holds nothing a script could read. A `401` is noted there as a
* session that needs a sign-in, and a `403 step_up_required` is confirmed and
* the SAME request replayed there — so a press refused for a stale proof is
* made once the person has confirmed who they are, under the same operation.
*
* # What an answer can be, and what each tells a person
*
*  - `applied` — the record landed at `position`, and this tab's read floor
*    for its domain rose to it (`protocol/floors.ts`), so every read after
*    this one includes it;
*  - `pending` — the engine accepted it and this node has not applied it
*    yet: the floor rises, and the reads wait for it;
*  - `unknown` — nobody can say whether it landed: the connection dropped
*    after the request left, a gateway gave up, or the engine itself said so.
*    NEVER RETRIED HERE. A retry is the person's decision, and when they make
*    it the same `opId` goes again, which the engine derives the same
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
/** Whether a code off the wire is one this build knows. */
function isActErrorCode(code) {
	return Object.hasOwn(ACT_ERRORS, code);
}
/**
* A fresh operation key: a UUIDv7, stamped with the instant the gesture began.
*
* VERSION 7 BECAUSE THE ENGINE READS THE INSTANT. The key travels in the
* `Idempotency-Key` header, which the act route REQUIRES and holds to the
* engine's operation-id grammar (`statelog.CheckCallerOpID`); the operation's
* mint instant is the key's own, and it is what the engine uses to decide
* whether its operation ledger can still vouch for a retry. A key carrying no
* instant is refused `op_id_invalid`. The layout is the engine's operation-id
* grammar with no name ([layoutOpID]), so there is one encoder rather than
* two. The engine then scopes the key by the principal that sent it, so a key
* somebody copied names an operation of theirs, never this person's.
*
* `crypto.getRandomValues`, never `crypto.randomUUID`: the second exists only
* in a secure context, and a node's dashboard reached at
* `http://10.0.0.4:8000` is not one — every write there would throw.
*/
function newActOpID(now = Date.now()) {
	return layoutOpID(now, crypto.getRandomValues(/* @__PURE__ */ new Uint8Array(10)), "");
}
/**
* Make one change, as the signed-in principal.
*
* The body is `{args}` as JSON — the engine refuses any other content type,
* which is what keeps a cross-site form from reaching a write — and the
* operation rides the `Idempotency-Key` header, where every surface that
* takes one reads it.
*/
async function act(tool, args, options = {}) {
	const opId = options.opId ?? newActOpID();
	const floors = options.floors ?? tabFloors;
	const unknown = (reason) => ({
		kind: "unknown",
		tool,
		opId,
		reason
	});
	let body;
	try {
		({body} = await rest.request("POST", `/operator/act/${encodeURIComponent(tool)}`, {
			body: { args },
			contentType: "application/json",
			headers: { "Idempotency-Key": opId },
			signal: options.signal
		}));
	} catch (err) {
		if (isAbort(err)) throw err;
		if (!(err instanceof RestError)) return unknown(String(err));
		return refusalOf(tool, opId, err, floors);
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
		opId,
		position,
		domain,
		receipt: answer.receipt
	};
}
/**
* The refusals that say the screen the press was made from is OUT OF DATE:
* somebody else changed the object since it was drawn (`stale_version`,
* `conflict`), removed it (`not_found`), made it already (`exists`), answered
* it (`already_answered`), or moved what the request named so its operation
* already landed on something else (`invalid_input`). Each asks again every
* question the write would have moved — without raising a floor, since
* nothing of this tab's landed — so the page redraws what IS there and the
* next press is made against it. Without that a task page kept sending the
* version it was drawn at and was refused `stale_version` on every press
* until its own poll happened to come round.
*/
var LOOK_AGAIN = /* @__PURE__ */ new Set([
	"stale_version",
	"conflict",
	"not_found",
	"exists",
	"already_answered",
	"invalid_input"
]);
/** What a refusal, or a request that never got an answer, came to. */
function refusalOf(tool, opId, err, floors) {
	if (err.body.outcome === "unknown") return {
		kind: "unknown",
		tool,
		opId,
		reason: err.detail || err.code
	};
	const code = isActErrorCode(err.code) ? err.code : "";
	if (err.unanswered || err.status >= 500 && code === "") return {
		kind: "unknown",
		tool,
		opId,
		reason: err.detail || `The answer was lost (status ${err.status}).`
	};
	if (LOOK_AGAIN.has(code)) {
		const { domain, refreshes } = ROWS[tool];
		floors.written(domain, null, refreshes);
	}
	return {
		kind: "refused",
		tool,
		opId,
		code,
		sentence: (code === "" ? null : ACT_ERRORS[code]) ?? (err.detail || err.sentence || `The engine refused the change (status ${err.status}).`),
		hint: err.hint,
		grants: err.grants,
		retryable: err.retryHint !== null && retryAfterMs(err.retryHint) !== null
	};
}
//#endregion
export { GATE_REQUEST_TIMEOUT_MS, LiveSocket, MAX_EVENTS, MAX_PHASES, QueryRefusedError, REQUEST_TIMEOUT_MS, RETRY_AFTER_MAX_MS, RestError, SessionFloors, Store, UNANSWERED_RETRY_BASE_MS, UNANSWERED_RETRY_MAX_MS, UNAVAILABLE_RETRY_MS, act, api, auth, confirmStepUp, currentSessionNeed, domainOf, isAbort, isLogRefusal, keepsOperation, layoutOpID, needSession, newActOpID, newGateOpID, onSessionNeed, queryErrorCode, queryFailure, refusedGrants, rest, restFailure, restRetryMs, retryAfterMs, retryAfterSeconds, retryHintOf, sessionNeedsEnrolment, sessionRestored, setStepUpConfirmer, share, tabFloors, unansweredRetryMs, unavailableRetryMs };
