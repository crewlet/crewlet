//#region src/protocol/equal.ts
/**
* Whether two values decoded off the wire say the same thing: the same JSON,
* compared structurally rather than by identity.
*
* WHY IT EXISTS: every live push decodes into new objects, so a row the engine
* sent again unchanged (a seat's state re-sent beside another seat's change,
* the same refusal reported once a second) was a new row to every screen
* keyed on identity, which drew it again. The store keeps the row it holds
* when the push says nothing new, and a screen deriving rows from it keeps
* the rows it drew, so nothing a person sees moves unless something did.
*
* JSON's own values only: objects, arrays, strings, numbers, booleans and
* null. A key whose value is `undefined` reads as a missing key, as
* `JSON.stringify` would write it.
*/
function sameValue(a, b) {
	if (a === b) return true;
	if (a === null || b === null || typeof a !== "object" || typeof b !== "object") return false;
	if (Array.isArray(a) !== Array.isArray(b)) return false;
	if (Array.isArray(a) && Array.isArray(b)) return a.length === b.length && a.every((v, i) => sameValue(v, b[i]));
	const left = a;
	const right = b;
	const keys = (o) => Object.keys(o).filter((k) => o[k] !== void 0);
	const lk = keys(left);
	if (lk.length !== keys(right).length) return false;
	return lk.every((k) => Object.hasOwn(right, k) && sameValue(left[k], right[k]));
}
//#endregion
//#region src/contract/wire.ts
/**
* A live call's HEAVY fields, by the version that numbers each — the engine's
* `livestate.CallDetail`, held there both ways by `internal/api/livestate`'s
* `TestTheDashboardMergesEveryVersionedField`.
*
* An `agents` push carries these only when their version moved since the last
* push for the same call — they move once a round, once a tool call or once a
* CLI run, while the row is pushed five times a second as a round streams —
* and names every version always. The store keeps the copy it holds of a field the push left
* out, and asks for the call whole (`live_call`) when a push names a version
* newer than the one it holds: the push that moved it was dropped. A field
* named here that the engine does not version is one a push never leaves out
* and the merge waits on for ever; one the engine versions that is not named
* here is a field the merge drops from the screen.
*/
var LIVE_CALL_DETAIL = {
	prompt: ["prompt", "prompt_messages"],
	response: ["response"],
	narration: ["round_narration"],
	executions: ["tool_executions"],
	rounds: ["rounds"],
	invocations: ["invocations"]
};
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
		"decisions",
		"answered_decisions"
	],
	pages: [
		"pages",
		"page",
		"containers",
		"page_activity",
		"page_revision"
	],
	chat: [
		"chat_channels",
		"chat_channel",
		"chat_messages",
		"chat_thread",
		"chat_mentions",
		"chat_search",
		"chat_room_names"
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
* THE ENGINE'S OWN GRAMMAR (`worklet.ParseLogPosition`), so the value handed
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
* Two positions on ONE log in log order: negative when `a` is earlier, zero
* when they are the same point, positive when `a` is later.
*
* GENERATION FIRST, for [isLater]'s reason, then the sequence — and nothing
* else, because the log's own order is exact and clock-free where an instant
* is neither: the broker's RFC 3339 instant trims trailing zeros, so as text
* `…:05.12Z` sorts after `…:05.123456789Z` and `…:05Z` after every fraction
* of its own second. For a list ordered by what happened most recently on a
* log — a tab's live chat rooms, the rooms it has seen — this is the order.
* Two positions on DIFFERENT streams have no order between them, and a caller
* comparing across logs has a bug this cannot repair.
*
* NO POSITION IS EARLIEST OF ALL: a frame from a build that sent none is the
* one nothing can say is recent, so it sorts before every position rather
* than throwing or comparing as zero by accident.
*/
function comparePositions(a, b) {
	if (!a || !b) return (a ? 1 : 0) - (b ? 1 : 0);
	if (a.generation !== b.generation) return a.generation - b.generation;
	return a.seq - b.seq;
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
		return domain !== null && domainOf(kind) === domain || refreshes.includes(kind) || refreshes.includes("*");
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
/** What a heavy field reads as while nothing of it is held. */
var NOTHING_HELD = {
	prompt: "",
	prompt_messages: null,
	response: "",
	round_narration: null,
	tool_executions: null,
	rounds: null,
	invocations: null
};
/**
* The heavy fields that are STATUS — each round's timing and tokens, and each
* CLI run's — which a call whose body is withheld from this reader still
* carries and versions. Every other detail is the seat's BODY
* (`LiveCall.body_withheld`).
*/
var STATUS_DETAIL = /* @__PURE__ */ new Set(["rounds", "invocations"]);
/**
* One seat's live call as a push states it, merged onto the copy a tab holds —
* and whether the push says the tab missed a change it no longer carries.
*
* THE PUSH LEAVES OUT WHAT DID NOT MOVE. A call's heavy fields
* (`LIVE_CALL_DETAIL`) travel only on the push that moved their version, and
* every push names every version. So a field the push carries is taken — unless
* its version is older than the one held, a push overtaken by a snapshot or an
* answer — and one it leaves out is the copy held, at the version held, for the
* SAME call (`turn_id`, `phase`, `iteration`); a call of its own holds nothing
* yet. A push naming a version newer than the one held, without the field, is
* a tab that missed the push which carried it: `stale`, and the field stays
* what is held until the call is fetched whole.
*
* A CALL WHOSE BODY IS WITHHELD (`body_withheld`) names every body version
* zero and carries no body field, so it is never `stale` for one — a status
* reader is never sent a body, and asking for the call whole would only fetch
* the same status again — and no body the tab held is kept: a reader who could
* read this seat's trail a moment ago and cannot now is shown none of it.
*/
function mergeLiveCall(held, next) {
	if (!next) return {
		call: null,
		stale: false
	};
	const same = !!held && sameCall(held, next);
	const withheld = next.body_withheld === true;
	const merged = { ...next };
	const versions = {};
	let stale = false;
	for (const [detail, fields] of Object.entries(LIVE_CALL_DETAIL)) {
		if (withheld && !STATUS_DETAIL.has(detail)) {
			for (const f of fields) delete merged[f];
			versions[detail] = 0;
			continue;
		}
		const pushed = next.versions[detail];
		const have = same ? held.versions[detail] : 0;
		const carried = fields.every((f) => f in next);
		if (carried && pushed >= have) {
			versions[detail] = pushed;
			continue;
		}
		for (const f of fields) merged[f] = same ? held[f] : NOTHING_HELD[f];
		versions[detail] = have;
		if (!carried && pushed > have) stale = true;
	}
	merged.versions = versions;
	return {
		call: merged,
		stale
	};
}
/** Whether two copies are of one call: one turn, one phase, one iteration. */
function sameCall(a, b) {
	return a.turn_id === b.turn_id && a.phase === b.phase && a.iteration === b.iteration;
}
/**
* The heavy fields of `older` — a copy of the call `held` is, read BEFORE the
* pushes that brought `held` — whose versions are newer than `held`'s, laid onto
* `held`; or null when it brings none, or is not a copy of the same call.
*
* A version is never handed out twice and only grows, so a newer one is newer
* content whenever it was read: the pushes after `older` left those fields out
* because the tab was meant to hold them already, and their copy is what the
* tab missed. Everything else — the light fields, and every heavy field the tab
* holds at a version as new — stays `held`'s, which the later pushes wrote.
*/
function newerDetail(held, older) {
	if (!sameCall(held, older)) return null;
	let merged = null;
	const versions = { ...held.versions };
	for (const [detail, fields] of Object.entries(LIVE_CALL_DETAIL)) {
		if (held.body_withheld === true && !STATUS_DETAIL.has(detail)) continue;
		const version = older.versions[detail];
		if (version <= held.versions[detail] || !fields.every((f) => f in older)) continue;
		merged ??= { ...held };
		for (const f of fields) merged[f] = older[f];
		versions[detail] = version;
	}
	if (!merged) return null;
	merged.versions = versions;
	return merged;
}
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
		health: null,
		lastHealth: null,
		tokens: null,
		budget: null,
		schedules: null,
		connected: false,
		lostAt: null,
		everConnected: false,
		authRejected: false,
		accessRefused: null,
		inboxMoves: {},
		chat: {
			rooms: {},
			frames: 0,
			dropped: 0,
			gap: 0
		},
		chatPresence: { rooms: {} },
		chatRead: null
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
		if (snap.health) this.state.health = this.state.lastHealth = snap.health;
		this.emit(...ALL_DATA_SLICES);
	}
	/**
	* Changed seat overlays, merged onto the roster rows by AGENT ID — and the
	* agent ids whose live call this tab now holds behind what the push
	* describes, which the socket fetches whole (`mergeLiveCall`).
	*
	* By the id and nothing else. They were merged by ROLE NAME, which is prose:
	* two seats sharing a name both took every overlay either of them moved, so
	* each card rendered whatever the other was last doing. The id is what the
	* engine sends on an overlay, and it names exactly one seat.
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
		if (!Array.isArray(rows) || rows.length === 0) return [];
		const byID = /* @__PURE__ */ new Map();
		for (const row of rows) if (row && typeof row.agent_id === "string" && row.agent_id !== "") byID.set(row.agent_id, row);
		if (byID.size === 0) return [];
		const stale = [];
		const merge = (held, patch) => {
			if (!("live_call" in patch)) return patch;
			const heldSeq = held.live_call_seq ?? 0;
			const patchSeq = patch.live_call_seq;
			if (patchSeq !== void 0 && patchSeq < heldSeq) return {
				...patch,
				live_call: held.live_call ?? null,
				live_call_seq: heldSeq
			};
			const { call, stale: behind } = mergeLiveCall(held.live_call, patch.live_call);
			if (behind) stale.push(patch.agent_id);
			return {
				...patch,
				live_call: call,
				live_call_seq: patchSeq ?? heldSeq
			};
		};
		let moved = false;
		const next = this.state.agents.map((a) => {
			const patch = byID.get(a.agent_id);
			if (!patch) return a;
			const merged = {
				...a,
				...merge(a, patch)
			};
			if (sameValue(a, merged)) return a;
			moved = true;
			return merged;
		});
		if (moved) {
			this.state.agents = next;
			this.emit("agents");
		}
		return stale;
	}
	/**
	* One seat's live call, fetched WHOLE because a push said this tab had
	* missed a change to it — and whether the answer was CURRENT: at or past the
	* sequence the seat has applied, so that nothing a push has said since is
	* missing from it.
	*
	* ORDERED BY `live_call_seq`, because this answer runs on its own goroutine
	* and the socket can deliver it AFTER pushes the engine generated later. A
	* current answer is merged like a push that carries everything. One BEHIND
	* the applied sequence describes the slot as it was before those pushes, and
	* the sequence decides only what it can order:
	*
	* - when they cleared the call or began another, the answer is DROPPED, so a
	*   cleared seat or a newer call is never overwritten by a running call read
	*   a moment before it — a per-field version cannot say this, since a cleared
	*   null carries none and two calls' fields are never compared;
	* - when they are the same call, the answer still brings every heavy field
	*   whose version is newer than the copy held (`newerDetail`), because those
	*   are exactly what the tab asked for — the pushes since left them out —
	*   while the light fields stay the newer pushes'. Dropping it whole lost
	*   that repair, and nothing asked again until the seat's next push.
	*
	* A behind answer is the caller's cue to ask again if a push said the call
	* was behind while this answer was out (`LiveSocket.fetchCalls`).
	*/
	applyLiveCall(answer) {
		if (!answer || typeof answer.agent_id !== "string" || answer.agent_id === "") return true;
		let moved = false;
		let current = true;
		this.state.agents = this.state.agents.map((a) => {
			if (a.agent_id !== answer.agent_id) return a;
			const heldSeq = a.live_call_seq ?? 0;
			const seq = answer.live_call_seq;
			if (seq >= heldSeq) {
				moved = true;
				return {
					...a,
					live_call: mergeLiveCall(a.live_call, answer.live_call).call,
					live_call_seq: seq
				};
			}
			current = false;
			const fresher = a.live_call && answer.live_call && newerDetail(a.live_call, answer.live_call);
			if (!fresher) return a;
			moved = true;
			return {
				...a,
				live_call: fresher
			};
		});
		if (moved) this.emit("agents");
		return current;
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
		this.state.health = health ?? null;
		if (health) this.state.lastHealth = health;
		this.link(!!health);
		this.emit("health");
	}
	setConnected(value) {
		this.link(value);
		if (!value) this.state.health = null;
		this.emit("health");
	}
	/**
	* The transport's state, and the two facts about its history the frame
	* reads (`lostAt`, `everConnected`), written in ONE place so a health frame
	* and the socket's own open and close cannot disagree about them.
	*/
	link(connected) {
		this.state.connected = connected;
		if (connected) {
			this.state.everConnected = true;
			this.state.lostAt = null;
		} else if (this.state.lostAt === null) this.state.lostAt = Date.now();
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
	/**
	* One committed chat record, as the socket announced it.
	*
	* ONE METHOD FOR SIX PUSH KINDS, because the frame already says which it is:
	* `op` is the record's own word (`post`, `edit`, `react`, …) and the kind is
	* a routing label the server derived FROM it. Taking the kind as well would
	* be a second vocabulary for one fact, and the two would disagree on exactly
	* the op somebody adds next.
	*
	* IT DERIVES NOTHING ABOUT WHAT A ROOM CONTAINS. A frame carries no body, so
	* a transcript rendered from these would be a second copy of the
	* conversation — the failure this store's own doc opens with. What is kept
	* is the high-water mark a screen compares against what it has rendered, the
	* count a screen re-reads on ([ChatRoomLive.frame], [ChatLive.frames]), and
	* the frames themselves, so a screen can tell a new message from a reaction
	* without asking the engine which it was.
	*/
	applyChatChange(change) {
		if (!change) return;
		if (!change.channel_id) {
			this.applyChatGap();
			return;
		}
		if (!change.op_id) return;
		const live = this.state.chat;
		const rooms = live.rooms;
		const room = rooms[change.channel_id] ?? {
			seq: 0,
			position: null,
			frame: 0,
			changes: []
		};
		if (room.changes.some((seen) => seen.op_id === change.op_id)) return;
		const seq = typeof change.channel_seq === "number" ? change.channel_seq : 0;
		live.frames += 1;
		const changes = [{
			...change,
			frame: live.frames
		}, ...room.changes];
		for (const gone of changes.slice(200)) live.dropped = Math.max(live.dropped, gone.frame);
		rooms[change.channel_id] = {
			seq: seq > room.seq ? seq : room.seq,
			position: comparePositions(change.position ?? null, room.position) > 0 ? change.position ?? null : room.position,
			frame: live.frames,
			changes: changes.slice(0, 200)
		};
		this.evictChatRooms();
		this.emit("chat");
	}
	/**
	* The engine could not say what this tab missed: read every room again.
	*
	* COUNTED AS A FRAME, so the screens that span the rooms move on it, and
	* LETTING GO OF EVERY FRAME BEFORE IT, so a screen asking what arrived since
	* the frame it last answered is told it cannot say — which is the truth,
	* and what makes it read everything it holds again. What is held per room
	* stays: it is what DID arrive, and a room's position still orders the
	* eviction below.
	*/
	applyChatGap() {
		const live = this.state.chat;
		live.frames += 1;
		live.dropped = live.frames;
		live.gap = live.frames;
		this.emit("chat");
	}
	/**
	* Keep the live state to [MAX_CHAT_ROOMS] rooms, dropping the one whose
	* newest frame is oldest.
	*
	* By where the room's newest frame sits on the log rather than by insertion
	* order: a room that has been quiet for a week is the one nobody is looking
	* at, whatever order this tab happened to hear about it in. A room no frame
	* gave a position goes first, being the one nothing can say is recent.
	*/
	evictChatRooms() {
		const live = this.state.chat;
		const rooms = live.rooms;
		const ids = Object.keys(rooms);
		if (ids.length <= 256) return;
		const oldest = ids.reduce((worst, id) => comparePositions(rooms[id]?.position ?? null, rooms[worst]?.position ?? null) < 0 ? id : worst);
		live.dropped = Math.max(live.dropped, rooms[oldest]?.frame ?? 0);
		delete rooms[oldest];
	}
	/** Who is in the rooms this socket is watching, whole. */
	applyChatPresence(frame) {
		this.state.chatPresence = frame?.rooms ? frame : { rooms: {} };
		this.emit("chatPresence");
	}
	/**
	* The read state of the seat this tab reads chat as, from its own flush or
	* from another tab's.
	*
	* IT IS NEVER MERGED WITH WHAT IS ON SCREEN. The record is authoritative and
	* whole — the engine advances cursors and ignores a value below what it
	* holds — so the answer to a flush and the push another tab caused are the
	* same fact about different moments of one record.
	*
	* AND THE NEWER MOMENT WINS, BY THE RECORD'S REVISION, never by arrival:
	* two flushes are taken in one order and their pushes can arrive in the
	* other, so a frame below the revision held is late and is dropped — kept,
	* it would put back a cursor or a mute the record has already moved past.
	* `updated_at` cannot order them, being each flush's own instant from
	* whichever node took it.
	*
	* BUT ONLY WITHIN ONE SEAT. The revision orders two states of one seat's
	* record — on the coordination KV it is the bucket's own sequence, shared
	* by everybody's — so a state NAMING ANOTHER SEAT is not older or newer
	* than the one held: it is the seat this tab now reads as, after the one
	* its session was bound to moved (the engine closes the socket then, and
	* the tab dials again as the seat it holds now). Compared, it was dropped
	* whenever the last seat's latest flush was newer than this one's record,
	* and the rail answered for the new seat was thrown away until it wrote
	* again. It replaces the held state, and everything else this tab held
	* about the last seat goes with it ([forgetSeat]).
	*/
	applyChatRead(state) {
		if (!state) return;
		const held = this.state.chatRead;
		if (held && held.handle !== state.handle) this.forgetSeat();
		else if (held && (state.revision ?? 0) < (held.revision ?? 0)) return;
		this.state.chatRead = state;
		this.emit("chatRead");
	}
	/**
	* Forget everything this tab holds about the SEAT it was reading chat as:
	* the frames of the rooms it could read, who was in the rooms it had open,
	* and its read state.
	*
	* ALL THREE ARE ONE SEAT'S. A frame reaches a socket only for a room its
	* seat may read and carries that room's name and an excerpt of what was
	* said, presence is for the rooms it had open, and a read state is its own
	* by its key — so a tab now reading as another seat, kept any of them,
	* would hold the last seat's private rooms under the next one's name, and
	* offer the next a Mute that undid the last one's. Called by
	* [applyChatRead] when a state names another seat, and by
	* [setAccessRefused], which drops everything the engine served.
	*/
	forgetSeat() {
		this.state.chat = this.forgottenChat();
		this.state.chatPresence = { rooms: {} };
		this.state.chatRead = null;
		this.emit("chat");
		this.emit("chatPresence");
		this.emit("chatRead");
	}
	/**
	* The live chat state with every room let go of.
	*
	* THE COUNT STAYS: it is nobody's ([ChatLive.frames]), and a count that went
	* back to zero could come back to a number a screen holds — which would
	* hide every frame from that screen until the count passed it again. Every
	* frame it counted is let go of, so a screen asking what arrived since one
	* it answered is told it cannot say ([ChatLive.dropped]).
	*/
	forgottenChat() {
		const { frames, gap } = this.state.chat;
		return {
			rooms: {},
			frames,
			dropped: frames,
			gap
		};
	}
	/**
	* The engine knows this browser and will not serve it the company, or null.
	*
	* A REFUSAL DROPS WHAT THE ENGINE SERVED. It is a decision about who may
	* read the company, so what an earlier socket sent stops being shown the
	* moment it lands: a tab whose seat was taken out of the chart went on
	* naming the company, listing its agents and tools in the palette, from the
	* snapshot it held — while a session refused at its first handshake, sent
	* nothing, named and listed none of it. An outage keeps the last state on purpose ("showing the
	* last state received"); a refusal is not an outage. `health` stays, and
	* `lastHealth` with it: it is the unguarded `/health`, not the company.
	*/
	setAccessRefused(reason) {
		const next = reason === null ? null : reason.trim();
		if (this.state.accessRefused === next) return;
		this.state.accessRefused = next;
		if (next !== null) {
			const { health, lastHealth, connected, lostAt, everConnected, authRejected } = this.state;
			const chat = this.forgottenChat();
			Object.assign(this.state, emptyState(), {
				health,
				lastHealth,
				connected,
				lostAt,
				everConnected,
				authRejected,
				accessRefused: next,
				chat
			});
			this.emit(...ALL_DATA_SLICES, "phases", "inboxMoves", "chat", "chatPresence", "chatRead");
		}
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
};
//#endregion
//#region src/contract/retry.ts
/**
* The two waits the dashboard's retries are bounded by that the ENGINE owns:
* what it says when it has nothing better, and the longest hint it fixes.
*
* Each is a COPY of an engine value, held to it by `internal/api`'s
* `TestTheDashboardRetriesOnTheEnginesOwnHints`. What is done with them — the
* arithmetic over a hint — is behaviour, and lives in `protocol/retry.ts`.
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
//#region src/contract/identity.ts
/**
* The header an invitation's secret travels in to its view:
* `authapi.InviteSecretHeader`, held by
* `internal/api/authapi.TestTheDashboardSendsALinksSecretInTheHeaderItsViewReads`.
*
* A HEADER, because the view is a GET and a GET has no body — and never the
* query string, which every access log between the browser and the engine
* records. The link carries the secret in its FRAGMENT precisely so that no
* server sees it on the way to this page. Spelled differently here, the screen
* would present no secret and every invitation it opened would be the one
* `410` an id nobody issued answers.
*/
var INVITE_SECRET_HEADER = "X-Crewlet-Invite-Secret";
/**
* The header a password reset link's secret travels in to its view, for
* `INVITE_SECRET_HEADER`'s reason: `authapi.ResetSecretHeader`, held by the
* same gate.
*/
var RESET_SECRET_HEADER = "X-Crewlet-Reset-Secret";
/**
* The header every request a tab makes FOR ITS READER carries that reader in —
* the principal id the tab recorded (`lib/reader.ts`) — beside the session
* cookie: `auth.ReaderHeader`, held by
* `internal/api/auth.TestTheDashboardNamesItsReaderInTheGuardsHeader`.
*
* The engine serves such a request only to that principal and refuses anybody
* else `409 reader_changed` before it runs, so a window left open on the last
* person's screen after somebody signed in as somebody else in another tab
* makes no change as the new person, and is handed to them
* (`protocol/signin.ts`). Spelled differently here, every request would name
* nobody, be served to whoever the cookie is, and nothing would go red.
*/
var READER_HEADER = "X-Crewlet-Reader";
/**
* The query parameter the socket's HANDSHAKE names the tab's reader in:
* `auth.ReaderParam`, held by
* `internal/api/auth.TestTheDashboardsSocketNamesItsReaderInTheGuardsParameter`.
*
* A browser sets no header on a WebSocket, so the handshake is the one request
* that cannot carry `READER_HEADER`. The engine reads this parameter on an
* upgrade that carries no header, and refuses a dial made for anybody but the
* cookie's principal `409 reader_changed` before the stream is served — which
* the browser reports as 1006, and the socket's refusal probe reads and hands
* the tab over on (`protocol/socket.ts`). The id is not a secret, but it is in
* the socket's URL, so a proxy's access log of that path records it. Spelled
* differently here, every handshake would name nobody and be served to
* whoever the cookie is, until the tab's next request was refused.
*/
var READER_PARAM = "reader";
//#endregion
//#region src/protocol/signin.ts
/**
* What this browser's sign-in needs from the person, as the transports learn
* it.
*
* # Why this is a module and not a prop
*
* The code that DISCOVERS a session problem is the transport: a REST answer
* of `401`, a socket handshake the refusal probe reads as one, a `403` that
* says the session may only enrol a second factor. The code that REPAIRS it is
* the application: a route to the sign-in screen, or to the screen that
* enrols the factor. Neither may reach into the other — a transport that draws a
* dialog cannot be tested without a browser, and a screen that inspects every
* status itself is a screen that forgets one — so the fact travels here,
* beside neither.
*
* Not `session.ts`, which is the READ FLOORS a tab's own writes raise: a
* sign-in session and a read session are two different things, and one module
* name for both was a reader opening the wrong file.
*
* # A need is a STATE, not an event
*
* The socket is started before React mounts, so its refusal probe can answer
* before anything has subscribed to hear it. An event fired into nobody is a
* browser that never learns it is signed out, so what is kept is the latest
* need, read by whoever subscribes when they subscribe. It is cleared by the
* one thing that answers it: a sign-in that ends holding a whole session.
*
* # And a step-up is a QUESTION, asked once however many requests need it
*
* A guarded write refused `403 step_up_required` is not a mistake the person
* made: they are signed in, and the engine wants a fresher proof of who they
* are before this particular gesture. The repair is to ask for the password
* again and send the SAME request — so nothing typed into a form is lost —
* and it is asked once however many requests were refused at the same
* moment: a screen that saves three things together must not stack three
* password dialogs. The transport asks here; the application answers.
*
* # And every request says who the tab is read by
*
* The tabs of one browser share ONE session cookie and each draws one person's
* company, so a sign-in as somebody else in another tab moves the cookie under
* every other tab. What hands those tabs over — the engine closing the
* sockets of the session it ended, `4401` — rides a close the sign-in makes
* best effort, and a window left open beside it may hear nothing: it would go
* on drawing the last person's screen while every request it sent went out as
* the new one, a retried change of theirs included. So every request a tab
* makes FOR ITS READER names them ([readerHeaders], `READER_HEADER`), and the
* engine refuses one whose credential resolves to anybody else `409
* reader_changed` before it runs. The transport reads the reader here and
* reports that refusal here ([readerMoved]); the application, which records
* who the tab is read by (`lib/reader.ts`) and knows how a tab is handed over
* (`lib/session.ts`), answers both. With nothing installed — the e2e replay,
* a suite that installs none — a request names nobody and is served as the
* cookie is.
*/
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
var asking = /* @__PURE__ */ new Set();
/**
* Whether a step-up is being asked of the person right now — what a dialog
* whose write is held behind it says it is waiting for: the person, not the
* engine.
*/
function stepUpAsked() {
	return confirming !== null;
}
/** Subscribe to a step-up starting or ending. Returns the unsubscribe. */
function onStepUpAsked(listener) {
	asking.add(listener);
	return () => {
		asking.delete(listener);
	};
}
function toldAsking() {
	for (const listener of asking) listener();
}
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
*/
function confirmStepUp() {
	if (!confirmer) return Promise.resolve(false);
	if (!confirming) {
		confirming = confirmer().catch(() => false).finally(() => {
			confirming = null;
			toldAsking();
		});
		toldAsking();
	}
	return confirming;
}
var tabReader = null;
/**
* Install who the tab is read by. Returns the uninstall, which leaves a later
* installation in place — the step-up confirmer's rule, for its reason: one
* tab, one reader.
*/
function setTabReader(reader) {
	tabReader = reader;
	return () => {
		if (tabReader === reader) tabReader = null;
	};
}
/**
* The header naming this tab's reader, for a request made on their behalf —
* or none, where nothing is installed or the tab has learned nobody: a
* request that names nobody is served as the cookie is, which is all a tab
* that does not know its reader can ask for.
*/
function readerHeaders() {
	const reader = tabReader?.current() ?? null;
	return reader === null ? {} : { [READER_HEADER]: reader };
}
/**
* The socket's handshake URL query naming this tab's reader (`READER_PARAM`),
* or "" where nothing is installed or the tab has learned nobody.
*
* A browser sets no header on a WebSocket, so the handshake is the one request
* [readerHeaders] cannot reach; the engine reads this parameter on an upgrade
* instead, and refuses a dial made for anybody but the cookie's principal.
*/
function readerQuery() {
	const reader = tabReader?.current() ?? null;
	return reader === null ? "" : `?${READER_PARAM}=${encodeURIComponent(reader)}`;
}
/** A request named this tab's reader and the engine said the session is somebody else's. */
function readerMoved() {
	tabReader?.moved();
}
//#endregion
//#region src/contract/remedies.ts
/**
* The configurable limits a refusal names as where to change it, as the engine
* names them (`internal/caveat`'s `Setting`, ADR-0060).
*
* A refusal a limit caused (a token budget spent) carries a `remedy` naming
* one of these, and `lib/remedy.ts` resolves each to the screen and the section
* that edit it. HELD AGAINST `caveat.Settings` by
* `internal/caveat.TestTheDashboardKnowsEverySettingARemedyNames`: a setting
* added there and not here would be a refusal with nothing to open.
*/
var LIMIT_SETTINGS = [
	"company_token_budget",
	"seat_token_budget",
	"assistant_token_budget",
	"model_providers"
];
//#endregion
//#region src/protocol/remedy.ts
/**
* A refusal's remedy, read off the wire: where a person changes the limit a
* refusal was caused by (ADR-0060). The engine sends it beside a budget
* refusal's words, on a question the assistant refused (`429
* budget_exhausted`), a turn's error frame, and an act's refusal.
*
* READ, NEVER TRUSTED AS TYPED: a setting this build does not know, or a seat
* remedy naming no seat, reads as no remedy at all, so a refusal from a newer
* engine draws its words and nothing to open rather than a link to nowhere.
*/
/** The remedy `raw` carries, or null where it carries none this build reads. */
function remedyOf(raw) {
	if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return null;
	const { setting, seat } = raw;
	const known = LIMIT_SETTINGS.find((s) => s === setting);
	if (known === void 0) return null;
	const handle = typeof seat === "string" ? seat.trim() : "";
	if (known === "seat_token_budget" !== (handle !== "")) return null;
	return handle === "" ? { setting: known } : {
		setting: known,
		seat: handle
	};
}
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
* unmounted screen and a server's `Retry-After` are each handled once. Writes
* never go over the socket, deliberately: a write is judged on its `Origin` by
* the engine's cross-site check before its handler runs, answers the three
* write outcomes as statuses with an op id to retry by, and may be refused for
* a step-up this module confirms and replays — none of which a frame on an
* open socket carries. And a handful of reads exist only as REST,
* `GET /secrets` above all, because no query in the registry answers them.
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
*
* AND EVERY REQUEST SAYS WHO IT IS FOR — the tab's reader, in the header
* `signin.ts` reads it into — unless it is one of the two whose point is to
* learn or change who the session is ([RequestOptions.forReader]). The cookie
* is the browser's and the screen is the tab's, and a request the engine
* would serve to somebody the tab was not drawn for is refused `409
* reader_changed` before it runs, which hands the tab over ([noteSession]).
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
	/** Everything else the body carried, for a caller that needs a field. */
	body;
	/**
	* The `Retry-After` the answer carried, in whole seconds — either form the
	* RFC allows ([retryAfterSeconds]) — or null where it named no wait.
	*
	* A `429` always carries one and says how long the sign-in curve makes the
	* next attempt wait. A `503` the engine wrote carries one where waiting can
	* clear the cause and NONE where it cannot (a node with no keyring, a full
	* log), which is the difference a loader retries on — see [retryHint],
	* which also refuses to read a proxy's header as the engine's.
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
		this.sentence = typeof body.message === "string" ? body.message : "";
		this.body = body;
		this.retryAfterSeconds = retryAfter;
	}
	/**
	* Whether the engine refused on AUTHORITY rather than on the request.
	*
	* Both statuses, because a screen locks the same way for either: 401 is
	* "nobody is signed in" and 403 is "the credential you presented does not
	* carry this". Only a 401 sends the reader to sign in (see [noteSession]): a
	* person holding a perfectly good session meets 403 the moment they open a
	* screen outside their grants, which is the ordinary case.
	*/
	get unauthorized() {
		return this.status === 401 || this.status === 403;
	}
	/** The grants a refusal on authority named — see [refusedGrants]. */
	get grants() {
		return refusedGrants(this.body);
	}
	/** Where the limit that caused the refusal is changed, or null ([remedyOf]). */
	get remedy() {
		return remedyOf(this.body.remedy);
	}
	/**
	* This refusal in the shape `QueryState` renders from — the one the
	* socket's error frame carries, under the same keys and on the same terms:
	* a `403 unauthorized`'s rule and the grants that would have admitted the
	* caller, and a 503's state-log code, its own words and whether waiting
	* changes it (no `Retry-After` is the engine saying it will not). Null for
	* anything else, a 401 included: nobody being signed in is not a rule's
	* refusal, and there are no grants to name to nobody.
	*
	* ONLY `unauthorized` IS A RULE'S REFUSAL. Every 403 used to be read as one,
	* with its code standing in for the rule, so a read refused
	* `seat_unavailable` or `csrf_origin` reached the banner as a refusal naming
	* no grant — which says "no grant would change that: the rule asks a
	* relation the org chart does not hold", a claim about neither.
	*/
	get refusal() {
		if (this.status === 403 && this.code === "unauthorized" && typeof this.body.reason === "string") return {
			reason: this.body.reason,
			grants: this.grants
		};
		const hint = this.retryHint;
		if (hint === null) return null;
		return {
			code: typeof this.body.refusal === "string" ? this.body.refusal : null,
			detail: this.detail || null,
			retryAfter: hint
		};
	}
	/**
	* When the ENGINE said to ask again, in whole seconds, or null where it said
	* nothing: a `503` it wrote says its `Retry-After`, and ZERO where it sent
	* none — its statement that waiting will not change the answer. Every other
	* answer, a `503` something in front of the engine wrote included, carries
	* no hint, because nobody at the engine decided one.
	*/
	get retryHint() {
		return this.status === 503 && !this.unanswered ? this.retryAfterSeconds ?? 0 : null;
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
	* reverse proxy whose read timeout is shorter than the fifty seconds the
	* engine takes to answer a node gate (`engine.GateAnswerBudget`) hands a slow
	* eviction to the browser as a 504 — and read as a refusal, its operation id
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
* goes stale the day the rule's grant moves.
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
* nothing but enrol the second factor the deployment requires. A `409
* reader_changed` is a session that is somebody else's than the tab that
* sent the request — whoever signed in in another tab — and the tab is handed
* to them. Every other refusal is about the REQUEST, and the session is fine.
*/
function noteSession(refusal) {
	if (refusal.status === 401 && !TYPED_REFUSALS.has(refusal.code)) needSession("sign_in");
	if (refusal.status === 403 && refusal.code === "second_factor_enrolment_required") needSession("second_factor");
	if (refusal.status === 409 && refusal.code === "reader_changed") readerMoved();
}
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
/** The seconds a response's `Retry-After` names, or null for none. */
function retryAfterOf(response) {
	return retryAfterSeconds(response.headers.get("Retry-After"), Date.now());
}
/**
* [RestError.retryHint] for a refused response that did not come through
* [request] — the degraded-mode snapshot (`api.ts`) and the socket's
* plain-HTTP re-ask of a refused handshake (`socket.ts`), each of which reads
* the response itself. Consumes the body.
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
* gate answers a gesture within `engine.GateAnswerBudget`, and a client waits
* the longer `GATE_REQUEST_TIMEOUT_MS` for it (`contract/gate.ts`), so thirty
* seconds gave up on a gesture the node went on to finish, holding nothing to
* finish it with.
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
* The route a step-up is given at. Asking to confirm the confirmation would
* be a dialog that reopens for ever, so a refusal there is the refusal.
*/
var STEP_UP_PATH = "/auth/step-up";
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
/** How many requests are out right now, and who is waiting for none to be. */
var inFlight = 0;
var settled = [];
/**
* Resolves once no request this module sent is still out — at once when none
* is.
*
* WHAT A SOCKET CLOSED 4401 WAITS FOR before it dials again. A step-up
* REPLACES the session it was made from, so the engine ends the old one and
* closes every socket it opened; the answer that sets the new cookie may
* still be on its way, and a dial before it lands carries the cookie the
* engine just ended and is refused — which would send a person who had just
* proved who they are to the sign-in form.
*/
function whenRequestsSettle() {
	if (inFlight === 0) return Promise.resolve();
	return new Promise((resolve) => settled.push(resolve));
}
/** The wall-clock instant, in ms, the engine last answered a request here. */
var answeredAt = 0;
/**
* When the engine last answered any request this module sent, in ms since
* the epoch, or 0 for never. Any answer to a request carrying the session
* cookie re-issues it once it is five minutes old, so a tab that has heard
* from the engine this recently needs no keepalive (`keepalive.ts`).
*/
function lastAnsweredAt() {
	return answeredAt;
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
* through whatever confirms a step-up (`signin.ts`), however many requests
* were refused together — and then REPLAYED: the same method, path, body and
* headers, so a form that was being saved is saved, rather than lost to a
* refusal its screen could only report. Once: a replay refused again is the
* refusal. Every screen gets this by sending its writes through here, and no
* screen implements it.
*
* The deadline is each ATTEMPT's, not the gesture's: the time a person spends
* typing their password is not the engine taking too long.
*/
async function request(method, path, options = {}) {
	try {
		return await counted(method, path, options);
	} catch (err) {
		if (!(err instanceof RestError && err.status === 403 && err.code === "step_up_required") || path.split("?")[0] === STEP_UP_PATH) throw err;
		if (!await unlessAborted(confirmStepUp(), options.signal)) throw err;
		return counted(method, path, options);
	}
}
/** One attempt, counted in flight for [whenRequestsSettle]. */
async function counted(method, path, options) {
	inFlight++;
	try {
		return await attempt(method, path, options);
	} finally {
		inFlight--;
		if (inFlight === 0) {
			const waiting = settled;
			settled = [];
			for (const resolve of waiting) resolve();
		}
	}
}
/** One round trip — see [request] for what surrounds it. */
async function attempt(method, path, options) {
	const { body, headers = {}, query, signal, read = "json", timeoutMs = REQUEST_TIMEOUT_MS, forReader = true } = options;
	const contentType = options.contentType ?? (body === void 0 ? void 0 : "application/json");
	let encoded;
	if (body !== void 0) {
		if (contentType && !isJson(contentType)) {
			if (typeof body !== "string" && !(body instanceof Blob)) throw new TypeError(`rest.request: a ${contentType} body must be a string or a Blob`);
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
			...headers,
			...forReader ? readerHeaders() : {}
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
	answeredAt = Date.now();
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
			detail: response.ok ? "the answer was cut short, or is not the engine's JSON" : `a ${response.status} came back without the engine's JSON, so something in front of it answered`
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
/**
* How long a download may take, from its request to its last byte.
*
* FIFTEEN MINUTES, not [REQUEST_TIMEOUT_MS]: a project's largest file is a
* gibibyte, and that is fourteen minutes at ten megabits a second. A deadline
* sized for a JSON answer would abandon every large download part way.
*/
var DOWNLOAD_TIMEOUT_MS = 9e5;
/**
* A file's bytes, fetched as a blob over the session cookie.
*
* NOT A LINK. A plain `<a href>` would carry the cookie too, but its answer is
* the browser's to handle: a refusal would be saved to disk as the file — the
* engine's JSON envelope under the file's own name — and a lost session would
* download a 401 rather than sending the reader to sign in. Fetched here, a
* refusal is the engine's JSON read like every other ([RestError], with its
* `Retry-After`), and a `401` reaches [noteSession] exactly as a request's
* does.
*
* NOT COUNTED IN FLIGHT ([whenRequestsSettle]): a download may run for
* [DOWNLOAD_TIMEOUT_MS], and a socket closed 4401 waits for the requests that
* could be carrying a new cookie — a GET of a file sets none.
*/
async function blob(path, signal) {
	if (signal?.aborted) throw signal.reason;
	const controller = new AbortController();
	let timedOut = false;
	const timer = setTimeout(() => {
		timedOut = true;
		controller.abort();
	}, DOWNLOAD_TIMEOUT_MS);
	const forward = () => controller.abort(signal?.reason);
	signal?.addEventListener("abort", forward, { once: true });
	const unanswered = (err) => {
		if (signal?.aborted) return signal.reason;
		return timedOut ? new RestError(0, {
			error: "unreachable",
			detail: `the download did not finish within ${waitWords(DOWNLOAD_TIMEOUT_MS)}`
		}) : offline(err);
	};
	try {
		let response;
		try {
			response = await fetch(location.origin + path, {
				method: "GET",
				cache: "no-store",
				credentials: "same-origin",
				headers: readerHeaders(),
				signal: controller.signal
			});
		} catch (err) {
			throw unanswered(err);
		}
		answeredAt = Date.now();
		if (!response.ok) {
			let body;
			try {
				body = await response.json();
			} catch {
				body = { error: "unreadable_body" };
			}
			const refusal = new RestError(response.status, body, retryAfterOf(response));
			noteSession(refusal);
			throw refusal;
		}
		try {
			return await response.blob();
		} catch (err) {
			throw unanswered(err);
		}
	} finally {
		clearTimeout(timer);
		signal?.removeEventListener("abort", forward);
	}
}
/**
* A long-lived text answer, read as it arrives: the Dashboard Assistant's
* turn stream (`assistant.ts`), whose bytes are server-sent events.
*
* THE SAME CREDENTIAL AND THE SAME REFUSALS as every request here: the
* session cookie and nothing else, the tab's reader named last
* ([readerHeaders], so a browser that changed hands is refused
* `reader_changed` and the tab handed over), no browser cache in front of it,
* and a
* refusal read as the engine's JSON ([RestError]), a `401` reaching
* [noteSession] exactly as a request's does. Only the STATUS LINE has a
* deadline ([REQUEST_TIMEOUT_MS]); the body runs for as long as the engine
* keeps it open, and the caller decides when a quiet stream has stalled,
* since only the caller knows how often the engine promised to speak.
*
* NOT COUNTED IN FLIGHT ([whenRequestsSettle]), for a download's reason: a
* stream may run for minutes, and it sets no cookie.
*/
async function stream(path, options = {}) {
	const { headers = {}, signal } = options;
	if (signal?.aborted) throw signal.reason;
	const controller = new AbortController();
	let timedOut = false;
	const timer = setTimeout(() => {
		timedOut = true;
		controller.abort();
	}, REQUEST_TIMEOUT_MS);
	const forward = () => controller.abort(signal?.reason);
	signal?.addEventListener("abort", forward, { once: true });
	const unanswered = (err) => {
		if (signal?.aborted) return signal.reason;
		return timedOut ? new RestError(0, {
			error: "unreachable",
			detail: `the engine did not answer within ${waitWords(REQUEST_TIMEOUT_MS)}`
		}) : offline(err);
	};
	const release = () => signal?.removeEventListener("abort", forward);
	let response;
	try {
		response = await fetch(location.origin + path, {
			method: "GET",
			cache: "no-store",
			credentials: "same-origin",
			headers: {
				Accept: "text/event-stream",
				...headers,
				...readerHeaders()
			},
			signal: controller.signal
		});
	} catch (err) {
		release();
		throw unanswered(err);
	} finally {
		clearTimeout(timer);
	}
	answeredAt = Date.now();
	if (response.status === 204) {
		release();
		return {
			next: async () => null,
			noContent: true
		};
	}
	if (!response.ok || response.body === null) {
		let body;
		try {
			body = await response.json();
		} catch {
			body = { error: "unreadable_body" };
		} finally {
			release();
		}
		const refusal = new RestError(response.ok ? 502 : response.status, body, retryAfterOf(response));
		noteSession(refusal);
		throw refusal;
	}
	const reader = response.body.getReader();
	controller.signal.addEventListener("abort", () => {
		reader.cancel(controller.signal.reason).catch(() => {});
	}, { once: true });
	const decoder = new TextDecoder();
	return {
		noContent: false,
		async next() {
			let chunk;
			try {
				chunk = await reader.read();
			} catch (err) {
				release();
				throw unanswered(err);
			}
			if (chunk.done) {
				release();
				const tail = decoder.decode();
				return tail === "" ? null : tail;
			}
			answeredAt = Date.now();
			return decoder.decode(chunk.value, { stream: true });
		}
	};
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
	/**
	* A file's bytes as the body, under the file's own type (or
	* `application/octet-stream` where the browser knows none): an artifact's
	* picture moving through the engine's transfer route.
	*/
	putBytes: (path, value) => bodyOf("PUT", path, {
		body: value,
		contentType: value.type || "application/octet-stream"
	}),
	del: (path, body, headers) => bodyOf("DELETE", path, {
		body,
		headers
	}),
	/** A file's bytes — see [blob]. */
	blob,
	/** A long-lived text answer, read as it arrives (see [stream]). */
	stream
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
* and a screen reads those through `lib/useRest.ts`. What this file keeps is
* its own separation: it had a second entry once, and that one is why the
* Fleet screen shipped dead — a screen reaching for its own transport takes
* its client from somewhere, and the somewhere it chose was a context field
* the shell never populated. Only `socket.ts` imports this file.
*/
var api = { 
/** The degraded-mode snapshot, or why not and when to ask again. */
async snapshot() {
	try {
		const response = await fetch(location.origin + "/stream/snapshot", {
			credentials: "same-origin",
			cache: "no-store",
			headers: readerHeaders()
		});
		if (!response.ok) {
			if (response.status === 401) {
				needSession("sign_in");
				return { state: "nobody" };
			}
			if (response.status === 409) {
				if ((await response.json().catch(() => null))?.error === "reader_changed") readerMoved();
				return {
					state: "unread",
					retryAfter: null
				};
			}
			return {
				state: "unread",
				retryAfter: await retryHintOf(response)
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
//#region src/protocol/auth.ts
var auth = {
	/**
	* Whether the company has its first person — `/health`'s `identity`:
	* `ready`, `unclaimed`, `unknown`, or absent. `/health` is unguarded and is
	* the one answer an install with nobody in it can reach, which is why the
	* sign-in page asks it rather than `/iam`.
	*/
	firstPerson: async () => (await rest.get("/health"))?.identity,
	/** The password floor and the second-factor posture — unguarded, like `/health`. */
	config: async (signal) => await rest.get("/auth/config", signal),
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
	*
	* MADE FOR NOBODY ([RequestOptions.forReader]): it signs somebody in, who
	* may be anybody but the person this tab was read by, and the screen that
	* made it hands the tab to them on the answer (`lib/session.ts`).
	*/
	exchangeToken: async (token) => (await rest.request("POST", "/auth/token", {
		body: {},
		headers: { Authorization: `Bearer ${token}` },
		forReader: false
	})).body,
	/** Render an invitation without spending it. */
	viewInvite: async (id, secret) => (await rest.request("GET", `/auth/invite/${encodeURIComponent(id)}`, { headers: { [INVITE_SECRET_HEADER]: secret } })).body,
	/** Redeem an invitation, which creates the person and signs them in. */
	redeemInvite: async (id, body) => await rest.post(`/auth/invite/${encodeURIComponent(id)}`, body),
	/** Say whose password a reset link sets, without spending it. */
	viewReset: async (id, secret) => (await rest.request("GET", `/auth/reset/${encodeURIComponent(id)}`, { headers: { [RESET_SECRET_HEADER]: secret } })).body,
	/**
	* Set a new password from a reset link, once. It ends every session the
	* person held and signs nobody in: the person signs in next.
	*/
	spendReset: async (id, body) => await rest.post(`/auth/reset/${encodeURIComponent(id)}`, body),
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
	* Change your own password. The current one — and, from somebody who holds
	* a second factor, a code, exactly as a step-up asks it — is the proof, so
	* no step-up is asked first; the change ends every other session and
	* personal token you hold, and answers a fresh session for this browser.
	*/
	changePassword: async (body) => await rest.post("/auth/password", body),
	/**
	* Who this browser is signed in as — ended by `signal` where the caller
	* passes one, for a read a newer one has superseded.
	*
	* MADE FOR NOBODY ([RequestOptions.forReader]), because it is how a tab
	* LEARNS who the session is: the frame's read and the hand-over a
	* `reader_changed` asks for both take its answer (`lib/session.ts`), and a
	* read refused for naming the last reader would leave the tab nothing to be
	* handed over with.
	*/
	session: async (signal) => (await rest.request("GET", "/auth/session", {
		signal,
		forReader: false
	})).body,
	/**
	* The same read, made FOR THE TAB'S READER — what the keepalive asks
	* (`keepalive.ts`). It re-issues the cookie exactly as [session] does, and
	* it is how a tab nobody is using learns that the browser changed hands
	* under it: refused `reader_changed`, it is handed to whoever the session
	* is now, within two keepalive intervals of the sign-in that moved it.
	*/
	keepAlive: async () => await rest.get("/auth/session"),
	/**
	* End this browser's session. The engine clears the cookie whatever its
	* own write did, so an answer at all means this browser holds no session.
	*/
	logout: async () => {
		await rest.post("/auth/logout", {});
	},
	/**
	* End ONE named session of yours — a laptop left signed in somewhere —
	* without ending this browser's.
	*/
	logoutOne: async (lineage) => {
		await rest.post(`/auth/logout/${encodeURIComponent(lineage)}`, {});
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
//#region src/protocol/retry.ts
/**
* When to ask the engine again after it said it cannot answer yet.
*
* THE ENGINE SAYS WHEN, and every retry path in this dashboard reads that one
* way, here. A socket query's or a watch's `unavailable` frame carries
* `retry_after`, and a REST `503` the engine wrote carries a `Retry-After`
* header; both are whole seconds decided by the engine — the refusal's derived
* hint where it has one, the node's health tick where it has none — and ZERO
* where waiting will not change the answer: a log at its byte ceiling, a
* record this node cannot decode, a barrier its broker refused.
*
* PURE ARITHMETIC OVER A NUMBER, bounded by the engine's own ceiling
* {@link RETRY_AFTER_MAX_MS}, declared in `contract/retry.ts` beside the
* fallback a caller waits when an answer carried no hint at all.
*/
/**
* The wait a hint of `seconds` asks for, in milliseconds, or `null` for "do not
* ask again on a timer".
*
* ZERO IS THE ANSWER, never an omission: the engine is saying waiting will not
* change it, so nothing re-asks — the screen shows the refusal and what it
* names, and it is asked again only when something a person does could have
* changed it (a reconnect, a write, a reload). A caller whose answer carried no
* hint at all does not come here: what an absent hint means is the caller's.
*
* BOUNDED, because the one hint the engine does not fix — a backlog divided by
* a drain rate — runs to minutes on a node that has just joined, and a screen
* that says "this fills in on its own" for that long is not believed.
*
* The hint is whole seconds and never negative — both parsers that read one
* admit nothing else — so anything at or under zero is the zero.
*/
function retryAfterMs(seconds) {
	if (!(seconds > 0)) return null;
	return Math.min(seconds * 1e3, RETRY_AFTER_MAX_MS);
}
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
* query registry answers — not for everything. Writes and the guarded reads no
* query answers (`/secrets`, `/setup`, `/config`) go over REST through
* `rest.ts`, and this file makes two HTTP requests of its own: the degraded
* snapshot, which keeps the page honest while the socket is down (a proxy that
* refuses to upgrade, a restarting engine) and stops the moment the socket is
* back, and the refusal probe after a handshake that never opened.
*
* # What a close means
*
* The handshake carries the session cookie, and the tab's reader in its URL
* (a browser sets no header on a WebSocket), and the engine decides it once.
* After that it CLOSES the socket when the identity estate moves under it —
* and the code says what the tab should do:
*
*  - 4401 (`CLOSE_UNAUTHENTICATED`): the session the socket was opened with
*    ended — a sign-out here or everywhere, a suspension, a removal, a
*    revocation, a fleet-wide invalidation, or a STEP-UP, which replaces the
*    session it was made from. Not a refusal on its own: the browser may hold
*    a newer cookie, and after a step-up it does, once the answer that set it
*    has landed. So the tab waits for its own requests to settle and asks
*    `GET /auth/session` once: a `401` is nobody and sends the reader to sign
*    in with nothing dialled, a session somebody else's sign-in left in the
*    browser hands the tab to them (`LiveSocketOptions.takeSession`), and any
*    other answer dials again.
*  - 4403 (`CLOSE_FORBIDDEN`): the engine knows who this is and will not serve
*    them this surface. Reconnecting reaches the same person with the same
*    access, so the socket STOPS and the page says why.
*  - anything else, the standard's 1013 "try again later" included: an
*    ordinary reconnect on the backoff.
*/
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
* What a watch refusal's error frame names in `what` — the engine's
* `watchWhat`. A watch carries no query id, so this is how an error frame about
* one is told from a query's.
*/
var WATCH_WHAT = "watch";
/**
* What an artifact room's join, its refusal and its withdrawal name in `what`:
* the engine's `artifactJoinWhat`. A join's answer carries its id; a
* withdrawal, which the engine sends when the reader's access narrows while
* they are in the room, carries none and names the artifact instead.
*/
var ARTIFACT_JOIN_WHAT = "artifact_join";
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
* A query the engine refused, with what its error frame said beyond the code.
*
* AN ERROR WHOSE MESSAGE IS THE CODE, so every caller that reads a refusal by
* [queryErrorCode] of its message keeps working, and a TYPE beside it for what
* the text of an error is no place to carry: the refusal — on AUTHORITY, the
* rule and the grants that would have admitted the reader; on `unavailable`,
* the state log's refusal and whether, and when, asking again can change it.
*/
var QueryError = class extends Error {
	/**
	* The refusal behind the code, or null — for a frame that is neither a
	* refusal on authority nor an `unavailable` answer, and for the socket's own
	* `timeout` and `closed`, which no engine said anything about.
	*/
	refusal;
	/**
	* The refusal's own sentence on a `bad_params` refusal — the parameter to
	* change and what it accepts — and null everywhere else. The engine writes
	* that one refusal FOR the caller and keeps every other failure's text in
	* its log, so this is never a path or a driver's message. An `unavailable`
	* answer's words are its {@link LogRefusal}'s.
	*/
	detail;
	constructor(code, refusal = null, detail = null) {
		super(code);
		this.name = "QueryError";
		this.refusal = refusal;
		this.detail = detail;
	}
};
/**
* Whether a refusal is the state log's, carried by an `unavailable` answer,
* rather than one on authority. The two ride the same field of an answer
* because a screen hands both to `QueryState` the same way.
*/
function isLogRefusal(refusal) {
	return "retryAfter" in refusal;
}
/**
* How soon something the engine answered `unavailable` is asked again — a
* query (see `useQuery`) or a watch — in milliseconds, or `null` for "not on a
* timer".
*
* `unavailable` is the engine saying it cannot answer HERE, and its frame says
* when that may change: `retry_after`, read through {@link retryAfterMs} —
* waited out, bounded, and ZERO meaning waiting will not change it, so nothing
* re-asks. An answer carrying no hint waits {@link UNAVAILABLE_RETRY_MS}, what
* the engine says when it has nothing better.
*/
function unavailableRetryMs(refusal) {
	if (refusal === null || !isLogRefusal(refusal)) return UNAVAILABLE_RETRY_MS;
	return retryAfterMs(refusal.retryAfter);
}
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
* refusal on authority nor an `unavailable` answer.
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
var LiveSocket = class {
	store;
	takeSession;
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
	/**
	* Whether the latch above is set because NOBODY IS SIGNED IN (see
	* `authRejected`) — the one refusal the tab coming back may lift, since a
	* sign-in in another tab gives this one the cookie too.
	*/
	signedOut = false;
	/**
	* Whether a sign-out holds the socket (see `hold`). Not `refused`, because
	* a sign-out that fails gives the socket back as it found it — still
	* refused, if it was.
	*/
	held = false;
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
	/**
	* The artifact rooms this tab is in, by artifact id. HELD HERE rather than
	* only sent, for the watch's reason: a room lives in the engine's routing
	* index for ONE socket, so every open joins each again.
	*/
	rooms = /* @__PURE__ */ new Map();
	/** Join frames out now, by their id, to the artifact each names. */
	joins = /* @__PURE__ */ new Map();
	/**
	* Which room a tab has open, and whether somebody is typing in it.
	*
	* HELD RATHER THAN SENT AND FORGOTTEN, for the reason the watch is: it is
	* state the SERVER keeps per socket, so a reconnect gets a new socket that
	* knows nothing, and a reader sitting in a room through a thirty-second
	* backoff would come back invisible to everybody else in it and see nobody.
	*/
	focused = "";
	typing = false;
	/** Seats, by AGENT ID, whose live call is being fetched whole, each with
	*  whether a push found the call behind again while the fetch was out — see
	*  `fetchCalls`. */
	fetchingCalls = /* @__PURE__ */ new Map();
	constructor(store, options = {}) {
		this.store = store;
		this.takeSession = options.takeSession ?? (() => true);
	}
	/**
	* The tab came back to a socket stopped because nobody was signed in: dial
	* ONCE, which is what notices a sign-in made in another tab. A dial that
	* finds nobody still stops again.
	*/
	onVisible = () => {
		if (!this.signedOut || this.isClosed || document.visibilityState !== "visible") return;
		this.signedOut = false;
		this.refused = false;
		this.connect();
	};
	/**
	* Dial for the session the frame has just read (`app/Shell.tsx`), unless a
	* dial is already open or under way.
	*
	* IT LIFTS WHAT AN EARLIER SESSION LATCHED: the frame calls this for every
	* session it reads — after a sign-in, or once an admin has put a refusal
	* right — and a refusal or a sign-out recorded under the last one no longer
	* describes the browser.
	*/
	start() {
		this.lift();
		this.connect();
	}
	/**
	* Drop this socket and re-dial immediately.
	*
	* A dropped envelope is gone: the server's per-client queue discards the
	* OLDEST frame under backpressure, so a lost `agents` overlay is never
	* re-sent and the only true repair is a fresh handshake snapshot. It is
	* also how a restored access is tried: a re-dial is a new attempt, so the
	* last refusal no longer describes it.
	*/
	reconnect() {
		this.lift();
		if (this.sock) this.sock.close();
		else this.connect();
	}
	/** Forget what the last refusal or sign-out latched. */
	lift() {
		this.refused = false;
		this.signedOut = false;
		this.store.setAuthRejected(false);
		this.store.setAccessRefused(null);
	}
	/**
	* Dial nothing while this tab signs out, until `release()`.
	*
	* The engine ends this session's socket (4401) as it applies the sign-out,
	* and that close re-dialled once this tab's own requests had settled — which
	* is before the sign-out's reload — so every sign-out made one more
	* handshake the engine refused.
	*/
	hold() {
		this.held = true;
		clearTimeout(this.reconnectTimer);
		this.reconnectTimer = 0;
		this.stopFallback();
	}
	/**
	* Give back what `hold` took: a sign-out nothing answered stays on the page,
	* and the socket dials again — unless a refusal had already stopped it,
	* which the sign-out did not change.
	*/
	release() {
		this.held = false;
		this.connect();
	}
	stop() {
		this.isClosed = true;
		document.removeEventListener("visibilitychange", this.onVisible);
		clearTimeout(this.reconnectTimer);
		clearTimeout(this.watchRetry);
		for (const room of this.rooms.values()) clearTimeout(room.retry);
		this.rooms.clear();
		this.joins.clear();
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
	* Rejects with a [QueryError] carrying the server's machine-readable code
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
			entry.reject(new QueryError("timeout"));
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
	* Say which room this tab is looking at, and whether it is being typed in.
	*
	* IT ASKS NOTHING AND IS ANSWERED BY NOTHING. What it buys is that the
	* fleet-wide presence probe is bounded to the rooms somebody actually has
	* open — without it a node would have to ask its peers about every room in
	* the company on every tick.
	*
	* It is not delivered while the socket is down and is not queued: presence
	* is about now, and a probe from ten seconds ago is worse than none. The
	* reconnect re-sends what this tab is looking at, which is the state above.
	* Only the chat screen sends it, and the frame mounts that screen only for
	* a reader holding Chatlet, so a tab without it never names a room.
	*/
	focus(channelID, typing) {
		this.focused = channelID;
		this.typing = typing;
		this.sendFocus();
	}
	sendFocus() {
		if (!this.connected || !this.sock) return;
		try {
			this.sock.send(JSON.stringify({
				kind: "chat_focus",
				channel_id: this.focused,
				typing: this.typing
			}));
		} catch {}
	}
	/**
	* The engine refused a watch.
	*
	* `unavailable` means this node could not read the chart that decides it,
	* or the directory a login resolves through, so it is asked again when the
	* frame's `retry_after` says that may have changed ({@link
	* unavailableRetryMs}). A ZERO is a read no wait clears, and ANY OTHER CODE
	* IS A DECISION; either way asking again on a timer would only be answered
	* the same: the screens' polls carry on as they did before there was a push
	* at all, and the next socket asks once more in case the answer moved.
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
	/**
	* Be in an artifact's room for as long as `listener` is: hear what moves on
	* the artifact (`artifact_changed`), what its datastore commits
	* (`artifact_data`) and who else is there (`artifact_presence`). Returns the
	* way out.
	*
	* ONE JOIN PER ARTIFACT however many screens listen: the first listener
	* joins, the last to leave leaves, and every open of the socket joins each
	* room again with the `through` the tab last held, so the engine answers
	* whether a catch-up is enough (`resync: false`) or the datastore must be
	* read whole. The engine decides a join as it decides reading the artifact,
	* and refuses one in the words an artifact nobody holds is answered with; a
	* refusal, and a room the engine withdraws when the reader's access narrows,
	* reach every listener's `refused`, and no push follows.
	*/
	room(artifact, listener) {
		const id = artifact.trim();
		let room = this.rooms.get(id);
		if (!room) {
			if (this.rooms.size >= 8) {
				listener.refused?.(new QueryError("bad_params", null, `a tab is in at most 8 artifact rooms at once`));
				return () => {};
			}
			room = {
				listeners: /* @__PURE__ */ new Set(),
				joining: 0,
				joined: false,
				since: 0,
				retry: 0
			};
			this.rooms.set(id, room);
			room.listeners.add(listener);
			this.sendJoin(id);
		} else room.listeners.add(listener);
		return () => this.unlisten(id, listener);
	}
	/**
	* Say what this tab is doing in a room it is in: what it is looking at and
	* whether it is editing. Who is doing it is never the frame's to say: the
	* engine shows the seat the socket's credential resolved to. A frame for a
	* room the engine has not put this socket in is not sent, since it would be
	* dropped there; the next one after the join lands says it again.
	*/
	presence(artifact, activity, focus) {
		if (!this.rooms.get(artifact)?.joined || !this.connected || !this.sock) return;
		try {
			this.sock.send(JSON.stringify({
				kind: "presence",
				artifact,
				activity,
				focus
			}));
		} catch {}
	}
	unlisten(artifact, listener) {
		const room = this.rooms.get(artifact);
		if (!room || !room.listeners.delete(listener) || room.listeners.size > 0) return;
		clearTimeout(room.retry);
		this.rooms.delete(artifact);
		if (room.joining) this.joins.delete(room.joining);
		if (!this.connected || !this.sock) return;
		try {
			this.sock.send(JSON.stringify({
				kind: "artifact_leave",
				artifact
			}));
		} catch {}
	}
	sendJoin(artifact) {
		const room = this.rooms.get(artifact);
		if (!room || !this.connected || !this.sock) return;
		clearTimeout(room.retry);
		room.retry = 0;
		if (room.joining) this.joins.delete(room.joining);
		const id = this.nextQueryId++;
		room.joining = id;
		this.joins.set(id, artifact);
		const frame = {
			kind: "artifact_join",
			id,
			artifact,
			...room.since > 0 ? { since: room.since } : {}
		};
		try {
			this.sock.send(JSON.stringify(frame));
		} catch {}
	}
	/** A join's answer: the room as it presents now, and where the datastore stands. */
	joinAnswered(id, answer) {
		const artifact = this.joins.get(id);
		if (artifact === void 0) return;
		this.joins.delete(id);
		const room = this.rooms.get(artifact);
		if (!room || room.joining !== id) return;
		room.joining = 0;
		room.joined = true;
		if (typeof answer.through === "number" && answer.through > 0) room.since = answer.through;
		for (const listener of [...room.listeners]) listener.joined?.(answer);
	}
	/**
	* A join the engine refused, or a room it withdrew (no id). `unavailable` is
	* a node that could not decide, asked again on the frame's `retry_after`
	* (`unavailableRetryMs`); every other code is a decision, said to the
	* listeners, which stay registered so a later open of the socket (after a
	* share is put right) asks again.
	*/
	joinRefused(msg) {
		let artifact = msg.artifact ?? "";
		if (msg.id !== void 0) {
			const joined = this.joins.get(msg.id);
			if (joined === void 0) return;
			this.joins.delete(msg.id);
			artifact = joined;
		}
		const room = this.rooms.get(artifact);
		if (!room || msg.id !== void 0 && room.joining !== msg.id) return;
		room.joining = 0;
		room.joined = false;
		const code = msg.error || "query_failed";
		const refusal = refusalOf$1(msg);
		if (code === "unavailable") {
			const wait = unavailableRetryMs(refusal);
			if (wait !== null) room.retry = setTimeout(() => {
				room.retry = 0;
				this.sendJoin(artifact);
			}, wait);
		}
		const error = new QueryError(code, refusal, badParamsDetail(msg));
		for (const listener of [...room.listeners]) listener.refused?.(error);
	}
	/** Hands a room's push to its listeners, reading the room off the payload's own `artifact`. */
	roomPush(kind, data) {
		if (!data || typeof data !== "object") return;
		const artifact = data.artifact;
		if (typeof artifact !== "string") return;
		const room = this.rooms.get(artifact);
		if (!room?.joined) return;
		if (kind === "data") {
			const through = data.through;
			if (typeof through === "number" && through > room.since) room.since = through;
		}
		for (const listener of [...room.listeners]) if (kind === "changed") listener.changed?.(data);
		else if (kind === "data") listener.data?.(data);
		else listener.presence?.(data);
	}
	connect() {
		if (this.isClosed || this.refused || this.held || this.sock && (this.sock.readyState === WebSocket.OPEN || this.sock.readyState === WebSocket.CONNECTING)) return;
		const proto = location.protocol === "https:" ? "wss" : "ws";
		let sock;
		try {
			sock = new WebSocket(`${proto}://${location.host}${PATH}${readerQuery()}`);
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
			for (const artifact of this.rooms.keys()) this.sendJoin(artifact);
			if (this.focused !== "") this.sendFocus();
		};
		sock.onmessage = (e) => this.onMessage(String(e.data));
		sock.onclose = (e) => {
			this.stopPing();
			this.sock = null;
			clearTimeout(this.watchRetry);
			this.watchRetry = 0;
			this.joins.clear();
			for (const room of this.rooms.values()) {
				clearTimeout(room.retry);
				room.retry = 0;
				room.joining = 0;
				room.joined = false;
			}
			for (const entry of this.inflight.values()) {
				clearTimeout(entry.timer);
				entry.timer = 0;
			}
			this.store.setConnected(false);
			if (this.held) return;
			const code = e ? e.code : 0;
			if (code === 4403) {
				this.accessRefused(e.reason);
				return;
			}
			if (code === 4401) {
				whenRequestsSettle().then(() => this.redialIfSignedIn());
				return;
			}
			this.scheduleReconnect();
			this.startFallback();
			if (!handshakeCompleted) this.probeRefusal();
		};
		sock.onerror = () => {
			this.store.setConnected(false);
		};
	}
	/**
	* After a `4401`: dial again only if the browser still holds a session.
	*
	* `GET /auth/session` FIRST, rather than a dial whose handshake decides:
	* a session that ended elsewhere — a password change, a sign-out
	* everywhere, an administrator — was a refused handshake, its refusal
	* probe and a refused degraded-mode snapshot before the sign-in, three
	* `401`s where one says it. A `401` here is that one (`rest.ts` raises the
	* sign-in need); any other failure says nothing about the session, and
	* the dial's own handshake decides.
	*
	* AND ONLY FOR THE PERSON THIS TAB WAS READ BY (`takeSession`): a sign-in
	* as somebody else in another tab ends this tab's session and moves the
	* cookie, and a re-dial on any answer reached the company as them in a tab
	* still holding the last reader's.
	*/
	async redialIfSignedIn() {
		try {
			const session = await auth.session();
			if (!this.takeSession(session.person)) return;
		} catch (err) {
			if (err instanceof RestError && err.status === 401) {
				this.authRejected();
				return;
			}
		}
		this.connect();
	}
	/**
	* Ask, over plain HTTP, whether that dial was refused or merely failed.
	*
	* A handshake the engine refuses NEVER reaches this page as a close code. A
	* close code travels in a close frame, and a connection that never opened
	* has no frames — so the browser reports 1006, the same code it gives for an
	* engine that is simply down, and withholds the status deliberately (a page
	* that could read it could use a socket to scan ports it cannot otherwise
	* reach).
	*
	* So the status is fetched where a browser will hand it over. A plain GET of
	* the same path runs the same guard, with the same cookie, and stops one line
	* short of the upgrade: 401 is nobody signed in, 426 (Upgrade Required)
	* means the session was accepted and only the missing header stopped it, a
	* 403 is somebody refused this surface, and a `503` the engine wrote is a
	* node that could not decide the handshake yet, which says in its
	* `Retry-After` when to dial again (see `redialWhenSaid`). A throw is the
	* network, which is not an auth problem and must not send anybody to sign
	* in.
	*/
	async probeRefusal() {
		if (this.isClosed) return;
		try {
			const res = await fetch(PATH, {
				credentials: "same-origin",
				cache: "no-store",
				headers: readerHeaders()
			});
			if (res.status === 401) this.authRejected();
			else if (res.status === 409) {
				if ((await res.json().catch(() => null))?.error === "reader_changed") readerMoved();
			} else if (res.status === 503) await this.redialWhenSaid(res);
			else if (res.status === 403) {
				const body = await res.json().catch(() => null);
				if (body?.error === "second_factor_enrolment_required") this.enrolmentRequired();
				else this.accessRefused(body?.detail ?? body?.error ?? "");
			}
		} catch {}
	}
	/**
	* A refused handshake's `503`: dial again when the engine said, in place of
	* the backoff — exactly, and bounded at `RETRY_AFTER_MAX_MS`.
	*
	* ONLY THE ENGINE'S `503` (`retryHintOf`): a proxy in front of a node that
	* is down writes one too, and that is the backoff's case. A ZERO keeps the
	* backoff rather than stopping the loop, because a dial is not a re-ask of
	* this one node — behind a balancer the next one may reach another — and
	* the loop is this tab's only way back to any engine.
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
	* `reconnect()` is the way back, once an admin has put it right — or
	* `start()`, which the frame calls once it reads the session again.
	*/
	accessRefused(reason) {
		this.stopDialling();
		this.store.setAccessRefused(reason);
	}
	/**
	* The engine accepts this browser's session for nothing but enrolling the
	* second factor the deployment requires. Every dial would be refused the
	* same way until it has, so the loop stops; the enrolment ends in the
	* frame, which dials again (`start()`) for the whole session it opened.
	*/
	enrolmentRequired() {
		this.stopDialling();
		needSession("second_factor");
	}
	/** Stops the reconnect loop and the REST fallback, until `start()` or `reconnect()`. */
	stopDialling() {
		this.refused = true;
		clearTimeout(this.reconnectTimer);
		this.reconnectTimer = 0;
		this.stopFallback();
	}
	/**
	* The engine resolved nobody from this browser's cookie.
	*
	* Asking for a sign-in is the repair — the dashboard is served
	* unauthenticated by design (the page that signs a person in cannot itself
	* require them to be), so the browser has no other moment to learn it needs
	* one — and the store flag is what the chrome reads meanwhile.
	*
	* AND THE LOOP STOPS, as it does for a refusal: every dial until somebody
	* signs in is the same 401, and a signed-out tab — the sign-in page, an
	* invitation's or a reset link's — dialled one on its backoff for as long
	* as it stayed open, each a failed handshake and a 401 in the console. What
	* ends it is a sign-in: in this tab, which lands in the frame and dials
	* (`start()`); or in another, which gives this one the cookie too and is
	* noticed by ONE dial when the tab comes back (`onVisible`).
	*
	* The socket does not own the screen: the sign-in is a route, and a
	* transport that reaches into the router is a transport that cannot be
	* tested without one — so it raises the session need the app follows.
	*/
	authRejected() {
		this.store.setAuthRejected(true);
		needSession("sign_in");
		this.stopDialling();
		this.signedOut = true;
		document.addEventListener("visibilitychange", this.onVisible);
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
				this.fetchCalls(this.store.applyAgents(msg.data));
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
			case "artifact_changed":
				this.roomPush("changed", msg.data);
				break;
			case "artifact_data":
				this.roomPush("data", msg.data);
				break;
			case "artifact_presence":
				this.roomPush("presence", msg.data);
				break;
			case "chat_posted":
			case "chat_edited":
			case "chat_deleted":
			case "chat_reacted":
			case "chat_room_changed":
			case "chat_membership_changed":
				this.store.applyChatChange(msg.data);
				break;
			case "chat_presence":
				this.store.applyChatPresence(msg.data);
				break;
			case "chat_cursor_moved":
				this.store.applyChatRead(msg.data);
				break;
			case "result":
				if (msg.id !== void 0 && this.joins.has(msg.id)) {
					this.joinAnswered(msg.id, msg.data);
					break;
				}
				this.settle(msg.id, null, msg.data);
				break;
			case "error":
				if (msg.what === WATCH_WHAT && msg.id === void 0) {
					this.watchAnswered(msg);
					break;
				}
				if (msg.what === ARTIFACT_JOIN_WHAT) {
					this.joinRefused(msg);
					break;
				}
				this.settle(msg.id, msg.error || "query_failed", null, refusalOf$1(msg), badParamsDetail(msg));
				break;
			case "pong": break;
			default: this.store.noteUnknownPush(msg.kind);
		}
	}
	/**
	* Fetch, whole, each seat's live call this tab holds behind what a push
	* described — asked by the seat's AGENT ID, the key the push named it by — the push that moved one of its heavy fields was dropped by the
	* server's queue (`mergeLiveCall`).
	*
	* ONE ASK PER SEAT IN FLIGHT, but a push that finds the call behind while it
	* is out is NOT the same thing said twice: the answer may have been read
	* before that push's change, and the store then drops it or takes only the
	* fields it has newer (`Store.applyLiveCall`). So such a push is remembered,
	* and once the answer lands BEHIND the pushes applied since — or the ask
	* fails — the call is asked for again, once, rather than left behind until
	* the seat's next push, which a frozen failed call or a long round never
	* sends. An answer at or past them holds everything they named, and ends it.
	*/
	fetchCalls(ids) {
		for (const id of ids) {
			if (this.fetchingCalls.has(id)) {
				this.fetchingCalls.set(id, true);
				continue;
			}
			this.fetchingCalls.set(id, false);
			this.query("live_call", { agent_id: id }).then((answer) => this.store.applyLiveCall(answer), () => false).then((current) => {
				const again = this.fetchingCalls.get(id) ?? false;
				this.fetchingCalls.delete(id);
				if (again && !current) this.fetchCalls([id]);
			});
		}
	}
	settle(id, error, data, refusal = null, detail = null) {
		if (id === void 0) return;
		const entry = this.inflight.get(id);
		if (!entry) return;
		this.inflight.delete(id);
		clearTimeout(entry.timer);
		if (error) entry.reject(new QueryError(error, refusal, detail));
		else entry.resolve(data);
	}
	failInflight(reason) {
		for (const entry of this.inflight.values()) {
			clearTimeout(entry.timer);
			entry.reject(new QueryError(reason));
		}
		this.inflight.clear();
	}
	/**
	* Dial again after the backoff — or after `after` ms, where the engine said
	* when (`redialWhenSaid`), which replaces the dial already scheduled and
	* leaves the backoff's count where it was.
	*
	* FULL JITTER: the wait is drawn uniformly between zero and the backoff's
	* ceiling for this attempt. A node that closes every socket at once — a
	* 1013 to everybody on a restart — would otherwise bring every tab back in
	* lockstep, one wave per doubling, each wave as large as the first.
	*/
	scheduleReconnect(after) {
		if (this.isClosed || this.refused) return;
		clearTimeout(this.reconnectTimer);
		let delay = after;
		if (delay === void 0) {
			delay = Math.random() * Math.min(1e3 * 2 ** Math.min(this.attempt, 10), MAX_BACKOFF_MS);
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
		if (read.state === "nobody") return;
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
* The fleet's broker and object store, as the engine bounds them: the kinds
* a node's broker can be and the disagreements named between the two records
* of its membership, how long a removal may take, and the states of the object
* store's report.
*
* Each is a COPY of something the engine owns, because this is a separate
* build that cannot import a Go identifier — held to the engine's in both
* directions by `internal/api`'s `broker_client_test.go` and
* `objects_test.go`. What is done with them is behaviour, and lives in
* `protocol/broker.ts` and the Settings screens.
*/
/**
* How a node's broker takes part in the fleet's — `placement.BrokerKind`, as
* `String()` renders it.
*
* `unknown` IS A VALUE, not an absence: a presence advertising a kind this build
* does not know (a newer build's) is rendered `unknown` so the cell is never
* empty — an empty cell reads as nothing to look at, and this one is counted
* as a member wherever that is the safe reading.
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
//#endregion
//#region src/protocol/keepalive.ts
/**
* Keeping a tab's session alive while it only listens.
*
* A session ends after twelve hours with nothing happening on it
* (`session.Idle`), and what counts as something happening is a REQUEST: the
* engine re-issues the cookie with a fresh idle deadline on any answer to a
* request carrying one that is five minutes old (`session.ReissueAfter`). The
* live socket neither enforces nor extends that deadline — it is authenticated
* once, at its handshake — so a tab somebody leaves open on a dashboard that
* only reads the socket would idle out with the person looking at it, and the
* next reconnect would land them on the sign-in form.
*
* So while the socket is open, a REST read is made at least once per
* re-issue interval — `GET /auth/session`, the cheapest guarded read there is —
* unless some other request already heard from the engine since the last
* tick, which re-issued the cookie just as well. Hidden or not: an open live
* view is somebody using the dashboard, and the session's absolute deadline
* still bounds how long that can last. A 401 is read where every other is
* (`rest.ts`): a session that ended needs a sign-in.
*
* THE READ IS MADE FOR THE TAB'S READER (`auth.keepAlive`), unlike the frame's
* read of who the session is, so it is also how a tab that only listens learns
* the browser changed hands under it: somebody signed in as somebody else in
* another tab and the close of this tab's session never reached its socket.
* The engine refuses the read `reader_changed`, and the tab is handed over
* within TWO intervals — under ten minutes of the sign-in — rather than
* drawing the last person's company for as long as it stays open. Two, not
* one: a tick passes over a tab the engine answered less than an interval
* ago, and that answer may have come just before the sign-in.
*/
/**
* How often an open tab makes sure the engine has heard from it, in ms:
* the engine's own re-issue interval (`session.ReissueAfter`, five minutes).
* Any sooner re-issues nothing — a cookie younger than that is not
* re-issued — and any later lets a whole interval go unrenewed.
*/
var SESSION_KEEPALIVE_MS = 3e5;
var SessionKeepAlive = class {
	socket;
	timer = 0;
	constructor(socket) {
		this.socket = socket;
	}
	start() {
		if (this.timer) return;
		this.timer = setInterval(() => this.tick(), SESSION_KEEPALIVE_MS);
	}
	stop() {
		clearInterval(this.timer);
		this.timer = 0;
	}
	/** One interval: a read, unless the socket is down or a request already answered. */
	tick(now = Date.now()) {
		if (!this.socket.connected) return;
		if (now - lastAnsweredAt() < 3e5) return;
		auth.keepAlive().catch(() => void 0);
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
*    read floor for that domain (`protocol/session.ts`), or `null` for a
*    write that lands in no log a question reads. A wrong one is SILENT: a
*    write filed under `null` raises no floor, so the screen's next read may
*    come from before the press;
*  - `apps` — the apps ANY ONE of which the change needs, in the order its
*    authority row names them: its app for an app's change, the row's own set
*    for one that draws on whichever of several the caller holds
*    (`answer_knowledge`, the tracker and the knowledge base it answers
*    from), and none for a change no app holds (pausing a seat, answering a
*    coding run). What a control the engine does not offer is held with —
*    "needs Worklet access" — so a reader is told what to ask an admin for
*    (`lib/useWriteAccess.ts`). It is the authority table's, stated per row
*    rather than guessed from the domain: a change can need an app and land
*    in no log;
*  - `refreshes` — questions OUTSIDE that domain's session set
*    (`contract/domains.ts`) that the write also moves, asked again without a
*    floor because they cannot take one;
*  - `scope` — `person` for a change somebody makes to the company's work in
*    their own name, `company` for one that rewrites what every seat's tracker
*    means (a project's settings, the catalogue), which a screen offers only
*    where it says so. It is the authority table's: a write decided by a
*    container's lead or by admin is `company`.
*
* HELD AGAINST THE REAL CATALOGUE by `internal/api/operator`'s
* `TestEveryActionTheDashboardTakesIsOneTheActTransportServes`: every tool is
* served by the act transport and is not a read, every argument is one its
* schema takes, every required one is present, every domain is the log its
* tool writes, and every scope and app set is the one its authority row
* gives it. A renamed argument in Go is a red build here
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
		apps: ["worklet"],
		refreshes: ["work_search"],
		scope: "person"
	},
	answer_knowledge: {
		args: ["q"],
		domain: null,
		apps: ["worklet", "doclet"],
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
		apps: ["worklet"],
		refreshes: ["work_search"],
		scope: "person"
	},
	restore_work_item: {
		args: ["item"],
		domain: "tracker",
		apps: ["worklet"],
		refreshes: ["work_search"],
		scope: "person"
	},
	write_project: {
		args: ["project", "target_date"],
		domain: "tracker",
		apps: ["worklet"],
		refreshes: [],
		scope: "company"
	},
	set_pins: {
		args: ["views", "favorites"],
		domain: "tracker",
		apps: ["worklet"],
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
		apps: ["worklet"],
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
		apps: ["worklet"],
		refreshes: [],
		scope: "person"
	},
	answer_run: {
		args: ["turn_id", "answer"],
		domain: null,
		apps: [],
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
		apps: [],
		refreshes: [],
		scope: "person"
	},
	resume_seat: {
		args: ["handle"],
		domain: null,
		apps: [],
		refreshes: [],
		scope: "person"
	},
	steer_turn: {
		args: ["turn_id", "note"],
		domain: null,
		apps: [],
		refreshes: ["turn"],
		scope: "person"
	},
	write_page: {
		args: [
			"title",
			"body",
			"container",
			"parent",
			"labels",
			"status"
		],
		domain: "pages",
		apps: ["doclet"],
		refreshes: ["knowledge"],
		scope: "person"
	},
	save_page: {
		args: [
			"page",
			"base_version",
			"body",
			"message",
			"watch"
		],
		domain: "pages",
		apps: ["doclet"],
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
		apps: ["doclet"],
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
		apps: ["worklet"],
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
		apps: ["worklet"],
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
		apps: ["worklet"],
		refreshes: [],
		scope: "person"
	},
	post_message: {
		args: ["channel", "body"],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	reply_in_thread: {
		args: [
			"channel",
			"reply_to",
			"body"
		],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	edit_message: {
		args: [
			"channel",
			"message_id",
			"body"
		],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	delete_message: {
		args: ["channel", "message_id"],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	react_to_message: {
		args: [
			"channel",
			"message_id",
			"emoji",
			"remove"
		],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	join_channel: {
		args: ["channel"],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	leave_channel: {
		args: ["channel"],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	create_channel: {
		args: [
			"channel",
			"kind",
			"topic",
			"members"
		],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	open_dm: {
		args: ["to"],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	update_channel: {
		args: [
			"channel",
			"topic",
			"purpose"
		],
		domain: "chat",
		apps: ["chatlet"],
		refreshes: [],
		scope: "person"
	},
	mark_chat_read: {
		args: [
			"cursors",
			"mute",
			"unmute"
		],
		domain: null,
		apps: ["chatlet"],
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
* THE SENTENCE IS SECOND PERSON, and it is ours, except where it is `null`,
* which means "show the engine's own words". Those are the two classes whose
* words say what to change (`invalid`) or whom to ask (`forbidden`). The
* engine answers them as a `detail` worded for a person, or, where nobody has
* worded that refusal for one yet, with no detail and its code's own
* `message` (ADR-0060): never the tool's sentence, which is written for a
* model and names arguments and record ids.
*/
var ACT_ERRORS = {
	invalid_token: "Signed out. Sign in again.",
	identity_unavailable: "Couldn’t confirm who you are. Try again.",
	seat_unavailable: "Your seat was removed. Ask an admin to move you.",
	second_factor_enrolment_required: "Set up two-step verification first.",
	reader_changed: "Someone else is signed in here now, so nothing changed. Reload to continue as them.",
	csrf_origin: "The engine refused a change sent from another site. Make it from this page's own address.",
	unauthorized: "You don’t have access to this.",
	step_up_required: "Confirm it’s you first.",
	unknown_tool: "This engine does not support that change. It may be running a different version. Reload the page.",
	read_only_tool: "The dashboard sent a read as a change. Reload; if it persists, it is a bug.",
	unsupported_media_type: "The dashboard sent the change in a form the engine does not take.",
	op_id_invalid: "The dashboard sent the change without a usable operation key. Reload; if it persists, it is a bug.",
	invalid_input: "What this change was about moved since you opened it. Look again, then make the change.",
	invalid_body: "The dashboard sent a change the engine could not read.",
	body_too_large: "The change is larger than the engine accepts in one request.",
	unreadable_body: "The change could not be read in full by the engine.",
	no_active_revision: "This node has not been handed a company yet, so there is nothing to change. Try again once it has.",
	draining: "This node is shutting down and takes no changes now. Try again in a moment.",
	starting: "This node is still starting and takes no changes yet. Try again in a moment.",
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
	internal_error: "The change failed inside the engine, and trying again will not fix it. Its log has the reason; nothing was refused on purpose."
};
//#endregion
//#region src/contract/caveats.ts
/**
* What a caveat on a landed write asks of the person who reads it, as the
* engine names it (`internal/caveat`, ADR-0060).
*
* `none` is something to know: shown, and gone by itself. `required` is
* something somebody has to act on: shown until it is dismissed. The engine
* states the action per caveat code, so a screen never guesses it from the
* sentence. HELD AGAINST `caveat.Actions` by
* `internal/caveat.TestTheDashboardReadsWhatACaveatAsks`: an action copied
* here and renamed there would render a caveat that needs a person as one that
* goes by itself.
*/
var CAVEAT_ACTIONS = ["none", "required"];
//#endregion
//#region src/protocol/writeOutcome.ts
/**
* What a write's answer means: the one interpreter of "did it land", shared by
* every surface that makes a change for a person.
*
* THE DASHBOARD'S BUTTONS (`act.ts`) AND THE ASSISTANT'S APPROVALS
* (`assistant.ts`) READ ONE ANSWER. An approval executes the same request the
* button beside it would have sent, so what its answer can be, and what each
* tells a person, cannot be decided twice: a copy would come to call a lost
* connection a refusal, or a refusal a success, on one surface and not the
* other.
*
* # What an answer can be
*
*  - `applied`: the record landed, at a position;
*  - `pending`: the engine accepted it and this node has not applied it yet;
*  - `unknown`: nobody can say whether it landed (the connection dropped
*    after the request left, a gateway gave up, or the engine said so). A
*    retry is the person's decision, made under the same operation;
*  - `refused`: the engine said no, with a code from `contract/errors.ts` and
*    the sentence a person is shown for it.
*
* # Every sentence drawn is a person's (ADR-0060)
*
* The engine answers a refusal's `detail` in a person's words, or with none
* where nobody has worded it yet, and then its `message` (the code's own
* sentence) is what is drawn. An answer from an older node during a rolling
* upgrade may lack `caveats` (read as none) or carry no detail (its message
* stands); neither is an error.
*/
/**
* The caveats an answer carries, as this build reads them: each with a code
* and a sentence, and an action this build knows. An action it does not know
* (a newer engine's) is read as `none`, the engine's own rule for a caveat it
* cannot explain: a toast nobody can say why it stays must not demand
* dismissal. Anything that is not a caveat is dropped.
*/
function caveatsOf(raw) {
	if (!Array.isArray(raw)) return [];
	const known = new Set(CAVEAT_ACTIONS);
	const out = [];
	for (const item of raw) {
		const c = item ?? {};
		if (typeof c.code !== "string" || typeof c.message !== "string") continue;
		if (c.message.trim() === "") continue;
		const action = typeof c.action === "string" && known.has(c.action) ? c.action : "none";
		out.push({
			code: c.code,
			message: c.message,
			action
		});
	}
	return out;
}
/**
* What a person is told of a change nobody could confirm: the engine's own
* `detail` on an unknown is the operation key's retry rule, written for a
* client sending the header, and a person never sees the key (ADR-0060).
*/
var UNCONFIRMED_SENTENCE = "Nobody could confirm it was saved. Look again to see whether it was.";
/** Whether a code off the wire is one this build knows. */
function isActErrorCode(code) {
	return Object.hasOwn(ACT_ERRORS, code);
}
/**
* The 2xx answer `{outcome, position, receipt, caveats}` a write returns.
*
* AN OUTCOME THIS BUILD DOES NOT KNOW IS NOT A SUCCESS, the engine's own rule
* for its own answer (`receiptOf`), for the same reason: it is the one reading
* that cannot tell a person to stop looking at a change nobody vouched for.
*/
function writeLanded(body) {
	const answer = body ?? {};
	if (answer.outcome !== "applied" && answer.outcome !== "pending") return {
		kind: "unknown",
		reason: "The engine accepted the change and could not confirm it landed."
	};
	return {
		kind: answer.outcome,
		position: typeof answer.position === "string" ? answer.position : null,
		receipt: answer.receipt,
		caveats: caveatsOf(answer.caveats)
	};
}
/**
* The refusals that say the screen the press was made from is OUT OF DATE:
* somebody else changed the object since it was drawn (`stale_version`,
* `conflict`), removed it (`not_found`), made it already (`exists`), answered
* it (`already_answered`), or moved what the request named so its operation
* already landed on something else (`invalid_input`). Each asks again every
* question the write would have moved, without raising a floor, since nothing
* of this tab's landed, so the page redraws what IS there and the next press
* is made against it.
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
function writeFailed(err) {
	if (err.body.outcome === "unknown") return {
		kind: "unknown",
		reason: UNCONFIRMED_SENTENCE
	};
	const code = isActErrorCode(err.code) ? err.code : "";
	if (err.unanswered || err.status >= 500 && code === "") return {
		kind: "unknown",
		reason: err.detail || `The answer was lost (status ${err.status}).`
	};
	return {
		kind: "refused",
		code,
		sentence: (code === "" ? null : ACT_ERRORS[code]) ?? (err.detail || err.sentence || `The engine refused the change (status ${err.status}).`),
		hint: err.hint,
		grants: err.grants,
		retryable: err.retryHint !== null && retryAfterMs(err.retryHint) !== null,
		remedy: err.remedy,
		lookAgain: LOOK_AGAIN.has(code)
	};
}
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
* And every press names the tab's reader there, so a press — or a Retry —
* from a tab whose browser somebody else has signed in to since is refused
* `reader_changed` unrun rather than made as them, and the tab is handed over.
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
	const floors = options.floors ?? session;
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
	const landed = writeLanded(body);
	if (landed.kind === "unknown") return unknown(landed.reason);
	const { domain, refreshes } = ROWS[tool];
	floors.written(domain, landed.position, refreshes);
	return {
		kind: landed.kind,
		tool,
		opId,
		position: landed.position,
		domain,
		receipt: landed.receipt,
		caveats: landed.caveats
	};
}
/** What a refusal, or a request that never got an answer, came to. */
function refusalOf(tool, opId, err, floors) {
	const failed = writeFailed(err);
	if (failed.kind === "unknown") return {
		kind: "unknown",
		tool,
		opId,
		reason: failed.reason
	};
	if (failed.lookAgain) {
		const { domain, refreshes } = ROWS[tool];
		floors.written(domain, null, refreshes);
	}
	const { lookAgain: _lookAgain, ...refused } = failed;
	return {
		...refused,
		tool,
		opId
	};
}
//#endregion
export { BROKER_FINDING_KINDS, BROKER_KINDS, BROKER_REMOVE_TIMEOUT_MS, LiveSocket, QueryError, REQUEST_TIMEOUT_MS, RestError, SESSION_KEEPALIVE_MS, SessionFloors, SessionKeepAlive, Store, act, api, auth, confirmStepUp, currentSessionNeed, domainOf, isAbort, isLogRefusal, keepsOperation, lastAnsweredAt, layoutOpID, needSession, newActOpID, newGateOpID, onSessionNeed, onStepUpAsked, queryErrorCode, refusedGrants, rest, retryAfterMs, retryAfterSeconds, sessionNeedsEnrolment, sessionRestored, setStepUpConfirmer, setTabReader, stepUpAsked, unavailableRetryMs, whenRequestsSettle };
