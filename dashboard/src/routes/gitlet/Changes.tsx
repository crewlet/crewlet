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
 * kept, so one can be short, or empty with a cursor: the first read follows
 * the cursor until a row is kept, and the list pages with Load more after it,
 * each page joined to the one before it (`lib/usePaged.ts`).
 *
 * THE ENGINE KEEPS NO INDEX OF EVERY CHANGE: a listing names a state, a
 * repository, a work item, a reviewer, what its changes wait on or a window
 * they moved in, and is refused naming none. So the section asks for the OPEN changes until it is
 * told otherwise, and "Every state" is offered only where something else
 * narrows the walk — and said in place, never sent, where nothing does.
 *
 * # A row is a change as a code host lists it
 *
 * Gitlet's row (`ChangeRow.tsx`), asked with its card (`cards=true`): the
 * engine's readiness, checks, reviewers and comments, never counted here.
 *
 * # The order is the engine's walk
 *
 * Sort is `CHANGE_SORTS`, chosen only where the walk can order by it (a live
 * state that no reviewer or need leads), and said in place everywhere else.
 *
 * # Waiting on you
 *
 * Above the list, the engine's `needs=me` with why each change waits; a reader
 * bound to no seat sees what waits on a person (`needs=person`) instead.
 */

import { useId, useMemo } from "react";
import { EyeGlyph, ListGlyph, PlusGlyph, Rows2Glyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  ButtonLink,
  EmptyState,
  Popover,
  SegmentedControl,
  Select,
  Skeleton,
  StatusDot,
  Tag,
  type SegmentedOption,
} from "@crewlethq/ui";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { CHANGE_SORTS } from "~/contract/code.ts";
import {
  CHANGE_NEED_FILTERS,
  CHANGE_WINDOW_FILTERS,
  EVERY_STATE,
  changePath,
  changeStateWord,
  isChangeNeed,
  isChangeWindow,
  isRepoName,
  repoPath,
  type KnownChangeState,
} from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { indexOrg, useSeatBadgeOf } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useViewer } from "~/lib/viewer.ts";
import type {
  CodeChangeCounts,
  CodeChangeState,
  CodeChangeSummary,
  CodeChangesAnswer,
} from "~/protocol/index.ts";
import {
  ChangeRows,
  ChangeStateGlyph,
  MetaEnd,
  MetaLine,
  ReadinessMeter,
  STATE_GLYPHS,
  waitingReason,
} from "./ChangeRow.tsx";
import { CodeList, useListSort } from "./CodeList.tsx";
import { REPO_PARAM, changeDiffHref, changeObject, compareHref } from "./paths.ts";
import { RepoPicker } from "./RepoPicker.tsx";
import { Face } from "./ui.tsx";
import "~/styles/gitlet.css";

/** How often the listing's first page is read again while open: a minute, the code log's cadence. */
const CHANGES_POLL_MS = 60_000;

/** How many changes Waiting on you lists: five, a glance above the list on a laptop's screen. */
export const WAITING_ROWS = 5;

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
 * The listing's parameters for the filters the address names, or a sentence saying why it
 * cannot be asked: a name, need or window the engine does not read, a pairing it refuses,
 * or every state where nothing narrows the walk. Said in place, never sent to be refused.
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
  if (f.needs !== "" && (f.state === "merged" || f.state === "closed")) {
    return {
      unasked: `A ${f.state} change waits on nobody: pick Open, Draft or Every state, or clear Waiting on.`,
    };
  }
  if (f.needs === "me" && (f.owner !== "" || f.reviewer !== "")) {
    return {
      unasked: "Waiting on me names no owner or reviewer: clear them, or pick another Waiting on.",
    };
  }
  if (f.needs === "fix" && f.reviewer !== "") {
    return {
      unasked:
        "Waiting on a fix lists an owner's changes and names no reviewer: clear Reviewer, or pick Waiting on a review.",
    };
  }
  if (f.needs === "person" && f.reviewer !== "") {
    return {
      unasked:
        "Waiting on a person names no reviewer: clear Reviewer, or pick Waiting on a review.",
    };
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

/** A sort the engine takes for a listing of changes (`contract/code.ts`). */
export type ChangeSort = (typeof CHANGE_SORTS)[number];

/** Each sort in the words its picker says. */
const SORT_WORDS: Readonly<Record<ChangeSort, string>> = {
  opened: "Newest",
  updated: "Recently updated",
};

/** The order the engine's walk takes for these filters, and what the reader may choose of it. */
export type ChangeOrder =
  | { send: ChangeSort; choosable: true; shown: ChangeSort; byDefault: ChangeSort }
  | { send: undefined; choosable: false; shown: string; why: string };

/**
 * The order of a listing: chosen where a live state leads the walk and no reviewer
 * or need does (the engine refuses `sort` elsewhere), else the walk's own, said.
 */
export function changeOrder(f: Filters, chosen: string): ChangeOrder {
  const live = f.state === "open" || f.state === "draft";
  if (live && f.reviewer === "" && f.needs === "") {
    // AN OWNER OR A WORK ITEM keeps its exact index by opening; by move it scans the company.
    const byDefault: ChangeSort = f.owner !== "" || f.work !== "" ? "opened" : "updated";
    const shown: ChangeSort = chosen === "opened" || chosen === "updated" ? chosen : byDefault;
    return { send: shown, choosable: true, shown, byDefault };
  }
  if (f.needs === "me") {
    return {
      send: undefined,
      choosable: false,
      shown: "Reviews, then fixes",
      why: "Reviews asked of you first, then your changes awaiting a fix, each newest opened first",
    };
  }
  if (f.reviewer !== "" || f.needs !== "") {
    return {
      send: undefined,
      choosable: false,
      shown: "Newest opened",
      why: "Newest opened first: what waits on somebody is listed by when each change was opened",
    };
  }
  if (f.window !== "" && !live && f.repo === "" && f.work === "" && f.owner === "") {
    return {
      send: undefined,
      choosable: false,
      shown: "Recently moved",
      why: "Most recently moved first: a window lists its changes by when they last moved",
    };
  }
  return {
    send: undefined,
    choosable: false,
    shown: "Newest opened",
    why:
      f.state === "merged" || f.state === "closed"
        ? "Newest opened first: ended changes are listed by when they were opened"
        : "Newest opened first: every state of a repository or a work item is listed by when it was opened",
  };
}

/** The state tabs, in the order a reader looks: what is live, then what ended. */
const STATE_TABS: readonly KnownChangeState[] = ["open", "draft", "merged", "closed"];

/** The question the state totals ask: the scope's alone, the company or the picked repository. */
function countParams(repo: string): CodeParams {
  return repo !== "" && isRepoName(repo) ? { parts: "changes", repo } : { parts: "changes" };
}

/** What the foot says it shows, of the scope's total where only the scope narrows. */
export function footWords(
  f: Filters,
  shown: number,
  total: number | undefined,
  more: boolean,
): string | undefined {
  if (shown === 0) return undefined;
  const word =
    f.state === EVERY_STATE ? "" : `${changeStateWord(f.state as CodeChangeState).toLowerCase()} `;
  const filtered =
    f.owner !== "" || f.reviewer !== "" || f.needs !== "" || f.window !== "" || f.work !== "";
  if (filtered) return `Showing ${plural(shown, `${word}change`)} that match the filters`;
  if (total !== undefined && (more || total >= shown) && f.state !== EVERY_STATE) {
    return `Showing ${fmtCount(shown)} of ${plural(total, `${word}change`)}`;
  }
  return `Showing ${plural(shown, `${word}change`)}`;
}

export function Changes() {
  const [state, setState] = useParam("state", DEFAULT_STATE, "filter");
  const [needs, setNeeds] = useParam("needs", "", "filter");
  const [repo, setRepo] = useParam(REPO_PARAM, "", "filter");
  const [owner, setOwner] = useParam("owner", "", "filter");
  const [reviewer, setReviewer] = useParam("reviewer", "", "filter");
  const [work, setWork] = useParam("work", "", "filter");
  const [movedIn, setMovedIn] = useParam("window", "", "filter");
  const [sort, setSort] = useListSort();
  const [density, setDensity] = useParam("density", "", "filter");
  const nav = useNavigator();
  const viewer = useViewer();
  const filters = useMemo(
    () => ({ state, needs, repo, owner, reviewer, work, window: movedIn }),
    [state, needs, repo, owner, reviewer, work, movedIn],
  );
  const order = changeOrder(filters, sort);
  const asked = useMemo(() => changesParams(filters), [filters]);
  const params = useMemo<CodeParams>(
    () =>
      "params" in asked
        ? { ...asked.params, cards: true, ...(order.send ? { sort: order.send } : {}) }
        : {},
    [asked, order.send],
  );
  // FILLED TO ONE ROW, so a sorted walk's short first page never reads as "none".
  const first = useCodeQuery("code_changes", params, {
    enabled: "params" in asked,
    pollMs: CHANGES_POLL_MS,
    fill: 1,
  });
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_changes", { ...params, cursor }),
    changesOf,
    changeIdOf,
    changesAfter,
  );
  const counts = useCodeQuery("code_counts", countParams(repo), { pollMs: CHANGES_POLL_MS });
  const rows = paged.items;
  const narrowed = narrowsTheWalk(filters);
  // A READER BOUND TO NO SEAT is asked nothing of: what waits on a person stands in.
  const boxNeed = viewer.handle === "" ? "person" : "me";
  const total =
    state === EVERY_STATE ? undefined : counts.data?.counts?.changes?.[state as KnownChangeState];

  const placeholder =
    "unasked" in asked ? (
      <EmptyState title="Nothing to list" description={asked.unasked} />
    ) : (
      <>
        {paged.loading && <Skeleton variant="text" rows={6} label="Loading changes" />}
        <QueryState
          error={paged.error}
          refusal={paged.refusal}
          detail={first.detail}
          loading={paged.loading}
          empty={paged.more ? undefined : { title: emptyTitle(filters), hint: emptyHint(filters) }}
        />
      </>
    );

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
          value={movedIn}
          onChange={(value) => setMovedIn(String(value))}
          options={CHANGE_WINDOW_FILTERS.map((f) => ({ value: f.value, label: f.label }))}
          ariaLabel="Moved"
          active={movedIn !== ""}
        />
        <Narrowing label="Work item" value={work} clear={() => setWork("")} />
      </PageToolbar>
      <PageActions>
        <NewChange repo={repo} />
      </PageActions>
      {!viewer.loading && needs !== boxNeed && "params" in asked && (
        <WaitingBox
          need={boxNeed}
          repo={repo}
          // THE BOX'S OWN LISTING, every live state, so a draft it lists stays listed.
          showAll={() => nav.filter({ needs: boxNeed, state: EVERY_STATE })}
        />
      )}
      <ChangeRows
        rows={rows}
        label="Changes"
        density={density === "compact" ? "compact" : "default"}
        head={
          <div className="code-changes-head">
            <StateTabs
              state={state}
              setState={setState}
              offerEvery={narrowed || state === EVERY_STATE}
              counts={counts.data?.counts?.changes}
              refused={counts.error !== null}
              repo={repo !== "" && isRepoName(repo) ? repo : ""}
            />
            <div className="code-changes-head__pickers">
              <RepoPicker size="sm" repo={repo} onChoose={setRepo} every="Every repository" />
              <SeatPicker label="Owner" value={owner} onChange={setOwner} />
              <SeatPicker label="Reviewer" value={reviewer} onChange={setReviewer} />
              <SortPicker order={order} setSort={setSort} />
              <SegmentedControl
                semantics="radio"
                activate="manual"
                size="sm"
                label="Density"
                value={density === "compact" ? "compact" : "default"}
                onValueChange={(value) => setDensity(value === "compact" ? "compact" : "")}
                options={[
                  {
                    value: "default",
                    icon: <Rows2Glyph size="sm" />,
                    srLabel: "Comfortable rows",
                    title: "Comfortable rows",
                  },
                  {
                    value: "compact",
                    icon: <ListGlyph size="sm" />,
                    srLabel: "Compact rows",
                    title: "Compact rows",
                  },
                ]}
              />
            </div>
          </div>
        }
        placeholder={rows.length === 0 ? placeholder : undefined}
        paged={paged}
        foot={footWords(filters, rows.length, total, paged.more)}
      />
    </>
  );
}

/** The state tabs, each with the scope's total from `code_counts`; refused, they draw no figures. */
function StateTabs({
  state,
  setState,
  offerEvery,
  counts,
  refused,
  repo,
}: {
  state: string;
  setState: (state: string) => void;
  offerEvery: boolean;
  counts: CodeChangeCounts | undefined;
  refused: boolean;
  repo: string;
}) {
  const scope = repo === "" ? "in every repository you read" : `in ${repo}`;
  const options: SegmentedOption[] = STATE_TABS.map((s) => {
    const Glyph = STATE_GLYPHS[s];
    const word = changeStateWord(s);
    const n = counts?.[s];
    return {
      value: s,
      label: word,
      icon: <Glyph size="sm" />,
      count: n ?? null,
      title: refused
        ? "Counts are unavailable on this fleet yet"
        : n === undefined
          ? undefined
          : `${plural(n, `${word.toLowerCase()} change`)} ${scope}`,
    };
  });
  if (offerEvery) options.push({ value: EVERY_STATE, label: "Every state" });
  return (
    <SegmentedControl
      semantics="radio"
      activate="manual"
      activateHint={null}
      size="sm"
      label="Change state"
      value={state}
      onValueChange={setState}
      options={options}
    />
  );
}

/** A seat to narrow by, from the chart: anybody, you, then every seat by name. */
function SeatPicker({
  label,
  value,
  onChange,
}: {
  label: "Owner" | "Reviewer";
  value: string;
  onChange: (handle: string) => void;
}) {
  const org = useOrg();
  const me = useViewer().handle;
  const options = useMemo(() => {
    const seats = indexOrg(org)
      .seats.filter((s) => s.handle !== me)
      .map((s) => ({ value: s.handle, label: s.name || s.handle }))
      .sort((a, b) => a.label.localeCompare(b.label));
    const named = value !== "" && value !== me && !seats.some((s) => s.value === value);
    return [
      // NOBODY PICKED says what the picker narrows by, as a code host's does.
      { value: "", label },
      ...(me !== "" ? [{ value: me, label: "You" }] : []),
      ...(named ? [{ value, label: value }] : []),
      ...seats,
    ];
  }, [org, me, value]);
  return (
    <Select
      size="sm"
      width="auto"
      searchable
      value={value}
      onChange={(next) => onChange(String(next))}
      options={options}
      ariaLabel={label}
      active={value !== ""}
    />
  );
}

/** Sort: the engine's `CHANGE_SORTS` where the walk takes one, else its own order, held and said. */
function SortPicker({ order, setSort }: { order: ChangeOrder; setSort: (sort: string) => void }) {
  if (!order.choosable) {
    return (
      <Select
        size="sm"
        width="auto"
        value="walk"
        options={[{ value: "walk", label: `Sort: ${order.shown}` }]}
        ariaLabel="Sort"
        disabled
        disabledReason={order.why}
      />
    );
  }
  return (
    <Select
      size="sm"
      width="auto"
      value={order.shown}
      onChange={(next) => setSort(next === order.byDefault ? "" : String(next))}
      options={CHANGE_SORTS.map((s: ChangeSort) => ({ value: s, label: `Sort: ${SORT_WORDS[s]}` }))}
      ariaLabel="Sort"
    />
  );
}

/** How many repositories New change's picker shows before it scrolls inside its popover. */
const NEW_CHANGE_ROWS = 6;

/** New change: a repository's comparison where one is picked, else a picker of the repository. */
function NewChange({ repo }: { repo: string }) {
  const nav = useNavigator();
  if (repo !== "" && isRepoName(repo)) {
    return (
      <ButtonLink
        size="small"
        variant="secondary"
        href={compareHref(repo)}
        leadingIcon={<PlusGlyph size="sm" />}
      >
        New change
      </ButtonLink>
    );
  }
  return (
    <Popover
      label="New change"
      align="end"
      trigger={(open, toggle) => (
        <Button
          size="small"
          variant="secondary"
          leadingIcon={<PlusGlyph size="sm" />}
          aria-haspopup="dialog"
          aria-expanded={open}
          onClick={toggle}
        >
          New change
        </Button>
      )}
    >
      <div className="code-new-change">
        <p className="code-new-change__lab">Compare the branches of</p>
        <RepoPicker
          size="sm"
          repo=""
          inline={NEW_CHANGE_ROWS}
          onChoose={(name) => {
            if (name !== "") nav.to([...repoPath(name), "compare"]);
          }}
        />
      </div>
    </Popover>
  );
}

const waitKey = (c: CodeChangeSummary) => c.id;
const waitTitle = (c: CodeChangeSummary) => c.title;
const waitPeek = (c: CodeChangeSummary) => changeObject(c.repo, c.number);
const waitLead = (c: CodeChangeSummary) => <ChangeStateGlyph state={c.state} />;

/**
 * What waits on the reader, above the list: the engine's `needs=me` (or
 * `needs=person`), filled to five, each with why; nothing where it found none.
 */
function WaitingBox({
  need,
  repo,
  showAll,
}: {
  need: "me" | "person";
  repo: string;
  showAll: () => void;
}) {
  const headingId = useId();
  const me = useViewer().handle;
  const badge = useSeatBadgeOf();
  const nameOf = (handle: string) => badge(handle).name || handle;
  const params = useMemo(
    () => ({
      needs: need,
      cards: true,
      limit: WAITING_ROWS,
      ...(repo !== "" ? { [REPO_PARAM]: repo } : {}),
    }),
    [need, repo],
  );
  const box = useCodeQuery("code_changes", params, {
    enabled: repo === "" || isRepoName(repo),
    pollMs: CHANGES_POLL_MS,
    fill: WAITING_ROWS,
  });
  const rows = (box.data?.changes ?? []).slice(0, WAITING_ROWS);
  const refused = box.error !== null;
  if (!refused && rows.length === 0) return null;
  const title = need === "me" ? "Waiting on you" : "Waiting on a person";
  const more = Boolean(box.data?.next) || (box.data?.changes?.length ?? 0) > WAITING_ROWS;
  return (
    <section className="code-wait" aria-labelledby={headingId}>
      <CodeList<CodeChangeSummary>
        rows={refused ? [] : rows}
        rowKey={waitKey}
        aria-label={title}
        density="compact"
        neighbours={false}
        // THE PAGE'S LIST takes j and k on landing; the box once the reader is in it.
        keysOnceClaimed
        head={
          <div className="code-wait__head">
            <StatusDot tone="warning" />
            <h2 id={headingId} className="code-wait__title">
              {title}
            </h2>
            {!refused && (
              <span className="code-wait__count">
                {fmtCount(rows.length)}
                {more ? "+" : ""}
              </span>
            )}
            {!refused && (
              <Button size="small" variant="ghost" className="code-wait__all" onClick={showAll}>
                Show them all in the list
              </Button>
            )}
          </div>
        }
        placeholder={
          refused ? (
            <QueryState
              error={box.error}
              refusal={box.refusal}
              detail={box.detail}
              loading={false}
            />
          ) : undefined
        }
        peek={waitPeek}
        lead={waitLead}
        title={waitTitle}
        meta={(c) => (
          <MetaLine
            parts={[
              (dot) => (
                <MetaEnd key="ref" dot={dot}>
                  <span className="mono">{c.ref}</span>
                </MetaEnd>
              ),
              (dot) => (
                <MetaEnd key="reason" dot={dot}>
                  <span className="code-wait__reason">
                    <Face who={c.owner} />
                    {waitingReason(c, me, nameOf)}
                  </span>
                </MetaEnd>
              ),
            ]}
          />
        )}
        facts={(c) => (
          <span className="code-wait__facts">
            <span className="code-wait__fact">
              <ReadinessMeter change={c} nameOf={nameOf} me={me} />
            </span>
            <span className="code-wait__fact">
              <WaitAction change={c} me={me} one={rows.length === 1} />
            </span>
          </span>
        )}
      />
    </section>
  );
}

/** The box's one action a row: Review for a review owed, Open for a fix or anything else. */
function WaitAction({ change, me, one }: { change: CodeChangeSummary; me: string; one: boolean }) {
  const asked = change.card?.reviewers?.some((f) => f.handle === me && f.asked) ?? false;
  const variant = one ? "primary" : "secondary";
  return asked ? (
    <ButtonLink
      size="small"
      variant={variant}
      href={changeDiffHref(change.repo, change.number)}
      leadingIcon={<EyeGlyph size="sm" />}
    >
      Review
    </ButtonLink>
  ) : (
    <ButtonLink size="small" variant={variant} href={href(changePath(change.repo, change.number))}>
      Open
    </ButtonLink>
  );
}

/**
 * A filter the address names that the bar has no picker for (a work item,
 * arriving from a link), said as what it is, with the press that lifts it.
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
  // HOW A PERSON OPENS ONE, where they read this: the tool a seat calls is no
  // way a person can take.
  return "Compare two branches of a repository, then press Open change.";
}
