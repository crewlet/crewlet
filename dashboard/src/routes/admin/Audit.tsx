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
 * a sprint rollover nobody asked for is the thing an operator opens an audit
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

import { useEffect, useMemo, useState } from "react";
import { Button, Card, Input, Select, Skeleton, Tag } from "@crewlethq/ui";
import { DescriptionGlyph, ContentCopyGlyph } from "@crewlethq/icons/glyphs";

import { href } from "~/app/router.tsx";
import { useParam } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { Dash, DateCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useNow } from "~/lib/clock.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useTimeRange, type Offer } from "~/lib/range.ts";
import { rest } from "~/protocol/index.ts";
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
 * their own names, so those three read fine; a GOAL and a SAVED VIEW are
 * addressed by uuid, so five rows in a row read `6dd4b0df-f455-448e-80e9-…`
 * with nothing saying what they were.
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
    case "goal":
      return { subject: `goal ${short(id)}`, path: ["goals", id] };
    case "view":
      return { subject: `view ${short(id)}`, path: ["work", "views", id] };
    case "person":
      return { subject: id, path: ["company", "people", id] };
    case "project":
      return { subject: id, path: ["work", id] };
    default:
      // EVERY OTHER SUBJECT KIND HAS NO PAGE — a counter, a catalogue, a
      // tag set, a sprint, an alias. Named by its kind rather than linked,
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
  const now = useNow();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
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
  const secrets = useSecrets();

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
        ...workSubject(record),
        detail: record.excerpt ?? "",
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
        subject: change.title || change.page_id,
        path:
          change.container && change.title
            ? ["knowledge", change.container, change.title]
            : undefined,
        detail: change.excerpt ?? "",
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
        actorKind: "operator",
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
        actorKind: "operator",
        subject: row.name,
        path: ["admin", "credentials"],
        detail: row.source ? `from ${row.source}` : "",
      });
    }
    return out;
  }, [work.data, knowledge.data, config.data, secrets.rows]);

  /** Newest first, narrowed to the window and to what the reader asked. */
  const shown = useMemo(() => {
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
        cell: (row) => (
          <span className="row gap-1">
            {row.actor ? (
              <SeatCell handle={row.actor} name={index.byHandle.get(row.actor)?.name} />
            ) : (
              // THE ENGINE IS A WRITER. A sprint rollover, a chart apply and a
              // repair duty carry no actor at all, and rendering them as a
              // blank would make the five writers with no tool invisible on
              // the one screen that exists to name every writer.
              <span className="muted">the engine</span>
            )}
            {row.actorKind && <Tag appearance="outline">{row.actorKind}</Tag>}
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
            <Dash title="none recorded" />
          ),
      },
    ],
    [now, index],
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
      <QueryState error={work.error ?? knowledge.error ?? config.error} loading={loading}>
        <Card padding="none">
          <Card.Header
            icon={<DescriptionGlyph size="sm" />}
            count={shown.length}
            subtitle={
              truncated.length > 0
                ? `${truncated.join(" and ")} answered one page, which does not reach the start of this window — those rows are the newest, not all of them.`
                : "Every write a person or a token made, across the tracker, the knowledge base, the configuration and the credentials."
            }
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
 * The credential rows, over REST.
 *
 * `/secrets` IS NOT A SOCKET QUESTION and deliberately is not one: it is the
 * route that can reveal a value, so it is guarded in full, reads included, and
 * a read of it is logged. This screen asks for the listing, which carries who
 * last stored each name and when — never a value, and it does not pass
 * `?reveal`.
 */
function useSecrets(): { rows: SecretRow[] | null } {
  const [rows, setRows] = useState<SecretRow[] | null>(null);
  useEffect(() => {
    let live = true;
    void (async () => {
      try {
        const body = (await rest.get("/secrets")) as { secrets?: SecretRow[] } | null;
        if (live) setRows(body?.secrets ?? []);
      } catch {
        // BEST EFFORT, like every other credential read on a screen that is
        // not about credentials: an operator without the scope for `/secrets`
        // still has an audit of everything else, and a failed read here must
        // not take the tracker's and the wiki's rows down with it.
        if (live) setRows(null);
      }
    })();
    return () => {
      live = false;
    };
  }, []);
  return { rows };
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
  const lines = [["at", "where", "who", "who_kind", "what", "to", "detail"].join(",")];
  for (const row of rows) {
    lines.push(
      [
        row.at,
        SOURCE_LABEL[row.source],
        row.actor,
        row.actorKind,
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
