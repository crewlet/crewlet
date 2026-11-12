/**
 * Gitlet › Changes, the workspace's landing: the changes proposed to the
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
 * Load more until no cursor comes back, each page joined to the one before it
 * (`lib/usePaged.ts`). The order is the engine's, and no head re-sorts the
 * pages loaded.
 *
 * THE ENGINE KEEPS NO INDEX OF EVERY CHANGE: a listing names a state, a
 * repository, a work item, a reviewer, what its changes wait on or a window
 * they moved in, and is refused naming none. So the section asks for the OPEN changes until it is
 * told otherwise, and "Every state" is offered only where something else
 * narrows the walk — and said in place, never sent, where nothing does.
 *
 * # What a row says is the engine's judgement
 *
 * A live change's READINESS is judged by the engine on the read that answered
 * it, against its one chart (`gitlet.ReadinessOf`): its preconditions, how many
 * are met, and the sentence of the first that is not. The row's Meter counts
 * them, and its title is that sentence — never counted here from rows this
 * screen does not hold.
 *
 * # Open findings and Updated are the engine's counts too
 *
 * Each row says how many of the change's findings stand open — none of its
 * authors has answered them — and when it last moved: the later of its
 * lifecycle's last record and the last thing somebody wrote about it, never
 * the home's facts, so a target branch that moved re-dates nothing. `window=`
 * narrows the listing to the changes that moved within the last day, week or
 * month. Beside every state, or a merged or closed one, and nothing else
 * narrowing the walk, the engine walks its two indexes of that instant and the
 * rows come newest move first; beside a live state (the default open, or
 * draft) it walks that state's changes, which a company keeps few of, and
 * tests the window on each, so the rows come newest opening first.
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
  peekRowActivation,
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
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
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
  CHANGE_WINDOW_FILTERS,
  isChangeNeed,
  isChangeWindow,
  isRepoName,
} from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { useSeatBadgeOf } from "~/lib/seats.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type { CodeChangeState, CodeChangeSummary, CodeChangesAnswer } from "~/protocol/index.ts";
import { REPO_PARAM, changeObject } from "./paths.ts";
import { readinessCount, readinessNext } from "./readinessWords.ts";
import { RepoPicker } from "./RepoPicker.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";

/**
 * How often the listing's first page is read again while it is open: a
 * MINUTE, the cadence the workspace reads the code log at (Repositories,
 * Pushes). The code log has no push to the
 * dashboard, so another seat's review reaches the open listing by this read
 * or not at all — and a change's readiness moves with every record about it.
 */
const CHANGES_POLL_MS = 60_000;

/**
 * How many changes the "Needs a person" band lists: FIVE, what a reader
 * takes in at a glance above the grid without pushing the grid off a laptop's
 * screen. Asked as each page's limit, and kept: a page is the rows the engine
 * read, some of which it then drops (a change of a repository the reader may
 * not read, one whose readiness no longer waits on a person), so the band
 * follows `next` for at most `CHANGES_FILL_PAGES` pages to keep five
 * ([CodeChangesOptions.fill]); its Show all is the grid narrowed to the same
 * need.
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
  window: string;
}

/**
 * Whether something beside the state narrows the engine's walk — a
 * repository, a work item, a reviewer, a need or a window — which is what
 * lets a listing name no state.
 */
export function narrowsTheWalk(f: Filters): boolean {
  return (
    f.repo !== "" ||
    f.work !== "" ||
    f.reviewer !== "" ||
    isChangeNeed(f.needs) ||
    isChangeWindow(f.window)
  );
}

/**
 * The listing's parameters for the filters the address names, or a sentence
 * saying why it cannot be asked: a repository's name no repository can have,
 * a need or a window the engine does not list by, or every state where
 * nothing narrows the walk. Said in place, never sent to be refused.
 */
export function changesParams(f: Filters): { params: CodeParams } | { unasked: string } {
  if (f.repo !== "" && !isRepoName(f.repo)) {
    return { unasked: `“${f.repo}” is not a name a repository can have.` };
  }
  if (f.needs !== "" && !isChangeNeed(f.needs)) {
    return { unasked: `“${f.needs}” is not something a change waits on.` };
  }
  if (f.window !== "" && !isChangeWindow(f.window)) {
    return { unasked: `“${f.window}” is not a window the changes are listed by.` };
  }
  if (f.state === EVERY_STATE && !narrowsTheWalk(f)) {
    return {
      unasked:
        "Every state is listed for one repository, one work item, one reviewer, what the changes wait on or a window they moved in: choose one.",
    };
  }
  const params: Record<string, string> = {};
  if (f.state !== EVERY_STATE) params.state = f.state;
  for (const key of ["needs", "repo", "owner", "reviewer", "work", "window"] as const) {
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
  const [movedIn, setMovedIn] = useParam("window", "", "filter");
  const filters = useMemo(
    () => ({ state, needs, repo, owner, reviewer, work, window: movedIn }),
    [state, needs, repo, owner, reviewer, work, movedIn],
  );
  const asked = useMemo(() => changesParams(filters), [filters]);
  const params = "params" in asked ? asked.params : {};
  const first = useCodeQuery("code_changes", params, {
    enabled: "params" in asked,
    pollMs: CHANGES_POLL_MS,
  });
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_changes", { ...params, cursor }),
    changesOf,
    changeIdOf,
    changesAfter,
  );
  const rows = paged.items;
  const narrowed = narrowsTheWalk(filters);

  return (
    <>
      <PageToolbar>
        <Select
          size="sm"
          width="auto"
          value={needs}
          onChange={(value) => setNeeds(String(value))}
          options={CHANGE_NEED_FILTERS.map((f) => ({ value: f.value, label: f.label }))}
          ariaLabel="Waiting on"
          active={needs !== ""}
        />
        <Select
          size="sm"
          width="auto"
          value={state}
          onChange={(value) => setState(String(value))}
          options={CHANGE_STATE_FILTERS.filter(
            (f) => f.value !== EVERY_STATE || narrowed || state === EVERY_STATE,
          ).map((f) => ({ value: f.value, label: f.label }))}
          ariaLabel="Which changes"
          active={state !== DEFAULT_STATE}
        />
        <RepoPicker size="sm" repo={repo} onChoose={setRepo} every="Every repository" />
        <Select
          size="sm"
          width="auto"
          value={movedIn}
          onChange={(value) => setMovedIn(String(value))}
          options={CHANGE_WINDOW_FILTERS.map((f) => ({ value: f.value, label: f.label }))}
          ariaLabel="Moved"
          active={movedIn !== ""}
        />
        <Narrowing label="Owner" value={owner} clear={() => setOwner("")} />
        <Narrowing label="Reviewer" value={reviewer} clear={() => setReviewer("")} />
        <Narrowing label="Work item" value={work} clear={() => setWork("")} />
      </PageToolbar>
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
              onLoadMore={paged.more ? paged.next : undefined}
              loadedNote={paged.more ? `${plural(rows.length, "change")} loaded` : undefined}
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
      onRowActivate={peekRowActivation((c: CodeChangeSummary) =>
        open(changeObject(c.repo, c.number)),
      )}
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
          cell: (c) => <ReadinessMeter change={c} nameOf={(h) => seatBadge(h).name} />,
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
          key: "findings",
          header: "Open findings",
          shrink: true,
          align: "right",
          drop: 2,
          // THE ENGINE'S COUNT, of every finding none of the change's authors
          // has answered — a merged or closed change's included, which still
          // says what was left unanswered.
          cell: (c) => <TableNumberCell value={c.open_findings} format={fmtCount} />,
        },
        {
          key: "updated",
          header: "Updated",
          shrink: true,
          align: "right",
          drop: 5,
          cell: (c) => <TableDateCell value={c.updated_at} now={now} />,
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
 * How many of a live change's conditions to merge are met, of how many: a
 * meter with its count beside it, titled by what the reader acts on next, or
 * nothing for a change that is no longer live.
 *
 * THE METER HAS A WIDTH OF ITS OWN (`.code-readiness__meter`): the kit's
 * meter fills its box and has no intrinsic size, so in a shrink column it was
 * a 4px dot, and the count says the value without a hover.
 */
export function ReadinessMeter({
  change,
  nameOf,
}: {
  change: Pick<
    CodeChangeSummary,
    "state" | "owner" | "owner_kind" | "revision" | "target_ref" | "readiness"
  >;
  nameOf: (handle: string) => string;
}) {
  const readiness = change.readiness;
  if (!changeLive(change.state) || !readiness) return <EmptyValue label="Not live" />;
  const next = firstUnready(readiness);
  const said = readinessNext(readiness, { change, nameOf });
  return (
    <span className="code-readiness" title={said}>
      <Meter
        className="code-readiness__meter"
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
        valueText={`${readinessCount(readiness)}. ${said}`}
      />
      <span className="code-readiness__count" aria-hidden="true">
        {readiness.met}/{readiness.total}
      </span>
    </span>
  );
}

/**
 * The changes that wait on a person, above the grid: the engine's
 * `needs=person` listing, the first few it keeps, each opening in the peek —
 * and nothing at all where it found none, since a band saying "nothing" every
 * visit is a band nobody reads. A band it draws no rows of claims nothing, so
 * one that stopped at its bound with `next` left is drawn as none: the grid
 * below is the listing.
 */
function PersonBand({ repo, showAll }: { repo: string; showAll: () => void }) {
  const { open } = usePeekControls();
  const seatBadge = useSeatBadgeOf();
  const params = useMemo(
    () => ({ needs: "person", limit: PERSON_BAND_ROWS, ...(repo ? { [REPO_PARAM]: repo } : {}) }),
    [repo],
  );
  const band = useCodeQuery("code_changes", params, {
    enabled: repo === "" || isRepoName(repo),
    pollMs: CHANGES_POLL_MS,
    fill: PERSON_BAND_ROWS,
  });
  const changes = (band.data?.changes ?? []).slice(0, PERSON_BAND_ROWS);
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
            {c.readiness && (
              <span className="muted">
                {" "}
                · {readinessNext(c.readiness, { change: c, nameOf: (h) => seatBadge(h).name })}
              </span>
            )}
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
  if (
    f.needs !== "" ||
    f.repo !== "" ||
    f.owner !== "" ||
    f.reviewer !== "" ||
    f.work !== "" ||
    f.window !== ""
  ) {
    return "Widen the filters to see more.";
  }
  return "A change is opened by a seat's code_open_change, or by a push with the change option.";
}
