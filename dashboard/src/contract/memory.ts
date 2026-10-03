/**
 * A seat's memory, as the `agent_memory` answer sends it: what it chose to
 * remember, what it did, the skills it drafted, and what it learned about the
 * people it works with.
 *
 * EXACTLY WHAT `internal/api/queries` SENDS, and held there by
 * `TestTheMemoryScreenReadsWhatThisAnswerSends` in both directions: every
 * member declared here is a key the answer carries, and every key it carries
 * is declared. These types had drifted with no gate at all — a diary entry
 * declared a `scope` and `tags` the engine never sent, an episode an
 * `outcome` and a `content`, a skill a `body` — so the memory tab's
 * fallbacks read fields that were always undefined, while the fields the
 * engine did send (an entry's `source` and `retrievals`, an episode's plan,
 * tool sequence and work key) were invisible to every screen.
 *
 * Every row type is declared once and composed into [AgentMemory], which is
 * the name a screen reads. The diary, episode, skill and subject types are
 * not exported, because nothing reads them on their own; a counterparty
 * profile is, because the seat page draws one per row.
 */

import type { Coverage } from "./coverage.ts";

/** One diary entry: something the seat chose to remember. */
interface DiaryEntry {
  id: string;
  content: string;
  /** `diary_long` or `diary_short` — whether it lapses. */
  retention: string;
  /** What wrote it: a worker of the learning loop (`persist_decider`), or
   *  the seat's own tool call (`tool:reflect_and_persist`) — what the seat
   *  chose to keep, told apart from what a worker decided for it. */
  source: string;
  /** The turn that wrote it, or "" when none did. */
  turn_id: string;
  created_at: string;
  /** When a short entry lapses, or "" for a long one, which never does. */
  ttl_until: string;
  /** How often it has been recalled since — the difference between a memory
   *  that keeps proving useful and one written once and never read. */
  retrievals: number;
}

/** One episode: a completed turn, summarised. */
interface Episode {
  id: string;
  turn_id: string;
  agent_handle: string;
  task_summary: string;
  plan_summary: string;
  /** "" when the turn ended without a review outcome. */
  review_outcome: string;
  /** Null when the turn recorded none. */
  tool_sequence: string[] | null;
  skills_used: string[] | null;
  conversation_key: string;
  work_key: string;
  created_at: string;
  ended_at: string;
  /** Milliseconds: a Go duration marshals as nanoseconds, which renders as a
   *  plausible and wildly wrong number. */
  duration_ms: number;
  /** A compacted row stands for a cluster of turns rather than one. */
  compacted: boolean;
  count: number;
}

/** One skill the seat drafted from its own repeated work. */
interface SynthesizedSkill {
  id: string;
  key: string;
  title: string;
  summary: string;
  version: number;
  updated_at: string;
  uses: number;
}

/** Who a counterparty IS, which is two identities and not one string.
 *
 *  A profile's subject is either a seat in this company or an unmapped person
 *  on some surface — a chat account, an issue reporter — and the name is
 *  DISPLAY ONLY: somebody renaming themselves on Slack must not orphan what
 *  this seat learned about them. */
interface CounterpartySubject {
  handle?: string;
  external_id?: string;
  platform?: string;
  name: string;
}

/** What one seat has learned about one colleague.
 *
 *  THE TWO INSTANTS MEASURE DIFFERENT CADENCES and both are carried, because
 *  the difference is the interesting one: `last_updated_at` moves on every
 *  interaction and `last_corroborated_at` only when the traits actually
 *  changed. A colleague seen daily whose profile has not moved in months is
 *  one this seat has stopped learning about — which is the state the Plan
 *  phase's own prefetch demotes on, so a screen showing one number would
 *  disagree with the prompt. */
export interface CounterpartyProfile {
  subject: CounterpartySubject;
  /** Whether the subject resolved to a seat in this company. */
  resolved: boolean;
  /** A bag whose keys the model invents — never a fixed schema. */
  traits: Record<string, unknown>;
  interactions: number;
  first_seen_at: string;
  last_updated_at: string;
  last_corroborated_at: string;
}

export interface AgentMemory {
  /** The handle the answer is about. */
  id: string;
  /** The newest LIVE entries, newest first — a page of `diary_total`. */
  diary: DiaryEntry[];
  diary_total: number;
  /** The newest episode rows — a page of `episodes_total`. */
  episodes: Episode[];
  episodes_total: number;
  skills: SynthesizedSkill[];
  /** How many the seat HAS, which is not how many `skills` carries: the
   *  listing is cut at the page limit, and this is what says so. ALWAYS
   *  PRESENT — `0` for a seat that has learned nothing rather than absent.
   *  Read off `skills.length` instead, it is the page size, which silently
   *  under-reports exactly the seat this count exists for. The same holds
   *  for every `…_total` here. */
  skills_total: number;
  /** Most recently updated first — a page of `counterparties_total`. */
  counterparties: CounterpartyProfile[];
  counterparties_total: number;
  /** The newest live diary entry, whatever page was asked for — what a
   *  profile's summary shows as the latest reflection — or null when the
   *  diary holds none. */
  latest_reflection: DiaryEntry | null;
  /** When the seat first finished onboarding, or "" when it has not. */
  onboarded_at: string;
  /** Who answered: the node holding the seat, whose copy is the one kept
   *  current — or `none` for a seat no node holds, whose answer is empty
   *  because no copy anywhere is current. */
  held_by: string;
}

/** One agent seat's row of `memory_overview`: its totals and newest note,
 *  counted by the node holding it. */
export interface MemoryOverviewSeat {
  /** The seat's agent id, which a row is matched by; `handle` is the handle it
   *  answers to now, which a list draws. */
  agent_id: string;
  handle: string;
  diary_total: number;
  episodes_total: number;
  skills_total: number;
  /** When the newest live diary entry was written, or "" with none. */
  last_reflection_at: string;
  /** That entry, or null. */
  latest_reflection: DiaryEntry | null;
  /** The node holding the seat, or `none` — then nothing is counted,
   *  because no copy anywhere is current. */
  held_by: string;
  /** Why the holder's count is NOT here — it did not answer, runs a build
   *  that cannot, or is still taking the seat — or "" when it is. A row
   *  with a reason carries zeros that are not a count; never draw them as
   *  one. */
  unavailable: string;
}

/** `memory_overview`: EVERY agent seat in the chart, in handle order — no
 *  cap — with the fleet `coverage` naming each holder that was asked. */
export interface MemoryOverview {
  seats: MemoryOverviewSeat[];
  coverage: Coverage;
}
