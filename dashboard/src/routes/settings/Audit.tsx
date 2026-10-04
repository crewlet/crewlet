/**
 * What the operators of this company did to it.
 *
 * # Why it is one screen and not six
 *
 * Every write in this product is attributed — the tracker records the actor
 * and their kind on every commit, the knowledge base does the same, a config
 * revision names who created it, a credential row names who last stored it,
 * and the identity estate keeps its own trail of who enrolled, signed in and
 * changed whom. Five subsystems, five honest records, and until now five places
 * to look: a question as ordinary as "what did we change last Tuesday" meant
 * reading the tracker's feed, the wiki's feed, the config history, the
 * credential table and the directory's trail, each in its own chrome, each
 * ordered differently.
 *
 * So this is the one screen that reads them together, in one order, over one
 * window. It writes nothing and asks no new question of the engine.
 *
 * # It is about WHO WAS WRITING, not about a list of people
 *
 * The two feeds narrow on `actor_kinds` rather than on a set of handles, and
 * that is the whole design. Every record names its author the one way
 * `iam.ActorFor` does: a person the identity directory binds to a seat writes
 * AS that seat, kind `human`; a credential bound to none — a person with no
 * seat, a machine, a Tier A token — writes under its WHOLE login, kind
 * `operator`; a seat in its own turn is kind `agent`; and the engine is
 * `system`. A login always carries a separator a seat handle cannot, so the
 * name spaces are disjoint — and the set of people is the roster, which
 * changes. An audit assembled from handles would quietly lose every commit
 * made by somebody who has since left the company, which is the one commit
 * somebody is looking for.
 *
 * The kinds are held apart and LABELLED rather than filtered down to one,
 * because "the engine did this" is as much an answer as "a person did": a bulk
 * edit nobody asked for is the thing an operator opens an audit to find.
 *
 * # And THROUGH WHICH CREDENTIAL
 *
 * A machine token acts as its owner, and a browser session is one of a
 * person's several, so the author alone reads a token's write as one its owner
 * made by hand. Every source records the credential beside the author
 * (`operator_id`: `pat:<id>`, `session:<lineage>`), and the Through column says
 * it wherever it names something the author does not ([throughOf]).
 *
 * # A config revision says who wrote it, and this screen believes it
 *
 * Every revision records its writer's KIND beside the name, and the pointer a
 * peer adopts from carries both — so this row reads the same on every node. It
 * used to be drawn as an operator's unconditionally, which labelled a node's
 * boot seed and the reconcile loop's reload after sealing a credential as a
 * person's write. The configuration is the one source read WHOLE rather than
 * narrowed to people: a revision the engine made is a change to the company as
 * much as one a person made, and the row's kind says which it was. A revision
 * whose writer nobody recorded says exactly that.
 *
 * # The fifth source: what a person did at runtime
 *
 * Four subsystems keep a record of the CHANGE a write made. None keeps one of
 * the CALL: a tool call that was refused, one whose answer never came back, and
 * a verb that changes no tracker or wiki record at all — a backup — left
 * nothing anywhere. The runtime audit is that record (`operator_acted` and
 * `backup_requested`, each with the envelope source `operator`), and it is read
 * here as `Runtime`: every call, whatever became of it, under the author and
 * kind the engine recorded (`actor`, and the `actor_kind` and `operator_id`
 * tags) and the node that served it. The arguments are never in it — they are
 * the company's content, and they already live in the history of whatever they
 * changed.
 *
 * # The sixth source: the identity estate's own trail
 *
 * Who invited whom, who signed in and when, whose grants changed, which
 * credential was revoked — `GET /iam/audit`, the directory's trail, behind the
 * same `audit:read` grant this screen takes. Its author is named the same way
 * every other record's is; its KIND is the record's own word, the principal's
 * (`person`, `machine`, `seat`, `engine`), because that is what the identity
 * estate records and redrawing it in another vocabulary would claim a reading
 * the record does not make.
 *
 * # What each source can and cannot be asked
 *
 * The tracker's feed and the runtime audit take a wall-clock window, and the
 * identity trail one instant it resolves to a log position; the wiki's pages
 * on a LOG POSITION, and the config and credential reads take neither. So those three are fetched as their newest page and narrowed to the
 * window HERE — and the screen says so rather than implying its window is the
 * engine's. A row count from a client-side narrowing is a count over what was
 * loaded, never over what exists, which is the rule every list in this product
 * is held to. And a windowed page can fill too: a busy week reaches further
 * back than one page, and that is said as well.
 */

import { useMemo, useRef } from "react";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { Callout, Card, EmptyValue, Input, Select, Skeleton, Tag } from "@crewlethq/ui";
import { FileTextGlyph, SearchGlyph } from "@crewlethq/icons/glyphs";

import { href } from "~/app/router.tsx";
import { useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { DateCell, SeatCell, SeatLabel, TextCell } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { DownloadButton } from "~/ui/primitives.tsx";
import { toCsv } from "~/lib/csv.ts";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { plainText } from "~/lib/markdown.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { OPERATOR_SOURCE, RUNTIME_AUDIT_TYPES } from "~/contract/audit.ts";
import { useNow } from "~/lib/clock.ts";
import { indexOrg, kindOfAuthor, type SeatKind } from "~/lib/seats.ts";
import { useTimeRange, type Offer } from "~/lib/range.ts";
import { rest, RestError } from "~/protocol/index.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { needsSentence } from "~/lib/refusal.ts";
import { throughOf } from "~/lib/attribution.ts";
import type { FeedRow, SecretRow, WorkActivityRecord } from "~/protocol/index.ts";

/**
 * The window this screen offers.
 *
 * NO `15m`, and a fallback of a week. An audit is read after the fact — "what
 * happened while I was away" — where the event log's own question is "what is
 * happening now", so the two offers differ in both directions: this one starts
 * wider and does not bother with the quarter-hour.
 */
const AUDIT_OFFER: Offer = {
  ranges: ["1d", "7d", "30d", "90d"],
  custom: true,
  fallback: "7d",
  buckets: ["hour", "day"],
};

/**
 * How many rows each source contributes at most.
 *
 * ONE PAGE EACH, not a paging loop. The three sources that cannot be windowed
 * server-side are narrowed here, so a bigger page buys a longer window and
 * nothing else — and a loop that chased cursors until the window was covered
 * would make the cost of opening this screen a property of how busy the
 * company has been, with no bound a reader could see. 200 is the tracker
 * feed's own maximum page (`tracker.MaxActivityRows`); the runtime audit and
 * the identity trail are asked for the same so the three windowed sources
 * reach equally far back, and the others are sized to it: the footer says what
 * was loaded, and a window wider than the page reaches is reported rather than
 * silently cut.
 */
const PAGE = { work: 200, pages: 100, config: 50, runtime: 200, identity: 200 } as const;

/** How often the feeds are re-read. An audit is read, not watched. */
const POLL_MS = 60_000;

/** The places a company's own writes are recorded, the runtime audit of every
 *  call a person made, and the identity estate's own trail. */
const SOURCES = ["work", "knowledge", "config", "credentials", "runtime", "identity"] as const;
type Source = (typeof SOURCES)[number];

const SOURCE_LABEL: Record<Source, string> = {
  work: "Work",
  knowledge: "Knowledge",
  config: "Configuration",
  credentials: "Credentials",
  runtime: "Runtime",
  identity: "Identity",
};

type RuntimeType = (typeof RUNTIME_AUDIT_TYPES)[number];

/**
 * What one runtime audit row says was done, and to what — keyed on the
 * engine's own list of runtime audit types, so a record the engine adds is a
 * compile error here rather than a row labelled with nothing.
 */
const RUNTIME_ROW: Record<
  RuntimeType,
  (row: FeedRow) => Pick<AuditEntry, "kind" | "subject" | "path" | "noSubject">
> = {
  // A TOOL CALL NAMES ITS TOOL AND NOTHING IT WAS CALLED WITH: the arguments
  // are the company's content, which lives in the history of whatever the call
  // changed, never in the audit — so the empty cell says exactly that.
  operator_acted: (row) => ({
    kind: row.tags?.tool ?? "tool call",
    subject: "",
    noSubject: "Not recorded",
  }),
  // A BACKUP'S SUBJECT IS WHERE IT WENT — a directory on the row's own node.
  // A row with no directory tag is a fact about the row, not a policy about
  // arguments, and its summary still names the directory beside it.
  backup_requested: (row) => ({
    kind: "backup",
    subject: row.tags?.dir ?? "",
    path: ["settings", "backups"],
    noSubject: "No directory recorded",
  }),
};

function isRuntimeType(type: string): type is RuntimeType {
  return (RUNTIME_AUDIT_TYPES as readonly string[]).includes(type);
}

/**
 * One entry of the identity estate's trail, as `GET /iam/audit` answers it
 * (`iamapi.auditView`).
 *
 * DECLARED HERE, beside its one reader, rather than in `~/contract`: no engine
 * test reads it.
 */
export interface IdentityAuditEntry {
  id: string;
  /** `change` or `session`. */
  class: string;
  /** What the entry is about: `person`, `login`, `email`, `seat`, `session`, … */
  object_kind: string;
  object_id?: string;
  /** The person it concerns, by id. */
  person?: string;
  /** What was done: `invite`, `enrol`, `update`, `revoke`, `open`, `close`, … */
  op: string;
  actor?: string;
  /** The PRINCIPAL's kind — `person`, `machine`, `seat`, `engine`. */
  actor_kind?: string;
  operator_id?: string;
  reason?: string;
  summary?: string;
  at?: string;
  position: number;
}

/** One page of the identity trail, newest first, and where the next starts. */
interface IdentityAuditPage {
  events?: IdentityAuditEntry[];
  /** The position to page back from, zero at the end. */
  next?: number;
  position?: string;
}

/**
 * One recorded action, whichever subsystem recorded it.
 *
 * A SHAPE THIS SCREEN OWNS, deliberately, rather than a union of six wire
 * types: the whole point is that they are read together, and a grid whose
 * every cell begins by asking which of six kinds of row it is holding is a
 * grid with six renderings in it.
 */
export interface AuditEntry {
  id: string;
  at: string;
  source: Source;
  /** What was done, in the subsystem's own word. */
  kind: string;
  /** The seat handle or login recorded on the write. */
  actor: string;
  /**
   * The record SAYS NOTHING about who wrote this, which is different from
   * "the engine wrote it" — the reading an empty actor gets everywhere else.
   * A config revision is the one row that can carry it.
   */
  unrecorded?: true;
  /**
   * The kind the record names its author with — `agent`, `human`,
   * `operator`, `system`, or on the identity trail the principal's own
   * `person`, `machine`, `seat`, `engine` — empty where none was recorded.
   */
  actorKind: string;
  /**
   * The credential the write was made through, where it names something the
   * actor does not — a person's machine token or browser session — and empty
   * otherwise ([throughOf]).
   */
  through: string;
  /** What it was done to, as a person would name it. */
  subject: string;
  /** Where that object lives, where it still has an address. */
  path?: string[];
  /**
   * WHY `subject` IS EMPTY, said in the To cell, where the row's source has
   * a reason — a tool call's arguments are never recorded, which is a
   * different fact from a record that simply named nothing. Absent, an empty
   * subject reads "None recorded".
   */
  noSubject?: string;
  /** The one line that says what actually changed. */
  detail: string;
  /** The node that served the call — the runtime audit's alone. */
  node?: string;
  /** The call was refused or failed, which only the runtime audit records. */
  failed?: true;
}

/**
 * WHAT A TRACKER COMMIT WAS ABOUT, as a person would name it.
 *
 * A FEED OF UUIDS IS A FEED NOBODY READS, which is the tracker's own rule
 * about its own history — and this screen broke it the moment it showed
 * anything but a task. A task carries a key and a project and a person carry
 * their own names, so those three read fine; a SAVED VIEW is addressed by
 * uuid, so five rows in a row read `6dd4b0df-f455-448e-80e9-…` with nothing
 * saying what they were.
 *
 * So a subject with no key is named by its KIND and linked to the page that
 * holds it, with the id shortened to the part a person would actually use to
 * tell two apart. The id is kept because it is what a reader pastes into a
 * tool call; it is not what they read the row by.
 */
function workSubject(record: WorkActivityRecord): Pick<AuditEntry, "subject" | "path"> {
  const kind = record.subject_kind;
  const id = record.subject_id;
  if (record.subject_key) {
    return {
      subject: record.subject_key,
      // A PURGED TASK HAS NO PAGE. Its rows are destroyed and this entry is
      // the only evidence it existed, so a link here would be a NotFound on
      // the one row a reader most wants to follow.
      path: record.kind === "purged" ? undefined : ["work", record.subject_key],
    };
  }
  switch (kind) {
    case "view":
      return { subject: `view ${short(id)}`, path: ["work", "views", id] };
    case "person":
      return { subject: id, path: ["agents", "seats", id] };
    case "project":
      return { subject: id, path: ["work", id] };
    default:
      // EVERY OTHER SUBJECT KIND HAS NO PAGE — a counter, a catalogue, a
      // tag set, an alias. Named by its kind rather than linked,
      // because a link to a screen the product does not have is worse than
      // none: the row still says what was changed.
      return { subject: kind ? `${kind} ${short(id)}` : short(id) };
  }
}

/**
 * WHAT AN IDENTITY ENTRY WAS ABOUT. A login and a seat are names a person
 * reads; a person, a session and an address are an id, a lineage and a blind,
 * so they are named by their kind with the id shortened — the email above all,
 * whose id is a keyed blind that says nothing and must never be read as the
 * address. Every one is the People & access screen's to show.
 */
function identitySubject(entry: IdentityAuditEntry): Pick<AuditEntry, "subject" | "path"> {
  const id = entry.object_id ?? "";
  const path = ["settings", "people"];
  switch (entry.object_kind) {
    case "login":
    case "seat":
      return { subject: id, path };
    case "email":
      return { subject: "an address", path };
    default:
      return {
        subject: id ? `${entry.object_kind} ${short(id)}` : entry.object_kind,
        path,
      };
  }
}

/** A case-blind substring match. */
function includes(value: string, needle: string): boolean {
  return value.toLowerCase().includes(needle.toLowerCase());
}

/** The leading segment of a uuid, which is what tells two of them apart. */
function short(id: string): string {
  return id.length > 8 && id.includes("-") ? id.slice(0, 8) : id;
}

/**
 * A source's rows, or none at all.
 *
 * SIX SOURCES AND ONE BAD ANSWER MUST NOT COST THE OTHER FIVE. This screen is
 * the only one that reads six subsystems at once, so what is a blank page
 * anywhere else is five subsystems' worth of history lost here — and the
 * failure mode is not hypothetical: `config_audit` is the one question in this
 * set that answers a BARE ARRAY rather than an object, so `?? []` covers a
 * null and does not cover anything else, and iterating whatever arrived is a
 * throw that takes the whole screen with it.
 *
 * It is not a guard against the engine. It is a guard against the SET: a
 * screen that composes six independent reads has six chances at that throw,
 * and an audit is the last screen that should go blank.
 */
function list<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : [];
}

/**
 * Who a row's writer IS, as the Who column draws them.
 *
 * A LOGIN IS NOT A SEAT. An `operator` write carries a credential's whole
 * login as its actor — a person bound to no seat, a machine, `token:<id>` —
 * and the chart has no seat by that name, so a cell that looked it up drew the
 * chart's default, the agent's squircle, and linked it to a seat page that
 * does not exist. So:
 *
 *  - a seat's write — an agent in its turn, or a person writing AS the seat
 *    the directory binds them to — draws that seat, linking to it, with the
 *    chart's kind or, where the chart no longer holds the seat, the kind the
 *    write was recorded under;
 *  - a login draws the human circle and its name as PLAIN TEXT, since there is
 *    no page to link to;
 *  - `system` names a duty rather than anybody, and an empty actor is the
 *    engine itself.
 *
 * The identity trail's principal kinds read the same way: `machine` is a
 * login, `engine` the system, `seat` a seat, and a `person` is their seat
 * where the chart holds one by that handle and their login where it does not.
 */
export type Writer =
  | { as: "seat"; handle: string; name: string; kind?: SeatKind }
  | { as: "login"; name: string }
  | { as: "system"; name: string }
  | { as: "engine" }
  | { as: "unrecorded" };

/** A seat by its handle, as the Who cell draws it — or null where the chart holds none. */
export type SeatOf = (handle: string) => { name: string; kind?: SeatKind } | null;

export function writerOf(
  row: Pick<AuditEntry, "actor" | "actorKind" | "unrecorded">,
  seatOf: SeatOf,
): Writer {
  if (row.unrecorded) return { as: "unrecorded" };
  if (!row.actor) return { as: "engine" };
  switch (row.actorKind) {
    case "operator":
    case "machine":
      return { as: "login", name: row.actor };
    case "system":
    case "engine":
      // THE ENGINE, NAMED: a config revision the engine wrote carries the
      // node that seeded it or the loop that made it, and neither is a seat
      // to link to or a person to draw.
      return { as: "system", name: row.actor };
  }
  const seat = seatOf(row.actor);
  if (!seat && row.actorKind === "person") return { as: "login", name: row.actor };
  return {
    as: "seat",
    handle: row.actor,
    name: seat?.name ?? row.actor,
    kind: seat?.kind ?? kindOfAuthor(row.actorKind === "seat" ? "agent" : row.actorKind),
  };
}

/**
 * THE CHART'S ANSWER ABOUT A HANDLE — the seat, or none — since the only thing
 * a screen naming a writer asks of the chart is how to draw them. One hook, so
 * every surface that names who did something (this trail, a backup's history)
 * draws the same writer the same way.
 */
export function useSeatOf(): SeatOf {
  const org = useOrg();
  return useMemo<SeatOf>(() => {
    const index = indexOrg(org);
    return (handle) => {
      const seat = index.byHandle.get(handle);
      return seat ? { name: seat.name, kind: seat.kind } : null;
    };
  }, [org]);
}

/** The Who cell: one [Writer], drawn. */
export function WriterCell({ writer }: { writer: Writer }) {
  switch (writer.as) {
    case "seat":
      return <SeatCell handle={writer.handle} name={writer.name} kind={writer.kind} />;
    case "login":
      return <SeatLabel name={writer.name} kind="human" />;
    case "system":
      return <TextCell>{writer.name}</TextCell>;
    case "engine":
      // THE ENGINE IS A WRITER. A chart apply and a repair duty carry no
      // actor at all, and rendering them as a blank would make the five
      // writers with no tool invisible on the one screen that exists to name
      // every writer.
      return <span className="muted">the engine</span>;
    case "unrecorded":
      // NOT THE ENGINE, AND NOT ANYBODY: the record does not say, and the
      // cell says that rather than guessing.
      return <EmptyValue label="Not recorded" />;
  }
}

/**
 * WHOSE WRITES COUNT AS AN OPERATOR'S.
 *
 * A person and a credential acting for the company are the two kinds an audit
 * is about, and they are DIFFERENT kinds rather than two spellings of one: a
 * person bound to a seat writes as the seat, kind `human`, and a credential
 * bound to none writes under its own login, kind `operator`, precisely so that
 * the distinction survives. Both are asked for, and the rows say which.
 */
const OPERATOR_KINDS = "operator,human";

export function Audit() {
  // `/` FOCUSES THIS SCREEN'S SEARCH rather than opening the palette over it.
  const searchBox = useRef<HTMLInputElement>(null);
  useSearchTarget(searchBox);
  const now = useNow();
  const seatOf = useSeatOf();
  const range = useTimeRange(now, AUDIT_OFFER, false);
  const { since, until } = range;
  const [actor, setActor] = useParam("actor", "");
  const [kind, setKind] = useParam("kind", "");

  // THE ONE SOURCE THAT TAKES THE WINDOW. `from` and `to` bound the AUTHORED
  // instants, which is what somebody typing "last week" means (D113).
  const work = useQuery(
    "work_activity",
    {
      container: "workspace",
      actor_kinds: OPERATOR_KINDS,
      from: since,
      to: until,
      limit: PAGE.work,
    },
    { pollMs: POLL_MS },
  );
  // AND THE THREE THAT DO NOT. `page_activity` bounds on a log POSITION
  // rather than a clock, and neither the config history nor the credential
  // table has a window at all — so each is asked for its newest page and
  // narrowed below, with `truncated` saying when that page did not reach back
  // as far as the window does.
  const knowledge = useQuery(
    "page_activity",
    { actor_kinds: OPERATOR_KINDS, limit: PAGE.pages },
    { pollMs: POLL_MS },
  );
  const config = useQuery("config_audit", { limit: PAGE.config }, { pollMs: POLL_MS });
  // THE RUNTIME AUDIT, windowed by the engine like the tracker's feed: the
  // event log narrowed to the source every runtime audit event carries.
  const runtime = useQuery(
    "events",
    { source: OPERATOR_SOURCE, since, until, limit: PAGE.runtime },
    { pollMs: POLL_MS },
  );
  const secrets = useSecrets();
  const identity = useIdentityTrail(since);
  // The first of the socket's reads that failed, whose code and refusal the
  // banner shows together. The two REST reads are best effort, and each says
  // what it withheld in a sentence of its own.
  const failed = [work, knowledge, config, runtime].find((read) => read.error !== null);

  const rows = useMemo<AuditEntry[]>(() => {
    const out: AuditEntry[] = [];
    for (const record of list(work.data?.records)) {
      out.push({
        id: `work:${record.id}`,
        at: record.at,
        source: "work",
        kind: record.kind,
        actor: record.actor ?? "",
        actorKind: record.actor_kind ?? "",
        through: throughOf(record.actor ?? "", record.operator_id),
        ...workSubject(record),
        // FLATTENED AT THE ROW, so the grid cell and `auditCsv` cannot differ.
        // It also keeps a body's newlines out of a CSV field, where they are
        // legal inside quotes and unreadable in every spreadsheet.
        detail: plainText(record.excerpt ?? ""),
      });
    }
    for (const change of list(knowledge.data?.changes)) {
      out.push({
        id: `page:${change.id}`,
        at: change.at,
        source: "knowledge",
        kind: change.kind,
        actor: change.actor ?? "",
        actorKind: change.actor_kind ?? "",
        through: throughOf(change.actor ?? "", change.operator_id),
        subject: change.title || change.page_id,
        // A PURGED PAGE HAS NO TITLE, and no page to open: its entry is the
        // record it ever existed.
        path: change.title ? ["knowledge", "pages", change.page_id] : undefined,
        detail: plainText(change.excerpt ?? ""),
      });
    }
    for (const revision of list(config.data)) {
      out.push({
        id: `config:${revision.revision_id}`,
        at: revision.created_at,
        source: "config",
        // THE SOURCE IS THE KIND HERE — `dashboard`, `cli`, `setup` — because
        // "a revision was created" is the only thing that ever happens to the
        // config history, so the word that distinguishes two rows is how it
        // was created rather than what was done.
        kind: revision.source || "revision",
        actor: revision.created_by ?? "",
        // THE REVISION'S OWN WORD for what wrote it. This was the literal
        // "operator", so a node's seed and the reconcile loop's reloads were
        // drawn as a person's writes.
        actorKind: revision.created_by_kind ?? "",
        ...(!revision.created_by_kind && !revision.created_by ? { unrecorded: true as const } : {}),
        through: throughOf(revision.created_by ?? "", revision.operator_id),
        subject: revision.revision_id.slice(0, 8),
        path: ["settings", "config", "revisions", revision.revision_id],
        detail: revision.summary ?? "",
      });
    }
    for (const row of list(secrets.rows)) {
      out.push({
        id: `secret:${row.name}`,
        at: row.updated_at,
        source: "credentials",
        // THE LAST WRITE ONLY, and the row says so. `/secrets` answers the
        // CURRENT state of each name — there is no history of a credential,
        // deliberately, because a history of writes to a secret is a map of
        // when it was weakest. So this is one entry per name, at the instant
        // it was last stored.
        kind: "stored",
        actor: row.updated_by ?? "",
        // THE RECORDED KIND, where there is one, never a guess: a person bound
        // to a seat and a Tier A token read alike under one.
        actorKind: row.updated_by_kind ?? "",
        through: throughOf(row.updated_by ?? "", row.operator_id),
        subject: row.name,
        path: ["settings", "secrets"],
        detail: row.source ? `from ${row.source}` : "",
      });
    }
    for (const event of list(runtime.data?.events)) {
      // A TYPE THIS BUILD DOES NOT KNOW is a newer node's record: drawn under
      // its own type name rather than dropped, because an audit that loses
      // a row during an upgrade is the wrong way round.
      const what = isRuntimeType(event.type)
        ? RUNTIME_ROW[event.type](event)
        : { kind: event.type, subject: "", noSubject: "None recorded" };
      out.push({
        id: `runtime:${event.id}`,
        at: event.timestamp,
        source: "runtime",
        ...what,
        actor: event.actor ?? "",
        // THE KIND AND THE CREDENTIAL THE ENGINE RECORDED, off the row's
        // promoted tags: a call is an operator's only where its author was
        // bound to no seat.
        actorKind: event.tags?.actor_kind ?? "",
        through: throughOf(event.actor ?? "", event.tags?.operator_id),
        ...(event.tags?.node ? { node: event.tags.node } : {}),
        ...(event.failed ? { failed: true as const } : {}),
        detail: event.summary ?? "",
      });
    }
    for (const entry of list(identity.page?.events)) {
      out.push({
        id: `identity:${entry.id}`,
        at: entry.at ?? "",
        source: "identity",
        kind: entry.op,
        actor: entry.actor ?? "",
        actorKind: entry.actor_kind ?? "",
        through: throughOf(entry.actor ?? "", entry.operator_id),
        ...identitySubject(entry),
        detail: entry.summary || entry.reason || "",
      });
    }
    return out;
  }, [work.data, knowledge.data, config.data, secrets.rows, runtime.data, identity.page]);

  /** Newest first, narrowed to the window and to what the reader asked. */
  const shown = useMemo(() => {
    const from = Date.parse(since);
    const to = Date.parse(until);
    return rows
      .filter((row) => {
        const at = Date.parse(row.at);
        if (Number.isNaN(at) || at < from || at > to) return false;
        if (kind && row.source !== kind) return false;
        // THE NAME THE ROW DRAWS, or the credential beside it: a reader
        // looking for one of somebody's tokens types the token.
        if (actor && ![row.actor, row.through].some((name) => includes(name, actor))) return false;
        return true;
      })
      .sort((a, b) => Date.parse(b.at) - Date.parse(a.at));
  }, [rows, since, until, kind, actor]);

  // A PAGE THAT DID NOT REACH BACK AS FAR AS THE WINDOW. The three sources
  // narrowed here are asked for their newest N, so a busy company's window can
  // extend past the oldest row that arrived — and a screen that said nothing
  // would be claiming those rows do not exist.
  const truncated = useMemo(() => {
    const short: string[] = [];
    const from = Date.parse(since);
    const oldest = (list: { at: string }[], page: number, label: string) => {
      if (list.length < page) return;
      const last = list[list.length - 1];
      if (last && Date.parse(last.at) > from) short.push(label);
    };
    // A WINDOWED PAGE CAN FILL TOO. Every row it holds is inside the window,
    // so a full one whose oldest row is still after the window's start is a
    // busy week reaching further back than a page — the same test.
    oldest(list(work.data?.records), PAGE.work, "Work");
    oldest(
      list(identity.page?.events).map((entry) => ({ at: entry.at ?? "" })),
      PAGE.identity,
      "Identity",
    );
    oldest(
      list(runtime.data?.events).map((event) => ({ at: event.timestamp })),
      PAGE.runtime,
      "Runtime",
    );
    oldest(list(knowledge.data?.changes), PAGE.pages, "Knowledge");
    oldest(
      list(config.data).map((revision) => ({ at: revision.created_at })),
      PAGE.config,
      "Configuration",
    );
    return short;
  }, [work.data, identity.page, runtime.data, knowledge.data, config.data, since]);

  const columns = useMemo<GridColumn<AuditEntry>[]>(
    () => [
      {
        key: "at",
        header: "When",
        shrink: true,
        sortValue: (row) => row.at,
        cell: (row) => <DateCell at={row.at} now={now} />,
      },
      {
        key: "source",
        header: "Where",
        shrink: true,
        sortValue: (row) => row.source,
        cell: (row) => <TextCell>{SOURCE_LABEL[row.source]}</TextCell>,
      },
      {
        key: "actor",
        header: "Who",
        shrink: true,
        sortValue: (row) => row.actor,
        cell: (row) => <WriterCell writer={writerOf(row, seatOf)} />,
      },
      {
        // THE KIND IS ITS OWN COLUMN. It shared the Who cell, where a chip
        // keeps its width and a name gives way — so beside Settings' column
        // at 1280 every writer read "m…" or "fo…" next to a whole "operator"
        // chip, and the log no longer said who did anything. Its own track
        // sizes to the chip, and the name has the Who track to itself.
        key: "as",
        header: "As",
        shrink: true,
        sortValue: (row) => row.actorKind,
        cell: (row) =>
          row.actorKind ? (
            <Tag appearance="outline">{row.actorKind}</Tag>
          ) : (
            <EmptyValue label="No kind recorded" />
          ),
      },
      {
        // THE CREDENTIAL BESIDE THE AUTHOR, in a track of its own for the
        // reason the kind has one.
        key: "through",
        header: "Through",
        shrink: true,
        sortValue: (row) => row.through,
        cell: (row) =>
          row.through ? (
            <span className="mono truncate" title={row.through}>
              {row.through}
            </span>
          ) : (
            <EmptyValue label="No credential beside the author" />
          ),
      },
      {
        key: "kind",
        header: "What",
        shrink: true,
        sortValue: (row) => row.kind,
        cell: (row) => <Tag appearance="outline">{row.kind.replaceAll("_", " ")}</Tag>,
      },
      {
        key: "subject",
        header: "To",
        shrink: true,
        sortValue: (row) => row.subject,
        // ONE LINE CUT WITH AN ELLIPSIS, the whole value on the title — a
        // backup's directory is a long absolute path, and clipped hard it
        // read "/tmp/claude-0/-home-use" with half a glyph and no way to see
        // the rest. On a phone card `.truncate` wraps instead (frame.css).
        cell: (row) =>
          !row.subject ? (
            // AN EMPTY CELL SAYS WHY, in the row's own terms, rather than
            // leaving a gap that reads as a lost value.
            <EmptyValue label={row.noSubject ?? "None recorded"} />
          ) : row.path ? (
            <a className="mono t-link truncate" href={href(row.path)} title={row.subject}>
              {row.subject}
            </a>
          ) : (
            <span className="mono truncate" title={row.subject}>
              {row.subject}
            </span>
          ),
      },
      {
        key: "detail",
        header: "Detail",
        cell: (row) =>
          row.detail ? (
            // THE WHOLE LINE ON THE TITLE, and the serving node under it: the
            // cell is cut at the column's width.
            <span
              className="truncate"
              title={row.node ? `${row.detail}\nOn ${row.node}` : row.detail}
            >
              {row.detail}
            </span>
          ) : (
            <EmptyValue label="None recorded" />
          ),
      },
    ],
    [now, seatOf],
  );

  // WHAT THE TWO BEST-EFFORT REST READS WITHHELD, each said by its source.
  const withheld = [secrets.withheld, identity.withheld].filter((sentence) => sentence !== "");

  const loading = work.loading || knowledge.loading || config.loading || runtime.loading;
  return (
    <>
      <PageActions>
        <TimeRangePicker range={range} ariaLabel="Window" />
        {/* THROUGH THE ONE DOWNLOAD PATH. This had its own, which revoked the
            file's URL in the same task as the click that queued it — racing
            the read the download is about to make — and said nothing either
            way; `DownloadButton` waits a task and says what happened. */}
        <DownloadButton
          variant="ghost"
          label="Export CSV"
          filename="crewlet-audit.csv"
          mime="text/csv;charset=utf-8"
          text={() => auditCsv(shown)}
          disabled={shown.length === 0}
          title={shown.length === 0 ? "Nothing on screen to export" : "The rows on screen, as CSV"}
        />
      </PageActions>

      {/* ONE BAR, SIZED BY WHAT IT HOLDS. The Who field took the full width
          (the kit's default) and pushed "Everywhere" onto a line of its own
          at every width; a named step is how every other filter field in the
          product is sized, and the toolbar is the row every other screen's
          filters sit in. */}
      <div className="toolbar">
        <Input
          type="search"
          value={actor}
          onChange={(e) => setActor(e.target.value)}
          onClear={() => setActor("")}
          clearLabel="Clear the actor filter"
          leading={<SearchGlyph size="sm" />}
          placeholder="Who"
          aria-label="Actor"
          ref={searchBox}
          width="sm"
        />
        <Select
          width="auto"
          value={kind}
          onChange={(value) => setKind(String(value))}
          ariaLabel="Where"
          active={kind !== ""}
          options={[
            { value: "", label: "Everywhere" },
            ...SOURCES.map((s) => ({ value: s, label: SOURCE_LABEL[s] })),
          ]}
        />
      </div>

      {loading && shown.length === 0 && (
        <Skeleton variant="text" rows={6} label="Loading what was done" />
      )}
      <CoverageNote coverage={[runtime.data?.coverage]} what="the runtime rows" />
      {/* A PAGE THAT STOPPED SHORT OF THE WINDOW IS A NOTICE OF ITS OWN, not
          the card's subtitle: a header line is one line, and the sentence was
          cut at "those rows are the newest, n…" — its whole point, lost at
          every width. */}
      {truncated.length > 0 && (
        <Callout variant="warning">
          {truncated.join(" and ")} answered one page, which does not reach the start of this window
          — those rows are the newest, not all of them.
        </Callout>
      )}
      {withheld.length > 0 && (
        <Callout variant="warning">
          {withheld.map((sentence) => (
            <p key={sentence}>{sentence}</p>
          ))}
        </Callout>
      )}
      <QueryState
        error={failed?.error ?? null}
        // THE REFUSAL OF THE READ WHOSE CODE IS SHOWN, never another's: reads
        // failing for different reasons must not pair one's code with a
        // second's grants.
        refusal={failed?.refusal ?? null}
        loading={loading}
      >
        <Card padding="none">
          <Card.Header
            icon={<FileTextGlyph size="sm" />}
            count={shown.length}
            subtitle="Every write a person or a credential made, every call they made at runtime, every change to who can reach the company, and every configuration revision, whoever wrote it."
          >
            <Card.Title>What was done</Card.Title>
          </Card.Header>
          <DataGrid
            rows={shown}
            columns={columns}
            rowKey={(row) => row.id}
            defaultSort="-at"
            isFailed={(row) => row.failed === true}
            empty={{
              title: "Nothing in this window",
              hint: "No person and no credential wrote anything here over this range. Widen the window, or clear the filters.",
              icon: "file-text",
            }}
            loadedNote={`${shown.length} loaded`}
          />
        </Card>
      </QueryState>
    </>
  );
}

/**
 * The credential rows, over REST — and, where they could not be read, a
 * sentence saying so.
 *
 * `/secrets` IS NOT A SOCKET QUESTION and deliberately is not one: it is the
 * route that can reveal a value, so it is guarded in full, reads included, and
 * a reveal is logged. This screen asks for the listing, which carries who last
 * stored each name and when — never a value, and it does not pass `?reveal`.
 *
 * BEST EFFORT, like every other credential read on a screen that is not about
 * credentials: a reader without the grant for `/secrets` still has an audit of
 * everything else, and a failed read here must not take the tracker's and the
 * wiki's rows down with it. But best effort is not SILENT: its failure is the
 * `withheld` sentence the notice carries.
 */
function useSecrets(): { rows: SecretRow[] | null; withheld: string } {
  const secrets = useRest(
    "/secrets",
    (signal) => rest.get("/secrets", signal) as Promise<{ secrets?: SecretRow[] } | null>,
    { pollMs: POLL_MS },
  );
  return {
    rows: secrets.data ? (secrets.data.secrets ?? []) : null,
    withheld: withheldSentence(CREDENTIALS_WITHHELD, secrets),
  };
}

/**
 * The identity estate's trail from the window's start, newest first, over REST
 * — and, where it could not be read, a sentence saying so.
 *
 * `at=` IS AN INSTANT the route resolves once into a log position: the trail
 * pages by position because no two nodes' clocks are compared. Best effort for
 * the reason the credentials are: a reader the directory's trail is refused to
 * keeps the rest of the audit, and is told what is missing.
 */
function useIdentityTrail(since: string): { page: IdentityAuditPage | null; withheld: string } {
  const params = new URLSearchParams({ at: since, limit: String(PAGE.identity) });
  const read = useRest(
    `/iam/audit?${params}`,
    (signal) => rest.get(`/iam/audit?${params}`, signal) as Promise<IdentityAuditPage | null>,
    { pollMs: POLL_MS },
  );
  return { page: read.data, withheld: withheldSentence(IDENTITY_WITHHELD, read) };
}

/**
 * What one best-effort source is called in the sentence that says it fell
 * short: the gesture its refusal names a grant for, the source as a subject,
 * and its rows.
 */
interface Withheld {
  gesture: string;
  subject: string;
  rows: string;
}

const CREDENTIALS_WITHHELD: Withheld = {
  gesture: "Listing the credentials' writes",
  subject: "The credentials",
  rows: "their writes",
};

const IDENTITY_WITHHELD: Withheld = {
  gesture: "Reading the identity trail",
  subject: "The identity trail",
  rows: "its entries",
};

/**
 * Why one best-effort source's rows are not all in this audit, as one
 * sentence — or "" when they are.
 *
 * THREE CASES, because the reader does something different about each: a
 * refusal names the grant that would list them; a read that failed with
 * nothing held says none of them are here; and one that failed while an
 * earlier answer is still on screen says those rows may be behind.
 */
function withheldSentence(source: Withheld, read: RestResult<unknown>): string {
  if (read.error === null) return "";
  if (read.error.unauthorized) {
    return needsSentence(source.gesture, read.error instanceof RestError ? read.error.grants : []);
  }
  return read.data === null
    ? `${source.subject} could not be read, so none of ${source.rows} are listed here.`
    : `${source.subject} could not be read again, so ${source.rows} are as they were last read.`;
}

/**
 * The loaded rows, as a file.
 *
 * WHAT IS ON SCREEN, never "the audit": an export that fetched more than the
 * grid shows would hand somebody a file they cannot reconcile with the page
 * they exported it from, and one that claimed to be complete would be a claim
 * about rows this screen never read. The footer says the same number.
 */
export function auditCsv(rows: AuditEntry[]): string {
  return toCsv(
    ["at", "where", "who", "who_kind", "through", "what", "to", "detail", "node", "failed"],
    rows.map((row) => [
      row.at,
      SOURCE_LABEL[row.source],
      row.actor,
      row.actorKind,
      row.through,
      row.kind,
      row.subject,
      row.detail,
      row.node ?? "",
      row.failed ? "true" : "",
    ]),
  );
}
