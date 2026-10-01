/**
 * What the operators of this company did to it.
 *
 * # Why it is one screen and not five
 *
 * Every write in this product is attributed — the tracker records the actor
 * and their kind on every commit, the knowledge base does the same, a config
 * revision names who created it, and a credential row names who last stored
 * it. Four subsystems, four honest records, and until now four places to look:
 * a question as ordinary as "what did we change last Tuesday" meant reading
 * the tracker's feed, the wiki's feed, the config history and the credential
 * table, each in its own chrome, each ordered differently.
 *
 * So this is the one screen that reads them together, in one order, over one
 * window. It writes nothing and asks no new question of the engine.
 *
 * # It is about WHO WAS WRITING, not about a list of people
 *
 * The two feeds narrow on `actor_kinds` rather than on a set of handles, and
 * that is the whole design: an `operator` commit carries a TOKEN's own label
 * where an `agent` one carries a seat handle, so the two name spaces are
 * disjoint — and the set of people is the roster, which changes. An audit
 * assembled from handles would quietly lose every commit made by somebody who
 * has since left the company, which is the one commit somebody is looking for.
 *
 * The three kinds are held apart and LABELLED rather than filtered down to
 * one, because "the engine did this" is as much an answer as "a person did":
 * a bulk edit nobody asked for is the thing an operator opens an audit
 * to find.
 *
 * # What each source can and cannot be asked
 *
 * Only the tracker's feed takes a wall-clock window (`from`/`to`); the wiki's
 * pages on a LOG POSITION, and the config and credential reads take neither.
 * So three of the four are fetched as their newest page and narrowed to the
 * window HERE — and the screen says so rather than implying its window is the
 * engine's. A row count from a client-side narrowing is a count over what was
 * loaded, never over what exists, which is the rule every list in this product
 * is held to.
 */

import { useMemo } from "react";
import { Button, Card, EmptyValue, Input, Select, Skeleton, Tag } from "@crewlethq/ui";
import { DescriptionGlyph, ContentCopyGlyph } from "@crewlethq/icons/glyphs";

import { href } from "~/app/router.tsx";
import { useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { DateCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { plainText } from "~/lib/markdown.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { needsSentence } from "~/lib/refusal.ts";
import { useRestRead, type RestRead } from "~/lib/restRead.ts";
import { throughOf } from "~/lib/attribution.ts";
import { indexOrg, seatLookup } from "~/lib/seats.ts";
import { useWindow, type Offer } from "~/lib/range.ts";
import { rest, RestError } from "~/protocol/index.ts";
import type { SecretRow, WorkActivityRecord } from "~/protocol/index.ts";

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
 * feed's own maximum page (`tracker.MaxActivityRows`), and the others are
 * sized to it: the footer says what was loaded, and a window wider than the
 * page reaches is reported rather than silently cut.
 */
const PAGE = { work: 200, pages: 100, config: 50 } as const;

/** How often the feeds are re-read. An audit is read, not watched. */
const POLL_MS = 60_000;

/** The four places a company's own writes are recorded. */
const SOURCES = ["work", "knowledge", "config", "credentials"] as const;
type Source = (typeof SOURCES)[number];

const SOURCE_LABEL: Record<Source, string> = {
  work: "Work",
  knowledge: "Knowledge",
  config: "Configuration",
  credentials: "Credentials",
};

/**
 * One recorded action, whichever subsystem recorded it.
 *
 * A SHAPE THIS SCREEN OWNS, deliberately, rather than a union of four wire
 * types: the whole point is that they are read together, and a grid whose
 * every cell begins by asking which of four kinds of row it is holding is a
 * grid with four renderings in it.
 */
export interface AuditEntry {
  id: string;
  at: string;
  source: Source;
  /** What was done, in the subsystem's own word. */
  kind: string;
  /** The handle or token label recorded on the write. */
  actor: string;
  /** `operator`, `human`, `agent`, `system` — empty where none was recorded. */
  actorKind: string;
  /**
   * The credential the write was made through, where it names something the
   * actor does not — a person's machine token or browser session — and empty
   * otherwise. Every source records one now, so a row a token wrote is never
   * read as one its owner made by hand.
   */
  through: string;
  /** What it was done to, as a person would name it. */
  subject: string;
  /** Where that object lives, where it still has an address. */
  path?: string[];
  /** The one line that says what actually changed. */
  detail: string;
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
      return { subject: id, path: ["company", "people", id] };
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

/** The leading segment of a uuid, which is what tells two of them apart. */
function short(id: string): string {
  return id.length > 8 && id.includes("-") ? id.slice(0, 8) : id;
}

/**
 * A source's rows, or none at all.
 *
 * FOUR SOURCES AND ONE BAD ANSWER MUST NOT COST THE OTHER THREE. This screen
 * is the only one that reads four subsystems at once, so what is a blank page
 * anywhere else is three subsystems' worth of history lost here — and the
 * failure mode is not hypothetical: `config_audit` is the one question in this
 * set that answers a BARE ARRAY rather than an object, so `?? []` covers a
 * null and does not cover anything else, and iterating whatever arrived is a
 * throw that takes the whole screen with it.
 *
 * It is not a guard against the engine. It is a guard against the SET: a
 * screen that composes four independent reads has four chances at that throw,
 * and an audit is the last screen that should go blank.
 */
function list<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : [];
}

/**
 * WHOSE WRITES COUNT AS AN OPERATOR'S.
 *
 * A person at the dashboard and a token acting for the company are the two
 * kinds an audit is about, and they are DIFFERENT kinds rather than two
 * spellings of one: the tracker refuses to record a token under a seat handle
 * precisely so that this distinction survives. Both are asked for, and the
 * rows say which.
 */
const OPERATOR_KINDS = "operator,human";

export function Audit() {
  const org = useOrg();
  // THE CHART'S TWO ANSWERS ABOUT A HANDLE — the name and the kind — since
  // the only thing this screen asks of the chart is how to draw a writer.
  const who = useMemo(() => seatLookup(indexOrg(org)), [org]);
  // THE CHOICE, NOT ITS EDGES. This screen read the one-second clock and
  // turned it into `from` and `to` at render, so every tick was a new
  // question: the tracker was asked for its feed once a second where the
  // poll below says once a minute, and every row on screen was drawn again
  // each time. The edges are computed when the feed is ASKED.
  const range = useWindow(AUDIT_OFFER);
  const [actor, setActor] = useParam("actor", "");
  const [kind, setKind] = useParam("kind", "");

  // THE ONE SOURCE THAT TAKES THE WINDOW. `from` and `to` bound the AUTHORED
  // instants, which is what somebody typing "last week" means (D113) — and
  // they are the window as of each ask, the first and every poll after it.
  const work = useQuery(
    "work_activity",
    {
      container: "workspace",
      actor_kinds: OPERATOR_KINDS,
      limit: PAGE.work,
    },
    { pollMs: POLL_MS, window: { over: range.window, since: "from", until: "to" } },
  );
  // AND THE WINDOW EVERY OTHER SOURCE IS CUT TO: the one the tracker was last
  // asked over, so the three sources narrowed here and the one the engine
  // narrowed agree about where it starts. Null only before the first ask,
  // when nothing has answered to be cut.
  const since = work.asked?.since ?? "";
  const until = work.asked?.until ?? "";
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
  const secrets = useSecrets();
  // The first of the three reads that failed, whose code and refusal the
  // banner shows together.
  const failed = [work, knowledge, config].find((read) => read.error !== null);

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
        path:
          change.container && change.title
            ? ["knowledge", change.container, change.title]
            : undefined,
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
        // THE RECORDED KIND, where there is one. This said `operator` for
        // every revision, so a person bound to a seat, the reconcile loop and
        // a Tier A token all read alike; a revision written before the kind
        // was recorded shows none rather than a guess.
        actorKind: revision.created_by_kind ?? "",
        through: throughOf(revision.created_by ?? "", revision.operator_id),
        subject: revision.revision_id.slice(0, 8),
        path: ["admin", "config", "revisions", revision.revision_id],
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
        actorKind: row.updated_by_kind ?? "",
        through: throughOf(row.updated_by ?? "", row.operator_id),
        subject: row.name,
        path: ["admin", "credentials"],
        detail: row.source ? `from ${row.source}` : "",
      });
    }
    return out;
  }, [work.data, knowledge.data, config.data, secrets.rows]);

  /** Newest first, narrowed to the window and to what the reader asked. */
  const shown = useMemo(() => {
    if (!since || !until) return [];
    const from = Date.parse(since);
    const to = Date.parse(until);
    return rows
      .filter((row) => {
        const at = Date.parse(row.at);
        if (Number.isNaN(at) || at < from || at > to) return false;
        if (kind && row.source !== kind) return false;
        if (actor && !row.actor.toLowerCase().includes(actor.toLowerCase())) return false;
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
    if (!since) return short;
    const from = Date.parse(since);
    const oldest = (list: { at: string }[], page: number, label: string) => {
      if (list.length < page) return;
      const last = list[list.length - 1];
      if (last && Date.parse(last.at) > from) short.push(label);
    };
    oldest(list(knowledge.data?.changes), PAGE.pages, "Knowledge");
    oldest(
      list(config.data).map((revision) => ({ at: revision.created_at })),
      PAGE.config,
      "Configuration",
    );
    return short;
  }, [knowledge.data, config.data, since]);

  const columns = useMemo<GridColumn<AuditEntry>[]>(
    () => [
      {
        key: "at",
        header: "When",
        shrink: true,
        sortValue: (row) => row.at,
        cell: (row) => <DateCell at={row.at} />,
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
        cell: (row) => (
          <span className="row gap-1">
            {row.actor ? (
              // THE NAME AND THE KIND, from one lookup: the badge's dashed
              // ring is a HUMAN seat, and a cell handed only the name draws
              // every writer as an agent on a screen whose subject is the
              // writers that are not.
              <SeatCell handle={row.actor} {...who(row.actor)} />
            ) : (
              // THE ENGINE IS A WRITER. A chart apply and a
              // repair duty carry no actor at all, and rendering them as a
              // blank would make the five writers with no tool invisible on
              // the one screen that exists to name every writer.
              <span className="muted">the engine</span>
            )}
            {row.actorKind && <Tag appearance="outline">{row.actorKind}</Tag>}
            {row.through && <span className="muted">through {row.through}</span>}
          </span>
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
        cell: (row) =>
          row.path ? (
            <a className="mono t-link" href={href(row.path)}>
              {row.subject}
            </a>
          ) : (
            <span className="mono">{row.subject}</span>
          ),
      },
      {
        key: "detail",
        header: "Detail",
        cell: (row) =>
          row.detail ? (
            <span className="truncate">{row.detail}</span>
          ) : (
            <EmptyValue label="None recorded" />
          ),
      },
    ],
    [who],
  );

  const loading = work.loading || knowledge.loading || config.loading;
  return (
    <>
      <PageActions>
        <TimeRangePicker range={range} ariaLabel="Window" />
        <Button
          size="small"
          variant="tertiary"
          leadingIcon={<ContentCopyGlyph />}
          onClick={() => downloadCsv(shown)}
          disabled={shown.length === 0}
        >
          Export CSV
        </Button>
      </PageActions>

      <div className="row wrap gap-2">
        <Input
          value={actor}
          onChange={(e) => setActor(e.target.value)}
          placeholder="Who"
          aria-label="Actor"
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
      <QueryState
        error={failed?.error ?? null}
        // THE REFUSAL OF THE READ WHOSE CODE IS SHOWN, never another's: three
        // reads failing for three reasons must not pair one's code with a
        // second's grants.
        refusal={failed?.refusal ?? null}
        loading={loading}
      >
        <Card padding="none">
          <Card.Header
            icon={<DescriptionGlyph size="sm" />}
            count={shown.length}
            subtitle={auditSubtitle(truncated, secrets.withheld)}
          >
            <Card.Title>What was done</Card.Title>
          </Card.Header>
          <DataGrid
            rows={shown}
            columns={columns}
            rowKey={(row) => row.id}
            defaultSort="-at"
            empty={{
              title: "Nothing in this window",
              hint: "No person and no operator token wrote anything here over this range. Widen the window, or clear the filters.",
              icon: "description",
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
 * wiki's rows down with it. But best effort is not SILENT. This was a loader of
 * its own that read once, at mount, and dropped every failure on the floor: a
 * request past its deadline, a node catching up, a refusal on authority — each
 * left the credentials out of an audit whose header said it covered them, with
 * nothing on the page to say they were missing, and nothing asked again while
 * the other three sources polled. It is the shared REST read now
 * (`~/lib/restRead.ts`), on the cadence the other three keep, asked again on
 * its own where waiting can clear a failure, and its failure is the `withheld`
 * sentence the card's header carries.
 */
function useSecrets(): { rows: SecretRow[] | null; withheld: string } {
  const read = useRestRead(
    "/secrets",
    async (signal) =>
      ((await rest.get("/secrets", signal)) as { secrets?: SecretRow[] } | null)?.secrets ?? [],
    { cadence: () => POLL_MS },
  );
  return { rows: read.data, withheld: withheldSentence(read) };
}

/**
 * Why the credentials' writes are not all in this audit, as one sentence — or
 * "" when they are.
 *
 * THREE CASES, because the reader does something different about each: a
 * refusal names the grant that would list them; a read that failed with
 * nothing held says none of theirs are here; and one that failed while an
 * earlier answer is still on screen says those rows may be behind.
 */
function withheldSentence(read: RestRead<SecretRow[]>): string {
  if (read.failure === null) return "";
  if (read.failure.error === "unauthorized") {
    return needsSentence(
      "Listing the credentials' writes",
      read.error instanceof RestError ? read.error.grants : [],
    );
  }
  return read.data === null
    ? "The credentials could not be read, so none of their writes are listed here."
    : "The credentials could not be read again, so their writes are as they were last read.";
}

/**
 * The card's header line: what the rows cover, and every way they fall short.
 *
 * Each shortfall is its own sentence, and the claim that the rows cover all
 * four sources is made only where nothing fell short — a header saying "across
 * … the credentials" over an audit that read none of them was the screen
 * claiming rows it never had.
 */
function auditSubtitle(truncated: string[], withheld: string): string {
  const short = [
    truncated.length > 0
      ? `${truncated.join(" and ")} answered one page, which does not reach the start of this window — those rows are the newest, not all of them.`
      : "",
    withheld,
  ].filter((sentence) => sentence !== "");
  return short.length > 0
    ? short.join(" ")
    : "Every write a person or a token made, across the tracker, the knowledge base, the configuration and the credentials.";
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
  const cell = (value: string) => `"${value.replaceAll('"', '""')}"`;
  const lines = [["at", "where", "who", "who_kind", "through", "what", "to", "detail"].join(",")];
  for (const row of rows) {
    lines.push(
      [
        row.at,
        SOURCE_LABEL[row.source],
        row.actor,
        row.actorKind,
        row.through,
        row.kind,
        row.subject,
        row.detail,
      ]
        .map(cell)
        .join(","),
    );
  }
  return lines.join("\n");
}

function downloadCsv(rows: AuditEntry[]): void {
  const blob = new Blob([auditCsv(rows)], { type: "text/csv;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = "crewlet-audit.csv";
  link.click();
  URL.revokeObjectURL(url);
}
