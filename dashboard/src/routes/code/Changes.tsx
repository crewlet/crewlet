/**
 * Code › Changes, the workspace's landing: the changes proposed to the
 * company's repositories, by what each waits on.
 *
 * # The engine's listing, narrowed by its own filters
 *
 * `code_changes` answers a page of changes held to the reader's grant on each
 * one's repository, in the order of the ONE index its filters lead: a work
 * item's changes, an owner's in a state, a repository's, the company's in a
 * state — or the changes waiting on somebody (`needs`, `reviewer`), each its
 * own index of who they wait on. A page is what the engine READ, never what it
 * kept, so one can be short, or empty with a cursor, and the grid pages with
 * Load more until no cursor comes back, its first page held still from the
 * press (`lib/usePaged.ts`). The order is the engine's, and no head re-sorts
 * the pages held.
 *
 * THE ENGINE KEEPS NO INDEX OF EVERY CHANGE: a listing names a state, a
 * repository, a work item, a reviewer or what its changes wait on, and is
 * refused naming none. So the section asks for the OPEN changes until it is
 * told otherwise, and "Every state" is offered only where something else
 * narrows the walk — and said in place, never sent, where nothing does.
 *
 * # What a row says is the engine's judgement
 *
 * A live change's READINESS is judged by the engine on the read that answered
 * it, against its one chart (`code.ReadinessOf`): its preconditions, how many
 * are met, and the sentence of the first that is not. The row's Meter counts
 * them, its title is that sentence, and the standing blockers are the blocker
 * precondition's — named and counted beyond — never counted here from rows
 * this screen does not hold.
 *
 * # Needs a person
 *
 * A change whose first unmet precondition waits on persons alone — a
 * person's approval, a blocker a person raised that an author answered —
 * waits on somebody who is not an agent and will not be woken like one. The
 * band above the grid lists them (`needs=person`) wherever the grid is not
 * already that listing, so the one thing an agent company cannot do for
 * itself is the first thing a person reads here.
 *
 * # A row opens its change in the peek
 *
 * By its reference, `peek=change:web!42`: the change's own reading, beside
 * the list, which is where a reader decides what to open next.
 */

import { useMemo } from "react";
import {
  Button,
  EmptyState,
  EmptyValue,
  Meter,
  Select,
  Skeleton,
  TableDateCell,
  TableEntityCell,
  TableKeyCell,
  TableNumberCell,
  TableTextCell,
  Tag,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { peekHref, peekRow, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { plainSeat } from "~/app/frame/cells.tsx";
import { useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import {
  CHANGE_NEED_FILTERS,
  CHANGE_STATE_FILTERS,
  EVERY_STATE,
  changeLive,
  changeStateWord,
  firstUnready,
  isChangeNeed,
  isRepoName,
  standingBlockers,
} from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import type {
  CodeChangeState,
  CodeChangeSummary,
  CodeChangesAnswer,
  CodeReadiness,
} from "~/protocol/index.ts";
import { REPO_PARAM, changeObject } from "./paths.ts";

/**
 * How often the listing's first page is read again while it is open and
 * nothing past it is loaded: a MINUTE, the cadence the workspace reads the
 * code log at (Repositories, Pushes). The code log has no push to the
 * dashboard, so another seat's review reaches the open listing by this read
 * or not at all — and a change's readiness moves with every record about it.
 */
const CHANGES_POLL_MS = 60_000;

/**
 * How many changes the "Needs a person" band lists: FIVE, what a reader
 * takes in at a glance above the grid without pushing the grid off a laptop's
 * screen. Asked as the page's limit, so the band reads no more than it draws;
 * its Show all is the grid narrowed to the same need.
 */
export const PERSON_BAND_ROWS = 5;

/** The state the section lists until it is told otherwise. */
const DEFAULT_STATE = "open";

/** A page's changes, each change's id, and the cursor to the page after. */
const changesOf = (a: CodeChangesAnswer) => a.changes ?? [];
const changeIdOf = (c: CodeChangeSummary) => c.id;
const changesAfter = (a: CodeChangesAnswer) => a.next ?? "";

/** The filters the address names, as the section reads them. */
interface Filters {
  state: string;
  needs: string;
  repo: string;
  owner: string;
  reviewer: string;
  work: string;
}

/**
 * Whether something beside the state narrows the engine's walk — a
 * repository, a work item, a reviewer or a need — which is what lets a
 * listing name no state.
 */
export function narrowsTheWalk(f: Filters): boolean {
  return f.repo !== "" || f.work !== "" || f.reviewer !== "" || isChangeNeed(f.needs);
}

/**
 * The listing's parameters for the filters the address names, or a sentence
 * saying why it cannot be asked: a repository's name no repository can have,
 * a need the engine does not list by, or every state where nothing narrows the
 * walk. Said in place, never sent to be refused.
 */
export function changesParams(f: Filters): { params: CodeParams } | { unasked: string } {
  if (f.repo !== "" && !isRepoName(f.repo)) {
    return { unasked: `“${f.repo}” is not a name a repository can have.` };
  }
  if (f.needs !== "" && !isChangeNeed(f.needs)) {
    return { unasked: `“${f.needs}” is not something a change waits on.` };
  }
  if (f.state === EVERY_STATE && !narrowsTheWalk(f)) {
    return {
      unasked:
        "Every state is listed for one repository, one work item, one reviewer or what the changes wait on: choose one.",
    };
  }
  const params: Record<string, string> = {};
  if (f.state !== EVERY_STATE) params.state = f.state;
  for (const key of ["needs", "repo", "owner", "reviewer", "work"] as const) {
    if (f[key] !== "") params[key] = f[key];
  }
  return { params };
}

export function Changes() {
  const [state, setState] = useParam("state", DEFAULT_STATE, "filter");
  const [needs, setNeeds] = useParam("needs", "", "filter");
  const [repo, setRepo] = useParam(REPO_PARAM, "", "filter");
  const [owner, setOwner] = useParam("owner", "", "filter");
  const [reviewer, setReviewer] = useParam("reviewer", "", "filter");
  const [work, setWork] = useParam("work", "", "filter");
  const filters = useMemo(
    () => ({ state, needs, repo, owner, reviewer, work }),
    [state, needs, repo, owner, reviewer, work],
  );
  const asked = useMemo(() => changesParams(filters), [filters]);
  const params = "params" in asked ? asked.params : {};
  // THE REPOSITORIES THE READER MAY READ, for the bar's picker: the listing's
  // first page, by name, as Pushes offers them.
  const choices = useCodeQuery("code_repos");
  const hold = usePageHold<CodeChangesAnswer>(params);
  const first = useCodeQuery("code_changes", params, {
    enabled: "params" in asked,
    pollMs: CHANGES_POLL_MS,
    hold: hold.holding,
  });
  const paged = usePaged(
    hold,
    first,
    (cursor) => codeRest.query("code_changes", { ...params, cursor }),
    changesOf,
    changeIdOf,
    changesAfter,
  );
  const rows = paged.items;
  const narrowed = narrowsTheWalk(filters);
  const repoChoices = [
    { value: "", label: "Every repository" },
    ...(choices.data?.repos ?? []).map((r) => ({ value: r.name, label: r.name })),
    // THE NAMED ONE, where the first page does not hold it: a choice the
    // address made is never one the picker cannot show.
    ...(repo !== "" && !(choices.data?.repos ?? []).some((r) => r.name === repo)
      ? [{ value: repo, label: repo }]
      : []),
  ];

  return (
    <>
      <div className="toolbar">
        <Select
          width="auto"
          value={needs}
          onChange={(value) => setNeeds(String(value))}
          options={CHANGE_NEED_FILTERS.map((f) => ({ value: f.value, label: f.label }))}
          ariaLabel="Waiting on"
          active={needs !== ""}
        />
        <Select
          width="auto"
          value={state}
          onChange={(value) => setState(String(value))}
          options={CHANGE_STATE_FILTERS.filter(
            (f) => f.value !== EVERY_STATE || narrowed || state === EVERY_STATE,
          ).map((f) => ({ value: f.value, label: f.label }))}
          ariaLabel="Which changes"
          active={state !== DEFAULT_STATE}
        />
        <Select
          width="auto"
          value={repo}
          onChange={(value) => setRepo(String(value))}
          options={repoChoices}
          ariaLabel="Repository"
          active={repo !== ""}
        />
        <Narrowing label="Owner" value={owner} clear={() => setOwner("")} />
        <Narrowing label="Reviewer" value={reviewer} clear={() => setReviewer("")} />
        <Narrowing label="Work item" value={work} clear={() => setWork("")} />
      </div>
      {needs !== "person" && "params" in asked && (
        <PersonBand repo={repo} showAll={() => setNeeds("person")} />
      )}
      {"unasked" in asked ? (
        <EmptyState title="Nothing to list" description={asked.unasked} />
      ) : (
        <>
          {paged.loading && rows.length === 0 && (
            <Skeleton variant="text" rows={6} label="Loading changes" />
          )}
          <QueryState
            error={paged.error}
            refusal={paged.refusal}
            detail={first.detail}
            loading={paged.loading}
            empty={
              rows.length > 0 || paged.more
                ? undefined
                : { title: emptyTitle(filters), hint: emptyHint(filters) }
            }
          >
            <ChangesGrid
              rows={rows}
              onLoadMore={paged.more ? paged.older : undefined}
              loadedNote={
                paged.more || paged.held
                  ? `${plural(rows.length, "change")} loaded${
                      paged.held ? ", paused while more are loaded" : ""
                    }`
                  : undefined
              }
            />
          </QueryState>
          {paged.pageError && <PageFailed error={paged.pageError} />}
        </>
      )}
    </>
  );
}

/**
 * A filter the address names that the bar has no picker for — an owner, a
 * reviewer, a work item, each arriving from a link — said as what it is, with
 * the press that lifts it.
 */
function Narrowing({ label, value, clear }: { label: string; value: string; clear: () => void }) {
  if (value === "") return null;
  return (
    <span className="row gap-1 nowrap">
      <Tag appearance="outline">
        {label}: <span className="mono">{value}</span>
      </Tag>
      <Button
        size="small"
        variant="ghost"
        onClick={clear}
        aria-label={`Stop narrowing by ${label}`}
      >
        Clear
      </Button>
    </span>
  );
}

/** The grid of changes, as every listing of them draws it. */
export function ChangesGrid({
  rows,
  onLoadMore,
  loadedNote,
}: {
  rows: CodeChangeSummary[];
  onLoadMore: (() => void) | undefined;
  loadedNote: string | undefined;
}) {
  const now = useNow();
  const seatBadge = useSeatBadgeOf();
  const { open } = usePeekControls();
  return (
    <DataGrid<CodeChangeSummary>
      rows={rows}
      rowKey={(c) => c.id}
      // THE ENGINE'S ORDER, over the whole set its filters walk.
      serverSorted
      rowHref={(c) => peekHref(changeObject(c.repo, c.number))}
      onRowActivate={peekRow((c: CodeChangeSummary) => open(changeObject(c.repo, c.number)))}
      onLoadMore={onLoadMore}
      loadedNote={loadedNote}
      aria-label="Changes"
      columns={[
        {
          key: "change",
          header: "Change",
          shrink: true,
          phoneLead: true,
          cell: (c) => <TableKeyCell value={c.ref} />,
        },
        {
          key: "title",
          header: "Title",
          floor: "12rem",
          cell: (c) => <TableTextCell>{c.title}</TableTextCell>,
        },
        {
          key: "state",
          header: "State",
          shrink: true,
          cell: (c) => <ChangeStateTag state={c.state} />,
        },
        {
          key: "readiness",
          header: "Readiness",
          shrink: true,
          drop: 1,
          cell: (c) => <ReadinessMeter readiness={c.readiness} live={changeLive(c.state)} />,
        },
        {
          key: "owner",
          header: "Owner",
          shrink: true,
          // NOT A LINK: the row around it is one, to the change.
          cell: (c) => <TableEntityCell {...plainSeat({ ...seatBadge(c.owner) })} />,
        },
        {
          key: "work",
          header: "Work item",
          shrink: true,
          drop: 3,
          cell: (c) =>
            c.work_item ? (
              <TableKeyCell value={c.work_item.key || c.work_item.id} />
            ) : (
              <EmptyValue label="No work item" />
            ),
        },
        {
          key: "revisions",
          header: "Revisions",
          shrink: true,
          align: "right",
          drop: 4,
          cell: (c) => <TableNumberCell value={c.revision} format={fmtCount} />,
        },
        {
          key: "blockers",
          header: "Blockers",
          shrink: true,
          align: "right",
          drop: 2,
          // A CHANGE THAT IS NO LONGER LIVE HAS NO READINESS, and so no
          // standing blocker to count: said as nothing rather than as none.
          cell: (c) =>
            c.readiness ? (
              <TableNumberCell value={standingBlockers(c.readiness)} format={fmtCount} />
            ) : (
              <EmptyValue label="Not live" />
            ),
        },
        {
          key: "opened",
          header: "Opened",
          shrink: true,
          drop: 5,
          cell: (c) => <TableDateCell value={c.opened_at} now={now} />,
        },
      ]}
    />
  );
}

/**
 * A change's state as its row marks it. COLOUR IS STATE, and only the two that
 * are an outcome carry one: merged, which reached its target, and closed,
 * which will not — neutral, since closing is no fault. A live change is
 * outlined: it is still moving.
 */
export function ChangeStateTag({ state }: { state: CodeChangeState }) {
  const word = changeStateWord(state);
  if (state === "merged") return <Tag variant="success">{word}</Tag>;
  if (state === "closed") return <Tag variant="neutral">{word}</Tag>;
  return <Tag appearance="outline">{word}</Tag>;
}

/**
 * How many of a live change's preconditions are met, of how many — titled by
 * the engine's sentence of the first that is not, which is what the reader
 * acts on — or nothing for a change that is no longer live.
 */
export function ReadinessMeter({
  readiness,
  live,
}: {
  readiness: CodeReadiness | undefined;
  live: boolean;
}) {
  if (!live || !readiness) return <EmptyValue label="Not live" />;
  const next = firstUnready(readiness);
  return (
    <span title={readiness.first}>
      <Meter
        hideLabel
        size="compact"
        label="Readiness"
        value={readiness.met}
        max={Math.max(readiness.total, 1)}
        // COLOUR IS STATE: a change every precondition of which is met is
        // ready, and only that is drawn as done; a part-met readiness is a
        // quantity, not a warning.
        polarity="progress"
        tone={next ? "neutral" : "success"}
        valueText={`${readiness.met} of ${plural(readiness.total, "precondition")} met${
          next ? `: ${readiness.first}` : ", ready to merge"
        }`}
      />
    </span>
  );
}

/**
 * The changes that wait on a person, above the grid: the engine's
 * `needs=person` listing, its first few, each opening in the peek — and
 * nothing at all where none does, since a band saying "nothing" every visit is
 * a band nobody reads.
 */
function PersonBand({ repo, showAll }: { repo: string; showAll: () => void }) {
  const { open } = usePeekControls();
  const params = useMemo(
    () => ({ needs: "person", limit: PERSON_BAND_ROWS, ...(repo ? { [REPO_PARAM]: repo } : {}) }),
    [repo],
  );
  const band = useCodeQuery("code_changes", params, {
    enabled: repo === "" || isRepoName(repo),
    pollMs: CHANGES_POLL_MS,
  });
  const changes = band.data?.changes ?? [];
  if (changes.length === 0) return null;
  return (
    <section className="code-person-band" aria-label="Needs a person">
      <div className="row gap-2">
        <strong>Needs a person</strong>
        <Button size="small" variant="ghost" onClick={showAll}>
          Show all
        </Button>
      </div>
      <ul className="code-person-band__list">
        {changes.map((c) => (
          <li key={c.id}>
            <a
              className="mono"
              href={peekHref(changeObject(c.repo, c.number))}
              onClick={(e) => {
                if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
                e.preventDefault();
                open(changeObject(c.repo, c.number));
              }}
            >
              {c.ref}
            </a>{" "}
            {c.title}
            {c.readiness && <span className="muted"> — {c.readiness.first}</span>}
          </li>
        ))}
      </ul>
    </section>
  );
}

/** What an empty listing says it found none of. */
function emptyTitle(f: Filters): string {
  switch (f.needs) {
    case "me":
      return "Nothing waits on you";
    case "review":
      return "No change waits on a review";
    case "fix":
      return "No change waits on a fix";
    case "person":
      return "No change waits on a person";
  }
  if (f.state === EVERY_STATE) return "No changes";
  return `No ${changeStateWord(f.state as CodeChangeState).toLowerCase()} changes`;
}

/** Where an empty listing sends the reader next. */
function emptyHint(f: Filters): string {
  if (f.needs !== "" || f.repo !== "" || f.owner !== "" || f.reviewer !== "" || f.work !== "") {
    return "Widen the filters to see more.";
  }
  return "A change is opened by a seat's code_open_change, or by a push with the change option.";
}
