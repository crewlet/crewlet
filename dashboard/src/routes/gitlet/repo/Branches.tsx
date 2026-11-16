/**
 * A repository's branches and tags as tables: each branch with who moved it and when, its checks,
 * its distance from the default branch and its change, in views by when each moved; tags under them.
 */

import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Count,
  EmptyValue,
  IconButton,
  IconButtonLink,
  Input,
  SegmentedControl,
  Skeleton,
  Tag,
  VisuallyHidden,
  useClipboard,
} from "@crewlethq/ui";
import {
  BookmarkGlyph,
  CheckGlyph,
  ClockGlyph,
  CopyGlyph,
  GitBranchGlyph,
  GitCompareGlyph,
  ListGlyph,
  RotateCcwClockGlyph,
  SearchGlyph,
  ShieldCheckGlyph,
} from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { href, useDraft, useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { ago } from "~/lib/ago.ts";
import { useClock, useNow } from "~/lib/clock.ts";
import { CHECK_HISTORY_DAYS, rollupFor, rollupTitle } from "~/lib/checks.ts";
import { PUSH_HISTORY_DAYS, changeRef, refName, repoPath, shortOID } from "~/lib/code.ts";
import { civilAt, civilKey, fmtDateTime, plural, readerDay } from "~/lib/format.ts";
import { zone } from "~/lib/prefs.ts";
import { dayStartIn } from "~/lib/range.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import {
  QueryError,
  type CodeCommitMark,
  type CodeRefListing,
  type CodeTip,
  type CodeRefMover,
  type CodeRefProtection,
  type CodeRefRow,
  type CodeRefsAnswer,
  type CodeRepoDetail,
} from "~/protocol/index.ts";
import { ChangeRefPill } from "../Changes.tsx";
import { useListKeys } from "../CodeList.tsx";
import { compareHref, repoHref } from "../paths.ts";
import {
  BranchChip,
  CheckRollupMark,
  Face,
  GITLET,
  ShaChip,
  useWhoName,
  type Who,
} from "../ui.tsx";
import { DeleteBranchButton, NewBranchButton } from "./BranchWrites.tsx";
import { useCommitMarks } from "./marks.ts";
import { ProtectButton } from "./Protect.tsx";
import "~/styles/gitlet.css";

/** A branch moved in the last 90 days is active; the rest are stale. */
const ACTIVE_DAYS = 90;

const DAY_MS = 86_400_000;
const HEADS = "refs/heads/";
/** Where a repository's tags are listed (`gitlet.ReadQuery.Prefix`). */
const TAG_PREFIX = "refs/tags/";
/** A page of a view: its heads stay under the 100 one marks or tips read takes. */
const PAGE_LIMIT = 50;
/** The Overview's active branches, the default branch left out. */
const OVERVIEW_ACTIVE = 10;
/** A page of the find that looks for the default branch by its name. */
const DEFAULT_FIND_LIMIT = 20;
/** How long typing rests before a find is written to the address. */
const FIND_PAUSE_MS = 250;

type View = "overview" | "active" | "stale" | "all";

const VIEWS: readonly { value: View; label: string; icon: ReactNode }[] = [
  { value: "overview", label: "Overview", icon: <GitBranchGlyph size="sm" /> },
  { value: "active", label: "Active", icon: <ClockGlyph size="sm" /> },
  { value: "stale", label: "Stale", icon: <RotateCcwClockGlyph size="sm" /> },
  { value: "all", label: "All", icon: <ListGlyph size="sm" /> },
];

const viewOf = (raw: string): View =>
  VIEWS.find((v) => v.value === raw && raw !== "overview")?.value ?? "overview";

/** The Branches tab at a view: `view=` beside the tab. */
const viewHref = (repo: string, view: View) =>
  href(repoPath(repo), { tab: "branches", ...(view === "overview" ? {} : { view }) });

/** The first instant of the reader's day ACTIVE_DAYS back, RFC 3339. */
function activeSince(today: string): string | undefined {
  const todayAt = civilAt(today);
  if (todayAt === null) return undefined;
  const start = dayStartIn(civilKey(todayAt - ACTIVE_DAYS * DAY_MS), zone());
  return start === null ? undefined : new Date(start).toISOString();
}

/** The repository's branches in views by when each moved, and its tags. */
export function Branches({ repo }: { repo: CodeRepoDetail }) {
  const [rawView, setView] = useParam("view", "");
  const [q, setQ] = useParam("q", "");
  const view = viewOf(rawView);
  const since = activeSince(useClock((t) => readerDay(t)));
  return (
    <>
      <PageToolbar>
        <SegmentedControl
          size="sm"
          semantics="radio"
          activate="manual"
          label="Which branches"
          value={view}
          onValueChange={(v) => setView(v === "overview" ? "" : v)}
          options={VIEWS.map((v) => ({ value: v.value, label: v.label, icon: v.icon }))}
        />
        <span className="spacer" />
        <FindBranch q={q} onQ={setQ} />
        <NewBranchButton repo={repo} />
      </PageToolbar>
      <div className="code-refs-boxes">
        {view === "overview" && !q ? (
          <Overview repo={repo} since={since} />
        ) : (
          <BranchView repo={repo} view={view} q={q} since={since} />
        )}
      </div>
    </>
  );
}

/** Find a branch: written to the address once typing rests, cleared by Escape. */
function FindBranch({ q, onQ }: { q: string; onQ: (q: string) => void }) {
  const [typed, setTyped] = useDraft(q);
  useEffect(() => {
    if (typed.trim() === q) return undefined;
    const t = setTimeout(() => onQ(typed.trim()), FIND_PAUSE_MS);
    return () => clearTimeout(t);
  }, [typed, q, onQ]);
  return (
    <span className="code-refs-find">
      <Input
        type="search"
        inputSize="sm"
        width="full"
        value={typed}
        onChange={(e) => setTyped(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Escape" && typed !== "") {
            e.preventDefault();
            setTyped("");
            onQ("");
          }
        }}
        leading={<SearchGlyph size="sm" />}
        placeholder="Find a branch"
        aria-label="Find a branch"
      />
    </span>
  );
}

type Params = Record<string, string | number | boolean>;

/** A listing's parameters: by when each moved, read in detail. */
function listingParams(
  repo: string,
  view: View,
  q: string,
  since: string | undefined,
  limit: number,
): Params {
  return {
    name: repo,
    order: "moved",
    detail: true,
    limit,
    ...(view === "active" && since ? { moved_since: since } : {}),
    ...(view === "stale" && since ? { moved_before: since } : {}),
    ...(q ? { contains: q } : {}),
  };
}

/** The Overview: the default branch, the active branches, and the newest tags. */
function Overview({ repo, since }: { repo: CodeRepoDetail; since: string | undefined }) {
  const found = useDefaultBranch(repo);
  const active = useActiveBranches(repo, since);
  // ONE MARKS READ for the heads of both boxes, asked once both are here.
  const heads =
    found.loading || active.read.loading
      ? ""
      : [found.row?.oid, ...active.rows.map((r) => r.oid)].filter(Boolean).join(",");
  const facts = useBranchFacts(repo, useHeadPages(heads === "" ? [] : [heads]));
  return (
    <>
      <FactsNote facts={facts} repo={repo} />
      <DefaultBox repo={repo} found={found} facts={facts} />
      <ActiveBox repo={repo} active={active} facts={facts} />
      <Tags repo={repo} />
    </>
  );
}

/** The Default branch box: the default branch found by its name. */
function DefaultBox({
  repo,
  found,
  facts,
}: {
  repo: CodeRepoDetail;
  found: ReturnType<typeof useDefaultBranch>;
  facts: Facts;
}) {
  const now = useNow();
  return (
    <RefsBox
      title="Default branch"
      keys={[HEADS + repo.default_branch]}
      loading={found.loading}
      loadingLabel={`Loading the default branch of ${repo.name}`}
      error={found.error}
      refusal={found.refusal}
      detail={found.detail}
      columns="default"
    >
      {found.row ? (
        <BranchRow repo={repo} row={found.row} now={now} facts={facts} />
      ) : (
        <UnreadDefault repo={repo} />
      )}
    </RefsBox>
  );
}

/** The default branch read in detail: found page by page, since a find matches any part of a name. */
function useDefaultBranch(repo: CodeRepoDetail) {
  const params = useMemo(
    () => ({
      name: repo.name,
      contains: repo.default_branch,
      detail: true,
      limit: DEFAULT_FIND_LIMIT,
    }),
    [repo.name, repo.default_branch],
  );
  const first = useCodeQuery("code_refs", params);
  const want = HEADS + repo.default_branch;
  const inFirst = first.data?.refs_detail?.find((r) => r.ref === want);
  const next = first.data?.next ?? "";
  const key = `${JSON.stringify(params)}\n${next}`;
  const [later, setLater] = useState<{ key: string; row: CodeRefListing | null } | null>(null);
  useEffect(() => {
    if (inFirst || !next || later?.key === key) return undefined;
    let live = true;
    void (async () => {
      let cursor = next;
      let row: CodeRefListing | null = null;
      while (cursor && !row) {
        const page: CodeRefsAnswer | null = await codeRest
          .query("code_refs", { ...params, cursor })
          .catch(() => null);
        row = page?.refs_detail?.find((r) => r.ref === want) ?? null;
        cursor = page?.next ?? "";
      }
      if (live) setLater({ key, row });
    })();
    return () => {
      live = false;
    };
  }, [inFirst, next, key, later, params, want]);
  const searching = !inFirst && next !== "" && later?.key !== key;
  return {
    row: inFirst ?? (later?.key === key ? later.row : null),
    loading: first.loading || searching,
    error: first.error,
    refusal: first.refusal,
    detail: first.detail,
  };
}

/** The default branch drawn from the repository's own detail, where its listing did not find it. */
function UnreadDefault({ repo }: { repo: CodeRepoDetail }) {
  return (
    <tr data-key={HEADS + repo.default_branch}>
      <td className="code-refs__name">
        <span className="code-refs__marks">
          <a className="code-refs__link" href={repoHref(repo.name)} data-list-link>
            <BranchChip name={repo.default_branch} />
          </a>
          <Tag appearance="outline" size="xs">
            Default
          </Tag>
        </span>
      </td>
      <td className="code-refs__updated">
        <EmptyValue label="Not read" />
      </td>
      <td className="code-refs__checks" />
      <td className="code-refs__drift" />
      <td className="code-refs__change" />
      <td className="code-refs__actions" />
    </tr>
  );
}

/** The Overview's active branches: up to ten, the default branch left out, and their count. */
function useActiveBranches(repo: CodeRepoDetail, since: string | undefined) {
  const params = useMemo(
    () => ({
      ...listingParams(repo.name, "active", "", since, OVERVIEW_ACTIVE + 1),
      ...(since ? { active_since: since } : {}),
    }),
    [repo.name, since],
  );
  const read = useCodeQuery("code_refs", params);
  const rows = (read.data?.refs_detail ?? [])
    .filter((r) => r.ref !== HEADS + repo.default_branch)
    .slice(0, OVERVIEW_ACTIVE);
  return { read, rows };
}

/** The Overview's Active branches box, and the way to all of them. */
function ActiveBox({
  repo,
  active,
  facts,
}: {
  repo: CodeRepoDetail;
  active: ReturnType<typeof useActiveBranches>;
  facts: Facts;
}) {
  const { read, rows } = active;
  const now = useNow();
  const count = read.data?.active_branches;
  const more = count ? count.more === true || count.count > rows.length : Boolean(read.data?.next);
  return (
    <RefsBox
      title="Active branches"
      count={count}
      keys={rows.map((r) => r.ref)}
      loading={read.loading}
      loadingLabel={`Loading the active branches of ${repo.name}`}
      error={read.error}
      refusal={read.refusal}
      detail={read.detail}
      empty={rows.length === 0 ? activeEmpty(repo) : undefined}
      foot={
        more ? (
          <a className="t-link" href={viewHref(repo.name, "active")}>
            View all active branches
          </a>
        ) : undefined
      }
    >
      {rows.map((r) => (
        <BranchRow key={r.ref} repo={repo} row={r} now={now} facts={facts} />
      ))}
    </RefsBox>
  );
}

/** No active branch, with the way to the stale ones. */
const activeEmpty = (repo: CodeRepoDetail) => ({
  ...EMPTY.active,
  action: (
    <a className="t-link" href={viewHref(repo.name, "stale")}>
      Stale branches
    </a>
  ),
});

const EMPTY = {
  active: {
    title: `No branch moved in the last ${ACTIVE_DAYS} days`,
    hint: "Stale lists the branches nobody has moved since.",
  },
  stale: { title: "No stale branch", hint: `Every branch moved in the last ${ACTIVE_DAYS} days.` },
  all: { title: "No branch yet", hint: "A branch is listed once a push that creates it applies." },
};

const TITLES = { active: "Active branches", stale: "Stale branches", all: "All branches" };

/** Active, Stale, All, or a find: one box, a page at a time. */
function BranchView({
  repo,
  view,
  q,
  since,
}: {
  repo: CodeRepoDetail;
  view: View;
  q: string;
  since: string | undefined;
}) {
  const shown = view === "overview" ? "all" : view;
  const params = useMemo(
    () => listingParams(repo.name, shown, q, since, PAGE_LIMIT),
    [repo.name, shown, q, since],
  );
  // ONLY A FIRST PAGE COUNTS THE ACTIVE BRANCHES: the engine refuses the count on a cursor.
  const counted = useMemo(
    () => (shown === "active" && since ? { ...params, active_since: since } : params),
    [params, shown, since],
  );
  const first = useCodeQuery("code_refs", counted);
  const paged = usePaged(
    first,
    counted,
    (cursor) => codeRest.query("code_refs", { ...params, cursor }),
    (a: CodeRefsAnswer) => a.refs_detail ?? [],
    (r: CodeRefListing) => r.ref,
    (a: CodeRefsAnswer) => a.next ?? "",
  );
  const now = useNow();
  // THE ENGINE'S ACTIVE COUNT LEAVES THE DEFAULT BRANCH OUT, and so do the rows it counts.
  const rows =
    shown === "active"
      ? paged.items.filter((r) => r.ref !== HEADS + repo.default_branch)
      : paged.items;
  const facts = useBranchFacts(
    repo,
    useHeadPages(paged.answers.map((a) => (a.refs_detail ?? []).map((r) => r.oid).join(","))),
  );
  const title = q ? `Branches matching “${q}”` : TITLES[shown];
  const empty = q
    ? {
        title: `No branch name holds “${q}”`,
        hint: "A find matches any part of a name, in any case.",
      }
    : shown === "active"
      ? activeEmpty(repo)
      : EMPTY[shown];
  // A FIND'S PAGE CAN HOLD NOTHING YET HAVE MORE: the engine reads at most 10,000 refs a page.
  const nothingYet = rows.length === 0 && paged.more;
  return (
    <>
      <FactsNote facts={facts} repo={repo} />
      <RefsBox
        title={title}
        count={
          q
            ? undefined
            : shown === "all"
              ? { count: repo.branches }
              : (first.data?.active_branches ?? undefined)
        }
        keys={rows.map((r) => r.ref)}
        loading={paged.loading && rows.length === 0}
        loadingLabel={`Loading the ${title.toLowerCase()} of ${repo.name}`}
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        empty={rows.length === 0 && !paged.more ? empty : undefined}
        foot={
          paged.more || paged.pageError ? (
            <>
              {nothingYet && <span className="t-caption">None among the branches read so far</span>}
              {paged.more && (
                <Button
                  size="small"
                  variant="secondary"
                  loading={paged.paging}
                  onClick={paged.next}
                >
                  {nothingYet ? "Keep looking" : "Load more branches"}
                </Button>
              )}
              {paged.more && rows.length > 0 && (
                <span className="t-caption">
                  {plural(rows.length, "branch", "branches")} loaded
                </span>
              )}
              {paged.pageError && <PageFailed error={paged.pageError} />}
            </>
          ) : undefined
        }
      >
        {rows.map((r) => (
          <BranchRow key={r.ref} repo={repo} row={r} now={now} facts={facts} />
        ))}
      </RefsBox>
    </>
  );
}

type Columns = "branch" | "default";

/** One box of branches: a table titled in its first head cell, its rows walked with `j`, `k` and Enter. */
function RefsBox({
  title,
  count,
  keys,
  loading,
  loadingLabel,
  error,
  refusal,
  detail,
  empty,
  foot,
  columns = "branch",
  children,
}: {
  title: string;
  count?: { count: number; more?: boolean } | undefined;
  keys: readonly string[];
  loading: boolean;
  loadingLabel: string;
  error: Parameters<typeof QueryState>[0]["error"];
  refusal?: Parameters<typeof QueryState>[0]["refusal"];
  detail?: string | undefined;
  empty?: { title: ReactNode; hint: ReactNode; action?: ReactNode } | undefined;
  foot?: ReactNode;
  columns?: Columns;
  children: ReactNode;
}) {
  const id = useId();
  const box = useRef<HTMLElement>(null);
  useListKeys({ root: box, keys });
  const failed = error !== null;
  return (
    <section className="code-refs" aria-labelledby={id} ref={box}>
      {(loading || failed || empty) && (
        <h2 id={id} className="code-refs__caption">
          {title}
        </h2>
      )}
      {loading ? (
        <Skeleton variant="table" rows={3} label={loadingLabel} />
      ) : (
        <QueryState error={error} refusal={refusal} detail={detail} loading={false} empty={empty}>
          <table className="code-refs__table" aria-labelledby={id}>
            <thead>
              <tr>
                <th scope="col" id={id} className="code-refs__title">
                  <span className="code-refs__heading">
                    {title}
                    {count && (
                      <Count
                        value={count.more ? `${count.count.toLocaleString()}+` : count.count}
                        label={title.toLowerCase()}
                      />
                    )}
                  </span>
                </th>
                <th scope="col">Updated</th>
                <th scope="col">Checks</th>
                {columns === "default" ? (
                  <>
                    <td />
                    <td />
                  </>
                ) : (
                  <>
                    <th scope="col">Behind | Ahead</th>
                    <th scope="col">Change</th>
                  </>
                )}
                <th scope="col">
                  <VisuallyHidden>Actions</VisuallyHidden>
                </th>
              </tr>
            </thead>
            <tbody>{children}</tbody>
          </table>
        </QueryState>
      )}
      {foot && <div className="code-refs__foot">{foot}</div>}
    </section>
  );
}

/** A branch as an address names it: none for the default branch, its name for any other. */
function branchRev(repo: CodeRepoDetail, ref: string): string {
  const name = refName(ref).name;
  return name === repo.default_branch ? "" : name;
}

/** A branch's name copied by one press, the control saying it landed. */
function CopyName({ name }: { name: string }) {
  const { state, copy } = useClipboard({ failedResetMs: null });
  return (
    <IconButton
      label={
        state === "copied"
          ? `Copied ${name}`
          : state === "failed"
            ? "The browser refused the clipboard"
            : `Copy the name ${name}`
      }
      icon={state === "copied" ? <CheckGlyph /> : <CopyGlyph />}
      size="sm"
      variant="ghost"
      onClick={() => void copy(name)}
    />
  );
}

/** A branch's rules in words: "Protected by release/*: no force-push, no delete, pushes through changes only". */
function protectionTitle(p: CodeRefProtection): string {
  const clauses = [
    p.allow_force ? "force-push allowed" : "no force-push",
    p.allow_delete ? "delete allowed" : "no delete",
    p.direct_push === "none"
      ? "pushes through changes only"
      : p.direct_push === "persons"
        ? "persons push directly"
        : `direct pushes by ${p.direct_push}`,
  ];
  const names = p.patterns.map((pattern) =>
    pattern.startsWith(HEADS) ? pattern.slice(HEADS.length) : pattern,
  );
  return `Protected by ${names.join(" and ")}: ${clauses.join(", ")}`;
}

/** One branch: its name and marks, who moved it and when, its change, and its controls. */
function BranchRow({
  repo,
  row,
  now,
  facts,
}: {
  repo: CodeRepoDetail;
  row: CodeRefListing;
  now: number;
  facts: Facts;
}) {
  const name = refName(row.ref).name;
  const rev = branchRev(repo, row.ref);
  const isDefault = rev === "";
  return (
    <tr data-key={row.ref}>
      <td className="code-refs__name">
        <span className="code-refs__marks">
          <a
            className="code-refs__link"
            href={repoHref(repo.name, { ref: rev })}
            title={`${name} at ${shortOID(row.oid)}`}
            data-list-link
          >
            <BranchChip name={name} />
          </a>
          {isDefault && (
            <Tag appearance="outline" size="xs">
              Default
            </Tag>
          )}
          {row.protection && (
            <span className="code-refs__rule" title={protectionTitle(row.protection)}>
              <Tag appearance="outline" size="xs" leadingIcon={<ShieldCheckGlyph size="xs" />}>
                Protected
              </Tag>
            </span>
          )}
        </span>
      </td>
      <td className="code-refs__updated">
        <MovedBy row={row} now={now} />
      </td>
      <td className="code-refs__checks">
        <BranchChecks repo={repo} row={row} mark={facts.mark(row.oid)} now={now} />
      </td>
      <td className="code-refs__drift">
        {!isDefault && row.oid !== repo.head && (
          <Drift tip={facts.tip(row.oid)} most={facts.most} base={repo.default_branch} />
        )}
      </td>
      <td className="code-refs__change">
        {!isDefault && row.change && (
          <ChangeRefPill
            change={{
              repo: repo.name,
              ref: changeRef(repo.name, row.change.number),
              title: row.change.title,
              state: row.change.state,
            }}
          />
        )}
      </td>
      <td className="code-refs__actions">
        <span className="code-refs__controls">
          <CopyName name={name} />
          {!isDefault && (
            <IconButtonLink
              href={compareHref(repo.name, { head: rev })}
              icon={<GitCompareGlyph />}
              label={`Compare ${name} with ${repo.default_branch}`}
              size="sm"
              variant="ghost"
            />
          )}
          {row.protection ? (
            <ButtonLink
              href={repoHref(repo.name, { tab: "settings" })}
              size="small"
              variant="secondary"
              leadingIcon={<ShieldCheckGlyph size="sm" />}
            >
              Protection rule
            </ButtonLink>
          ) : (
            <ProtectButton repo={repo} pattern={row.ref} label="Protect" />
          )}
          <DeleteBranchButton
            repo={repo}
            row={{
              ref: row.ref,
              oid: row.oid,
              held: row.may_delete ? undefined : row.delete_detail || undefined,
            }}
          />
        </span>
      </td>
    </tr>
  );
}

/** Each loaded page's heads, once each: a list of whole ids per page, kept while the pages say the same. */
function useHeadPages(pages: readonly string[]): readonly (readonly string[])[] {
  const key = pages.join("\n");
  return useMemo(
    () =>
      key === "" ? [] : key.split("\n").map((p) => [...new Set(p.split(",").filter(Boolean))]),
    [key],
  );
}

/** What each head carries beside its row: its checks, how far it is from the default branch, and why either is missing. */
interface Facts {
  mark: (oid: string) => CodeCommitMark | null | undefined;
  tip: (oid: string) => CodeTip | undefined;
  /** The most commits any loaded head is behind or ahead, which the bars are drawn against. */
  most: number;
  marksFailed: string | null;
  tipsFailed: string | null;
}

/** The engine's sentence for a refused read, else its code. */
function sentenceOf(err: unknown): string {
  if (!(err instanceof QueryError)) return "the read failed";
  const refusal = err.refusal as { detail?: string | null } | null;
  return err.detail ?? refusal?.detail ?? err.message;
}

/** How far each page's heads are from the default branch's head: whole ids both sides, so kept by the tab. */
function useTips(
  repo: string,
  base: string | undefined,
  pages: readonly (readonly string[])[],
): { of: (oid: string) => CodeTip | undefined; failed: string | null } {
  const [held, setHeld] = useState<Readonly<Record<string, CodeTip[] | { failed: string }>>>({});
  const asks = useMemo(
    () =>
      base
        ? pages
            .map((oids) => oids.filter((o) => o !== base))
            .filter((oids) => oids.length > 0)
            .map((oids) => ({ key: `${repo}\n${base}\n${oids.join(",")}`, oids: oids.join(",") }))
        : [],
    [repo, base, pages],
  );
  const asking = useRef(new Map<string, AbortController>());
  useEffect(() => {
    for (const { key, oids } of asks) {
      if (key in held || asking.current.has(key)) continue;
      const abort = new AbortController();
      asking.current.set(key, abort);
      codeRest
        .query("code_tips", { name: repo, oids, base: base ?? "" }, abort.signal)
        .then(
          (a): CodeTip[] | { failed: string } => a.tips?.tips ?? [],
          (err: unknown) => ({ failed: sentenceOf(err) }),
        )
        .then((page) => {
          if (abort.signal.aborted) return;
          asking.current.delete(key);
          setHeld((h) => ({ ...h, [key]: page }));
        });
    }
  }, [asks, held, repo, base]);
  useEffect(() => {
    const running = asking.current;
    return () => {
      for (const abort of running.values()) abort.abort();
      running.clear();
    };
  }, []);
  const byOid = useMemo(() => {
    const out = new Map<string, CodeTip>();
    for (const { key } of asks) {
      const page = held[key];
      if (Array.isArray(page)) for (const t of page) out.set(t.oid, t);
    }
    return out;
  }, [asks, held]);
  const failed =
    asks
      .map(({ key }) => held[key])
      .map((page) => (page && !Array.isArray(page) ? page.failed : null))
      .find((f) => f !== null) ?? null;
  return { of: (oid) => byOid.get(oid), failed };
}

/** The checks and the drift of every head the pages name. */
function useBranchFacts(repo: CodeRepoDetail, pages: readonly (readonly string[])[]): Facts {
  const marks = useCommitMarks(repo.name, pages);
  const tips = useTips(repo.name, repo.head, pages);
  let most = 0;
  for (const oids of pages) {
    for (const oid of oids) {
      const t = tips.of(oid);
      most = Math.max(most, t?.behind ?? 0, t?.ahead ?? 0);
    }
  }
  return {
    mark: marks.of,
    tip: tips.of,
    most,
    marksFailed: marks.failed ? (marks.failed.sentence ?? marks.failed.code) : null,
    tipsFailed: tips.failed,
  };
}

/** Why the checks or the drift are missing, said once above the boxes. */
function FactsNote({ facts, repo }: { facts: Facts; repo: CodeRepoDetail }) {
  const { marksFailed, tipsFailed } = facts;
  if (!marksFailed && !tipsFailed) return null;
  const drift = `how far each branch is from ${repo.default_branch}`;
  const what =
    marksFailed && tipsFailed
      ? `Checks and ${drift} are`
      : marksFailed
        ? "Checks are"
        : `${drift[0]!.toUpperCase()}${drift.slice(1)} is`;
  return (
    <p className="code-refs-note" role="status">
      {what} unavailable: {marksFailed ?? tipsFailed}.
    </p>
  );
}

/** A branch's checks: its head's, for itself or else for the default branch; never a cross for none. */
function BranchChecks({
  repo,
  row,
  mark,
  now,
}: {
  repo: CodeRepoDetail;
  row: CodeRefListing;
  mark: CodeCommitMark | null | undefined;
  now: number;
}) {
  if (mark === undefined) return null;
  const rollup = rollupFor(mark, row.ref, HEADS + repo.default_branch);
  if (rollup) {
    return (
      <CheckRollupMark
        rollup={rollup}
        title={rollupTitle(rollup, row.ref, now)}
        of={rollup.target === row.ref}
      />
    );
  }
  const gone = now - Date.parse(row.moved_at) > CHECK_HISTORY_DAYS * DAY_MS;
  return <EmptyValue label={gone ? "Checks no longer kept" : "No check ran"} />;
}

/**
 * How far a branch is behind and ahead of the default branch: the two counts either side of
 * a hairline, each over a bar on a log scale of the loaded rows' most; words on a phone.
 */
function Drift({ tip, most, base }: { tip: CodeTip | undefined; most: number; base: string }) {
  if (!tip) return null;
  if (tip.counts_out_of_time || tip.behind === undefined || tip.ahead === undefined) {
    return <EmptyValue label="Not counted in the read's time" />;
  }
  const share = (n: number) => (most > 0 ? (Math.log1p(n) / Math.log1p(most)) * 100 : 0);
  const side = (n: number, which: "behind" | "ahead") => (
    <span className="code-drift__side" data-side={which}>
      <span className="code-drift__count">{n}</span>
      <span className="code-drift__bar" style={{ inlineSize: `${share(n)}%` }} />
    </span>
  );
  return (
    <span
      className="code-drift"
      role="img"
      aria-label={`${plural(tip.behind, "commit")} behind ${base}, ${tip.ahead} ahead`}
    >
      <span className="code-drift__bars">
        {side(tip.behind, "behind")}
        {side(tip.ahead, "ahead")}
      </span>
      <span className="code-drift__words">
        {tip.behind} behind, {tip.ahead} ahead
      </span>
    </span>
  );
}

/** Who moved a branch, in a word and a sentence: Gitlet for a merge or a queue landing; never a guess. */
function moverOf(
  mover: CodeRefMover | undefined,
  movedAt: string,
  now: number,
  name: string,
  whoName: (who: Who) => string,
): { who: Who | null; words: string; title: string } {
  if (!mover) {
    return now - Date.parse(movedAt) < PUSH_HISTORY_DAYS * DAY_MS
      ? {
          who: null,
          words: "Unknown",
          title: "Who moved it is unknown: one record moved it away and back.",
        }
      : {
          who: null,
          words: "No longer kept",
          title: `Who moved it is no longer kept: pushes are kept ${PUSH_HISTORY_DAYS} days.`,
        };
  }
  const by = mover.actor_seat || mover.actor;
  switch (mover.via) {
    case "merge":
      return {
        who: GITLET,
        words: "Gitlet",
        title: `Gitlet moved ${name} when ${whoName(by)} merged a change`,
      };
    case "queue":
      return {
        who: GITLET,
        words: "Gitlet",
        title: `Gitlet moved ${name} landing its merge queue's train, for ${whoName(by)}`,
      };
    case "mirror":
      return { who: by, words: whoName(by), title: "Moved by the mirror's sync" };
    case "import":
      return { who: by, words: whoName(by), title: "Moved by the import" };
    case "engine":
      return { who: by, words: whoName(by), title: "Written by a Gitlet tool" };
    default:
      return { who: by, words: whoName(by), title: "Pushed with git" };
  }
}

/** Who moved a branch and when, as a byline. */
function MovedBy({ row, now }: { row: CodeRefListing; now: number }) {
  const whoName = useWhoName();
  const m = moverOf(row.moved_by, row.moved_at, now, refName(row.ref).name, whoName);
  return (
    <span className="code-byline" title={m.title}>
      {m.who !== null && <Face who={m.who} />}
      <span className={m.who === null ? "code-refs__unknown" : "code-byline__who"}>{m.words}</span>
      <time className="code-byline__when" dateTime={row.moved_at} title={fmtDateTime(row.moved_at)}>
        {ago(row.moved_at, now)}
      </time>
    </span>
  );
}

/**
 * The repository's tags, under its branches: the engine's listing under
 * `refs/tags/`, by name a page at a time, each opening its files at the tag —
 * named by its whole ref, which no branch of the same name can shadow.
 */
function Tags({ repo }: { repo: CodeRepoDetail }) {
  const params = useMemo(() => ({ name: repo.name, prefix: TAG_PREFIX }), [repo.name]);
  const first = useCodeQuery("code_refs", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_refs", { ...params, cursor }),
    (a: CodeRefsAnswer) => a.refs ?? [],
    (r: CodeRefRow) => r.ref,
    (a: CodeRefsAnswer) => a.next ?? "",
  );
  const rows = paged.items;
  return (
    <section className="code-section" aria-labelledby="code-tags-title">
      <h2 id="code-tags-title" className="code-section__title">
        Tags
      </h2>
      {paged.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={3} label={`Loading the tags of ${repo.name}`} />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          rows.length > 0 || paged.more
            ? undefined
            : { title: "No tag yet", hint: "A tag is listed once a push that creates it applies." }
        }
      >
        <DataGrid<CodeRefRow>
          name="tags"
          rows={rows}
          rowKey={(r) => r.ref}
          serverSorted
          rowHref={(r) => repoHref(repo.name, { ref: r.ref })}
          onLoadMore={paged.more ? paged.next : undefined}
          loadedNote={paged.more ? `${plural(rows.length, "tag")} loaded` : undefined}
          aria-label={`Tags of ${repo.name}`}
          columns={[
            {
              key: "name",
              header: "Tag",
              floor: "12rem",
              phoneLead: true,
              cell: (r) => (
                <span className="code-ref">
                  <BookmarkGlyph size="sm" className="code-glyph" />
                  <span className="code-ref__name">{refName(r.ref).name}</span>
                </span>
              ),
            },
            {
              key: "commit",
              header: "Commit",
              shrink: true,
              action: true,
              cell: (r) => (r.oid ? <ShaChip oid={r.oid} /> : null),
            },
          ]}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </section>
  );
}
