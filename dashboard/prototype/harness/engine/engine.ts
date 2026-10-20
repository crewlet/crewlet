/**
 * The demo engine: one in-memory company, answering every question the
 * dashboard asks and making the writes a person can make in it.
 *
 * It stands where the real engine stands, at the two transports: the socket
 * (`DemoSocket` hands it every query and receives every push it makes) and
 * REST (`restShim` hands it every request the dashboard makes over `fetch`).
 * Nothing between the dashboard and those two seams is replaced, so a screen
 * runs exactly the code it runs against a real engine: the protocol module,
 * the store, the hooks, the screens.
 *
 * WHAT IT DOES NOT MODEL IT REFUSES, OUT LOUD. A question with no answer
 * here is refused `unknown_query`; a write with no model is refused the way
 * the engine refuses a write it will not make, with a sentence saying it is
 * not modelled in this prototype; and both are logged to the console naming
 * what was asked. A silent success would let a proposal look as if it worked.
 */

import type {
  AgentRow,
  EventRecord,
  InboxChange,
  OverlayRow,
  Frame,
  FeedRow,
  QueryMap,
  QueryName,
  Snapshot,
} from "~/protocol/types.ts";
import { MAX_EVENTS } from "~/contract/wire.ts";
import { copyCompany, documentOf, type DemoCompany } from "./company.ts";
import { identityOf } from "./iam.ts";
import { orgProjection, roster, scheduleRows } from "./derive.ts";
import { LEVELLED, servedLevel } from "./freshness.ts";
import { QUERIES } from "./queries.ts";
import { liveRollupOf, orgMeterOf, seatMeterOf } from "./spend.ts";
import { handleRest, type RestRequest, type RestReply } from "./rest.ts";
import { DemoRefusal, LOG } from "./refusal.ts";

export { DemoRefusal, LOG, notModelled } from "./refusal.ts";

/** What the engine pushes, before it is serialised onto the socket. */
export type Push = Pick<Frame, "kind"> & { data: unknown };

type Listener = (push: Push) => void;

export class DemoEngine {
  /** The company as it stands now. Every write replaces part of it. */
  company: DemoCompany;
  private readonly listeners = new Set<Listener>();
  private readonly changed = new Set<(company: DemoCompany) => void>();

  constructor(company: DemoCompany) {
    this.company = copyCompany(company);
  }

  /** The instant the engine reads as now. */
  now(): Date {
    return new Date();
  }

  // ---- the socket -------------------------------------------------------

  /** Hear every push. Returns the unsubscribe. */
  onPush(listener: Listener): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  /** Hear every write that changed the company, for whoever keeps it. */
  onChange(listener: (company: DemoCompany) => void): () => void {
    this.changed.add(listener);
    return () => this.changed.delete(listener);
  }

  /** Push one frame to every listener. */
  push(kind: Push["kind"], data: unknown): void {
    for (const listener of this.listeners) listener({ kind, data });
  }

  /** The handshake snapshot, built from the company as the engine builds it. */
  snapshot(): Snapshot {
    const c = this.company;
    const doc = documentOf(c);
    return {
      agents: this.agentRows(),
      events: c.events.slice(0, MAX_EVENTS).map(feedRow),
      sandboxes: c.runs.live,
      org: orgProjection(doc),
      tools: c.tools,
      health: this.health(),
      tokens: liveRollupOf(c, this.now()),
      budget: orgMeterOf(c, this.now(), 1),
      schedules: scheduleRows(doc, this.now()),
    };
  }

  /** Every agent seat's row: the roster's static half with the seat's live overlay. */
  agentRows(): AgentRow[] {
    const now = this.now();
    return roster(documentOf(this.company)).map((row) => {
      const handle = row.handle ?? row.id;
      return {
        ...row,
        ...(this.company.seats[handle]?.overlay ?? {}),
        budget: seatMeterOf(this.company, handle, now),
      };
    });
  }

  /** The `health` frame: the fixture's, with what the company's state decides filled in. */
  health() {
    const c = this.company;
    return {
      ...c.engine.health,
      applied_epoch: c.config.epoch,
      seats: Object.keys(c.seats),
      identity: identityOf(c),
    };
  }

  /**
   * Answer one question, as the engine's query registry would: a value, or a
   * [DemoRefusal] carrying one of the codes a screen branches on.
   *
   * EVERY REFUSAL IS SAID in the console, naming the question and its
   * parameters. A screen draws a refusal as an inline notice, which nothing
   * else hears, and a refused question is either a request the engine would
   * refuse too or one the demo does not model: worth knowing either way, and
   * what the route walk fails on.
   */
  answer<K extends QueryName>(what: K, params: Record<string, unknown>): QueryMap[K] {
    try {
      const handler = QUERIES[what] as
        ((engine: DemoEngine, p: Record<string, unknown>) => QueryMap[K]) | undefined;
      if (!handler) {
        throw new DemoRefusal(
          "unknown_query",
          `The question "${what}" is not modelled in this prototype.`,
        );
      }
      const answer = handler(this, params);
      // A state-log answer states the level it was served at, which is the read's own.
      return LEVELLED.has(what) ? { ...answer, read_level: servedLevel(params) } : answer;
    } catch (err) {
      if (err instanceof DemoRefusal) {
        console.warn(
          LOG,
          `The question "${what}" was refused ${err.code}: ${err.detail ?? ""}`,
          params,
        );
      }
      throw err;
    }
  }

  // ---- REST --------------------------------------------------------------

  /** Answer one REST request: a status, a body and the headers that matter. */
  rest(request: RestRequest): RestReply {
    return handleRest(this, request);
  }

  // ---- writes -------------------------------------------------------------

  /**
   * Record that a write changed the company: every keeper hears the new
   * state, so a variant switch opens on what the person just did. The REST
   * router calls it after every successful write.
   */
  committed(): void {
    for (const listener of this.changed) listener(this.company);
  }

  /** Push the slices a configuration change moves, in the order the engine pushes them. */
  pushConfiguration(): void {
    const doc = documentOf(this.company);
    this.push("org", orgProjection(doc));
    this.push("seats", roster(doc));
    this.push("agents", this.agentRows().map(overlayRow));
    this.push("schedules", { schedules: scheduleRows(doc, this.now()) });
    this.push("health", this.health());
  }

  /** Push one seat's overlay as the `agents` push carries it. */
  pushSeat(handle: string): void {
    const row = this.agentRows().find((r) => r.handle === handle);
    if (row) this.push("agents", [overlayRow(row)]);
  }

  /**
   * Say that one committed batch moved `handle`'s inbox: identifiers and a
   * count, never content. Only a socket WATCHING that seat is sent it, which
   * [DemoSocket] decides as the engine's hub does.
   */
  pushInboxChanged(handle: string, change: Omit<InboxChange, "handle">): void {
    this.push("inbox_changed", { handle, ...change });
  }

  /** Add an event to the feed, newest first, and push it. */
  publish(event: EventRecord): void {
    this.company.events.unshift(event);
    this.push("event", { ...event, failed: event.failed ?? false });
  }
}

/**
 * A seat's row as an `agents` push carries it: its overlay and the AGENT ID
 * it is merged onto the roster by, and nothing else that names the seat.
 */
export function overlayRow(row: AgentRow): OverlayRow {
  const { id: _id, role: _role, handle: _handle, ...overlay } = row;
  return overlay as OverlayRow;
}

/** A stored event as the feed carries it: everything but the payload. */
export function feedRow(event: EventRecord): FeedRow {
  const { payload: _payload, work_key: _key, ...row } = event;
  const out: FeedRow = { ...row, failed: event.failed ?? false };
  const agentId = event.tags?.agent_id;
  if (agentId) out.agent_id = agentId;
  return out;
}
