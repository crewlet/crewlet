/**
 * Gitlet › Checks: where every check stands (`code_checks` with `standing`), each check's latest run
 * on a live change or a protected branch, counted by state on the engine, a run open beside the list.
 */

import { useEffect, useMemo, useRef, type KeyboardEvent, type ReactNode } from "react";
import { SegmentedControl, Select, Skeleton, Tag, type SegmentedOption } from "@crewlethq/ui";
import {
  ArrowUpDownGlyph,
  CircleCheckGlyph,
  CircleDashedGlyph,
  CircleStopGlyph,
  CircleXGlyph,
  ClockGlyph,
  ListChecksGlyph,
} from "@crewlethq/icons/glyphs";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { href, useParam, useRoute } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { CHECK_STATES } from "~/contract/checks.ts";
import { CHECK_HISTORY_DAYS, checkStateWord } from "~/lib/checks.ts";
import { useClock, useNow } from "~/lib/clock.ts";
import { isRepoName, refName, shortOID } from "~/lib/code.ts";
import { fmtCount, fmtDuration, fmtElapsed, plural } from "~/lib/format.ts";
import { RANGE_LABEL, isRange, useTimeRange, type Offer, type Range } from "~/lib/range.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import type { CodeCheckRow, CodeChecksAnswer } from "~/protocol/index.ts";
import { CHECKS_POLL_MS, CheckPane } from "./checks/CheckList.tsx";
import { CheckMark, EndedAgo } from "./checks/marks.tsx";
import { parseRunKey, runKey, standingWords, type RunKey } from "./checks/standing.ts";
import { CodeList, useListSort } from "./CodeList.tsx";
import { REPO_PARAM } from "./paths.ts";
import { RepoPicker } from "./RepoPicker.tsx";
import { useGitletSectionCounts } from "./sectionCounts.ts";
import { BranchChip } from "./ui.tsx";
import "~/styles/gitlet.css";

/** A day by default, at most the thirty days a run is kept; no custom interval, since `standing` ends now. */
const CHECKS_OFFER: Offer = {
  screen: "gitlet.checks",
  ranges: ["1d", "7d", "30d"],
  custom: false,
  fallback: "1d",
  buckets: ["hour", "day"],
};

const HOUR_MS = 3_600_000;

/** The state tabs always drawn; the other states only where counted or named. */
const ALWAYS_TABS: readonly string[] = ["failed", "running", "passed"];

const TAB_GLYPHS: Readonly<Record<string, ReactNode>> = {
  failed: <CircleXGlyph size="sm" />,
  running: <CircleDashedGlyph size="sm" />,
  passed: <CircleCheckGlyph size="sm" />,
  queued: <ClockGlyph size="sm" />,
  errored: <CircleXGlyph size="sm" />,
  cancelled: <CircleStopGlyph size="sm" />,
  skipped: <CircleStopGlyph size="sm" />,
};

const checksOf = (a: CodeChecksAnswer) => a.checks ?? [];
const checksAfter = (a: CodeChecksAnswer) => a.next ?? "";

/** The engine's counts of a standing listing ("" is all of them); a state at its bound is a floor. */
interface Counted {
  of: (state: string) => number;
  capped: (state: string) => boolean;
}

function counted(c: CodeChecksAnswer["standing_counts"]): Counted | null {
  if (!c) return null;
  const counts = c.counts as Record<string, number | undefined>;
  const floors = new Set<string>(c.at_least ?? []);
  return {
    of: (s) =>
      s === "" ? Object.values(counts).reduce<number>((a, n) => a + (n ?? 0), 0) : (counts[s] ?? 0),
    capped: (s) => (s === "" ? floors.size > 0 : floors.has(s)),
  };
}

/** A count as said: "128+" where the engine stopped counting. */
function countWords(c: Counted, state: string): string {
  return `${fmtCount(c.of(state))}${c.capped(state) ? "+" : ""}`;
}

/** The row a key names where no loaded row holds it, with the change a loaded row of its commit judges. */
function rowOf(k: RunKey, rows: readonly CodeCheckRow[]): CodeCheckRow {
  const same = rows.find((r) => r.repo === k.repo && r.oid === k.oid && r.target === k.target);
  return {
    repo_id: "",
    repo: k.repo,
    oid: k.oid,
    target: k.target,
    name: k.name,
    attempt: k.attempt,
    state: "",
    ...(same?.change
      ? { change: same.change, source_ref: same.source_ref, change_head: same.change_head }
      : {}),
  };
}

/** The highest attempt the rows hold of the open run's check on its commit and branch, above it. */
function laterAttempt(rows: readonly CodeCheckRow[], open: CodeCheckRow): number | null {
  let later: number | null = null;
  for (const r of rows) {
    const same =
      r.repo === open.repo &&
      r.oid === open.oid &&
      r.target === open.target &&
      r.name === open.name;
    if (same && r.attempt > open.attempt && (later === null || r.attempt > later))
      later = r.attempt;
  }
  return later;
}

export function Checks() {
  useGitletSectionCounts();
  // THE CLOCK BY THE HOUR, the window's alignment: a tick re-renders nothing here.
  const hour = useClock((now) => now - (now % HOUR_MS));
  const range = useTimeRange(hour, CHECKS_OFFER);
  const [state, setState] = useParam("state", "", "filter");
  const [name, setName] = useParam("name", "", "filter");
  const [repo, setRepo] = useParam(REPO_PARAM, "", "filter");
  const [sort, setSort] = useListSort();
  const [run, setRun] = useParam("run", "", "filter");
  const route = useRoute();
  const asked = repo !== "" && isRepoName(repo) ? repo : "";
  const params = useMemo<CodeParams>(
    () => ({
      standing: sort === "recent" ? "recent" : "failing",
      since: range.since,
      ...(state ? { state } : {}),
      ...(name ? { name } : {}),
      ...(asked ? { repo: asked } : {}),
    }),
    [sort, range.since, state, name, asked],
  );
  const first = useCodeQuery("code_checks", params, { pollMs: CHECKS_POLL_MS });
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_checks", { ...params, cursor }),
    checksOf,
    runKey,
    checksAfter,
  );
  const rows = paged.items;
  const counts = counted(first.data?.standing_counts);

  const named = parseRunKey(run);
  const openRow = rows.find((r) => runKey(r) === run) ?? (named ? rowOf(named, rows) : null);
  const later = openRow ? laterAttempt(rows, openRow) : null;

  // FOCUS FOLLOWS A PRESS: to the pane's heading as it opens, back to its row as it closes.
  const list = useRef<HTMLDivElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const focus = useRef<{ to: "pane" } | { to: "row"; key: string } | null>(null);
  useEffect(() => {
    const move = focus.current;
    focus.current = null;
    if (!move) return;
    if (move.to === "pane") {
      heading.current?.focus({ preventScroll: true });
      heading.current?.scrollIntoView?.({ block: "nearest" });
      return;
    }
    const links = [...(list.current?.querySelectorAll<HTMLElement>("[data-list-link]") ?? [])];
    const back =
      links.find((l) => l.closest("[data-key]")?.getAttribute("data-key") === move.key) ?? links[0];
    back?.focus();
  }, [run]);
  const open = (key: string) => {
    focus.current = { to: "pane" };
    setRun(key);
  };
  const close = () => {
    focus.current = { to: "row", key: run };
    setRun("");
  };
  const runHref = (row: CodeCheckRow) =>
    href(route.path, { ...Object.fromEntries(route.query), run: runKey(row) });

  const scope = asked ? ` in ${asked}` : "";
  const windowWords = `last ${RANGE_LABEL[isRange(range.window) ? range.window : "1d"]}`;
  // AN OLDER FLEET refuses `standing` itself; with a filter sent, the refusal may be the filter's.
  const unserved = paged.error === "bad_params" && !state && !name && !asked;
  const placeholder = unserved ? (
    <QueryState
      error={null}
      loading={false}
      empty={{
        title: "Where checks stand is unavailable on this fleet yet",
        hint: first.detail
          ? `The engine says: ${first.detail.replace(/\.$/, "")}.`
          : "This engine does not list where checks stand.",
      }}
    />
  ) : (
    <>
      {paged.loading && <Skeleton variant="text" rows={5} label="Loading checks" />}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          paged.more
            ? undefined
            : {
                title: "Nothing has run in this window",
                hint:
                  state || name || asked
                    ? "Widen the filters to see more."
                    : "A check is listed once it is queued for a live change, a queue's candidate or a protected branch's tip.",
              }
        }
      />
    </>
  );

  return (
    <>
      <PageToolbar>
        <SegmentedControl
          semantics="radio"
          activate="manual"
          size="sm"
          label="Window"
          value={isRange(range.window) ? range.window : "1d"}
          onValueChange={(value) => range.set(value as Range)}
          options={CHECKS_OFFER.ranges.map((r) => ({ value: r, label: RANGE_LABEL[r] }))}
        />
        <StateTabs state={state} setState={setState} counts={counts} scope={asked} />
        <span className="code-checks-bar__pickers">
          <CheckPicker rows={rows} name={name} setName={setName} />
          <RepoPicker size="sm" repo={repo} onChoose={setRepo} every="Every repository" />
        </span>
      </PageToolbar>
      <div className="code-checks" data-open={openRow?.repo ? "" : undefined}>
        <div ref={list} className="code-checks__list">
          <CodeList<CodeCheckRow>
            rows={rows}
            rowKey={runKey}
            aria-label="Where checks stand"
            head={
              <div className="code-checks-head">
                <p className="code-checks-head__words">
                  {!unserved && (
                    <HeadWords
                      counts={counts}
                      state={state}
                      name={name}
                      scope={scope}
                      windowWords={windowWords}
                    />
                  )}
                </p>
                <Select
                  size="sm"
                  width="auto"
                  value={sort === "recent" ? "recent" : ""}
                  onChange={(next) => setSort(String(next))}
                  ariaLabel="Sort"
                  options={[
                    {
                      value: "",
                      label: "Failing first",
                      leading: <ArrowUpDownGlyph size="sm" />,
                    },
                    {
                      value: "recent",
                      label: "Newest first",
                      leading: <ArrowUpDownGlyph size="sm" />,
                      description: "What runs and waits first, then the newest endings",
                    },
                  ]}
                />
              </div>
            }
            placeholder={rows.length === 0 ? placeholder : undefined}
            rowHref={runHref}
            isSelected={(r) => runKey(r) === run}
            onOpen={(r) => (runKey(r) === run ? close() : open(runKey(r)))}
            lead={(r) => <CheckMark state={r.state} />}
            title={(r) => <span className="code-checks-row__name">{r.name}</span>}
            titleAfter={(r) => (
              <>
                {r.required && (
                  <Tag size="xs" appearance="outline">
                    Required
                  </Tag>
                )}
                {r.attempt > 1 && <span className="t-caption">{`attempt ${r.attempt}`}</span>}
              </>
            )}
            meta={(r) => <RowMeta row={r} />}
            facts={(r) => <RowFacts row={r} />}
            paged={paged}
            moreLabel="Load more runs"
            foot={
              rows.length > 0 ? (
                <FootWords loaded={rows.length} more={paged.more} counts={counts} state={state} />
              ) : undefined
            }
          />
        </div>
        {openRow?.repo && (
          <div
            className="code-checks__pane"
            onKeyDown={(e: KeyboardEvent) => {
              if (e.key === "Escape" && !e.defaultPrevented) {
                e.preventDefault();
                close();
              }
            }}
          >
            <CheckPane
              key={runKey(openRow)}
              row={openRow}
              repo={openRow.repo}
              onClose={close}
              headingRef={heading}
              later={later}
              onOpenLater={(attempt) => open(runKey({ ...openRow, attempt }))}
            />
          </div>
        )}
      </div>
    </>
  );
}

/** The state tabs, each counted by the engine; All, Failed, Running and Passed always. */
function StateTabs({
  state,
  setState,
  counts,
  scope,
}: {
  state: string;
  setState: (state: string) => void;
  counts: Counted | null;
  scope: string;
}) {
  const where = scope ? `in ${scope}` : "in every repository you read";
  const tab = (value: string, label: string): SegmentedOption => ({
    value,
    label,
    ...(TAB_GLYPHS[value] ? { icon: TAB_GLYPHS[value] } : {}),
    count: counts ? counts.of(value) : null,
    capped: counts?.capped(value) ?? false,
    title: counts
      ? `${countWords(counts, value)} ${value ? `${checkStateWord(value)} ` : ""}${counts.of(value) === 1 ? "run" : "runs"} ${where}`
      : "Counts are unavailable on this fleet yet",
  });
  const options: SegmentedOption[] = [tab("", "All")];
  for (const s of [...ALWAYS_TABS, ...CHECK_STATES.filter((s) => !ALWAYS_TABS.includes(s))]) {
    if (ALWAYS_TABS.includes(s) || state === s || (counts?.of(s) ?? 0) > 0) {
      const word = checkStateWord(s);
      options.push(tab(s, word.charAt(0).toUpperCase() + word.slice(1)));
    }
  }
  // A STATE THE ADDRESS NAMES THAT THIS BUILD HAS NO WORD FOR, as the engine spells it.
  if (state && !options.some((o) => o.value === state)) options.push(tab(state, state));
  return (
    <SegmentedControl
      semantics="radio"
      activate="manual"
      activateHint={null}
      size="sm"
      label="State"
      value={state}
      onValueChange={setState}
      options={options}
    />
  );
}

/** Check: every name on the runs loaded and the one chosen; no question lists a company's checks. */
function CheckPicker({
  rows,
  name,
  setName,
}: {
  rows: readonly CodeCheckRow[];
  name: string;
  setName: (name: string) => void;
}) {
  const options = useMemo(() => {
    const names = new Set(rows.map((r) => r.name));
    if (name) names.add(name);
    return [
      { value: "", label: "Every check", leading: <ListChecksGlyph size="sm" /> },
      ...[...names].sort().map((n) => ({
        value: n,
        label: n,
        leading: <ListChecksGlyph size="sm" />,
        group: "Checks on the runs loaded",
      })),
    ];
  }, [rows, name]);
  return (
    <Select
      size="sm"
      width="auto"
      searchable
      value={name}
      onChange={(next) => setName(String(next))}
      options={options}
      ariaLabel="Check"
      active={name !== ""}
    />
  );
}

/** "12 latest runs of e2e on live changes and protected branches in web, last 24 hours". */
function HeadWords({
  counts,
  state,
  name,
  scope,
  windowWords,
}: {
  counts: Counted | null;
  state: string;
  name: string;
  scope: string;
  windowWords: string;
}) {
  const of = name ? ` of ${name}` : "";
  const where = `on live changes and protected branches${scope}, ${windowWords}`;
  if (!counts) return <>{`Latest runs${of} ${where}`}</>;
  if (state) {
    return (
      <>
        <strong>{countWords(counts, state)}</strong>
        {` ${checkStateWord(state)} of the ${countWords(counts, "")} latest runs${of} ${where}`}
      </>
    );
  }
  return (
    <>
      <strong>{countWords(counts, "")}</strong>
      {` latest ${counts.of("") === 1 ? "run" : "runs"}${of} ${where}`}
    </>
  );
}

/** Under the rows: how many are loaded while pages remain, then where earlier runs are. */
function FootWords({
  loaded,
  more,
  counts,
  state,
}: {
  loaded: number;
  more: boolean;
  counts: Counted | null;
  state: string;
}) {
  const total =
    more && counts ? `${fmtCount(loaded)} of ${countWords(counts, state)} runs loaded. ` : "";
  return (
    <>
      {total}
      {`The latest run of each check. Earlier runs are on each change's and repository's Checks tab, kept ${plural(CHECK_HISTORY_DAYS, "day")}.`}
    </>
  );
}

/** The repository, the branch, the commit and what it judged: "api backend/orders 9b2e4d7 for api!17". */
function RowMeta({ row }: { row: CodeCheckRow }) {
  const said = standingWords(row);
  const branch = row.source_ref && row.purpose !== "tip" ? row.source_ref : row.target;
  return (
    <>
      <span>{row.repo ?? row.repo_id}</span>
      <BranchChip name={refName(branch).name} />
      <span className="mono code-checks-row__oid">{shortOID(row.change_head ?? row.oid)}</span>
      <span>
        {said.before}
        {said.strong && <strong>{said.strong}</strong>}
        {said.after}
      </span>
    </>
  );
}

/** The duration, then the time after its state word. */
function RowFacts({ row }: { row: CodeCheckRow }) {
  return (
    <span className="code-checks-row__facts">
      <RowDuration row={row} />
      {row.finished_at ? (
        <EndedAgo word="ended" at={row.finished_at} />
      ) : row.started_at ? (
        <EndedAgo word="started" at={row.started_at} />
      ) : row.queued_at ? (
        <EndedAgo word="queued" at={row.queued_at} />
      ) : null}
    </span>
  );
}

/** "1m 52s"; a running run's clock ticking; nothing for a queued run or an ended one with none. */
function RowDuration({ row }: { row: CodeCheckRow }) {
  if (row.state === "running" && row.started_at && !row.finished_at) {
    return <RunningClock since={row.started_at} />;
  }
  if (!row.finished_at || row.duration_ms === undefined) return null;
  return (
    <span className="code-checks-row__took">
      <ClockGlyph size="xs" />
      {fmtDuration(row.duration_ms)}
    </span>
  );
}

/** A running run's elapsed time, reading the clock alone so a tick re-renders this cell. */
function RunningClock({ since }: { since: string }) {
  const now = useNow();
  return (
    <span className="code-checks-row__took" data-tone="info">
      <ClockGlyph size="xs" />
      {fmtElapsed(now - Date.parse(since))}
    </span>
  );
}
