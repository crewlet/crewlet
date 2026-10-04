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

// RELATIVE, not through `~`: protocol/ is also built alone as protocol.js,
// where the alias does not exist.
import { SESSION_QUERIES } from "../contract/domains.ts";

/** A log a write lands in and a read can wait on. */
export type SessionDomain = keyof typeof SESSION_QUERIES;

/** A position as the engine spells it: `<stream>@<generation>:<sequence>`. */
export interface LogPosition {
  stream: string;
  generation: number;
  seq: number;
}

/**
 * A position off the wire, or null for one this client cannot read.
 *
 * THE ENGINE'S OWN GRAMMAR (`tracker.ParseLogPosition`), so the value handed
 * back as `min_position` is one the engine will accept: a stream name, then a
 * generation and a sequence that are both non-negative integers.
 */
export function parsePosition(raw: string | null | undefined): LogPosition | null {
  const match = /^([^@\s]+)@(\d+):(\d+)$/.exec(raw ?? "");
  if (!match) return null;
  const generation = Number(match[2]);
  const seq = Number(match[3]);
  if (!Number.isSafeInteger(generation) || !Number.isSafeInteger(seq)) return null;
  return { stream: match[1]!, generation, seq };
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
export function isLater(next: LogPosition, held: LogPosition): boolean {
  if (next.stream !== held.stream) return true;
  if (next.generation !== held.generation) return next.generation > held.generation;
  return next.seq > held.seq;
}

/** Heard when a domain was written from this tab: its floor rose. */
export type WrittenListener = (domain: SessionDomain | null, refreshes: readonly string[]) => void;

/**
 * One tab's floors. A class rather than module state so a suite can hold its
 * own; the dashboard uses [session], the one per tab.
 */
export class SessionFloors {
  private readonly floors = new Map<SessionDomain, { raw: string; at: LogPosition }>();
  private readonly listeners = new Set<WrittenListener>();

  /** The floor a read of `domain` names, or null before this tab wrote to it. */
  floor(domain: SessionDomain): string | null {
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
  written(
    domain: SessionDomain | null,
    position: string | null,
    refreshes: readonly string[],
  ): boolean {
    let rose = false;
    const at = parsePosition(position);
    if (domain !== null && at && position) {
      const held = this.floors.get(domain);
      if (!held || isLater(at, held.at)) {
        this.floors.set(domain, { raw: position, at });
        rose = true;
      }
    }
    for (const listener of this.listeners) listener(domain, refreshes);
    return rose;
  }

  /** Subscribe to writes. Returns the unsubscribe. */
  onWritten(listener: WrittenListener): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  /**
   * Whether a write heard by a listener moves the answer to `kind`: a question
   * that reads the written domain at a floor, or one the write names as
   * moving beside it.
   */
  static moves(kind: string, domain: SessionDomain | null, refreshes: readonly string[]): boolean {
    return (domain !== null && domainOf(kind) === domain) || refreshes.includes(kind);
  }

  /**
   * The freshness a read of `kind` names, or null when it names none: the
   * question takes no floor, or this tab has not written its domain.
   */
  freshness(kind: string): { read_level: "session"; min_position: string } | null {
    const domain = domainOf(kind);
    if (domain === null) return null;
    const floor = this.floor(domain);
    return floor === null ? null : { read_level: "session", min_position: floor };
  }
}

/** The domain whose log a question reads at a floor, or null for one that takes none. */
export function domainOf(kind: string): SessionDomain | null {
  for (const domain of Object.keys(SESSION_QUERIES) as SessionDomain[]) {
    if ((SESSION_QUERIES[domain] as readonly string[]).includes(kind)) return domain;
  }
  return null;
}

/** This tab's floors. */
export const session = new SessionFloors();
