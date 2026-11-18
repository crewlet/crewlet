/**
 * Gitlet › Repositories: every repository the engine hosts that the reader may
 * read, as a code host lists them.
 *
 * # The engine's listing, a page at a time
 *
 * `code_repos` answers the repositories, each held to the reader's grant on
 * it: a person reads every repository (ADR-0088), and a seat what its place
 * in the chart relates it to. A page is what the engine SCANNED, not what it
 * kept, so it can be shorter than its limit, or empty with a cursor, and
 * the list pages until no cursor comes back, each page joined to the one
 * before it (`lib/usePaged.ts`).
 *
 * # Its order and its filters
 *
 * By last push by default, or by name; a kind and a state narrow it, each the
 * engine's own filter.
 *
 * # A row opens the repository's page
 *
 * A row is a repository as a code host lists one: its marks, its owner,
 * branch, size, pushes and last push, and at its end its live changes, its
 * default branch's checks and 14 days of pushes.
 *
 * # Two writes: a new repository, and an import
 *
 * "New repository" (`NewRepo.tsx`) makes an empty project a unit owns, and
 * "Import a repository" (`ImportRepo.tsx`), a person's alone, a new project
 * filled from another host or a pull mirror of one. A deleted repository is
 * listed under the Deleted filter, and restored on its own page.
 * A declared repository's settings are the company document's (`code.repos`,
 * its `managed_by` is `config`), and every activation settles them: the way to
 * change one is the configuration, which the empty state offers a reader who
 * may open it. One a tool made (`managed_by` `tools`) is changed on its own
 * page's Settings tab.
 */

import { useMemo } from "react";
import {
  ButtonLink,
  Callout,
  IconTile,
  SegmentedControl,
  Select,
  Skeleton,
  Tag,
  VisuallyHidden,
} from "@crewlethq/ui";
import {
  CircleDashedGlyph,
  ClockGlyph,
  FileCodeGlyph,
  GitBranchGlyph,
  GitPullRequestGlyph,
  HardDriveGlyph,
  NetworkGlyph,
  SearchGlyph,
  ShieldGlyph,
  SquareCodeGlyph,
  UploadGlyph,
} from "@crewlethq/icons/glyphs";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { closedBy } from "~/app/nav.ts";
import { href, useParam } from "~/app/router.tsx";
import { AgoTime } from "~/components/AgoTime.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { CHECK_HISTORY_DAYS, rollupTitle } from "~/lib/checks.ts";
import { REPO_STATUS_FILTERS, repoStatusWord } from "~/lib/code.ts";
import { fmtBytes, fmtDateTime, plural } from "~/lib/format.ts";
import { indexOrg, unitByKey } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useHeldGrants } from "~/lib/viewer.ts";
import type { CodeRepoSummary, CodeReposAnswer, CodeReposTotal } from "~/protocol/index.ts";
import { PushSpark, SPARK_DAYS, useDayBounds } from "./activity.tsx";
import { CodeList, useListSort } from "./CodeList.tsx";
import { ImportRepoButton } from "./ImportRepo.tsx";
import { moverOf } from "./movers.ts";
import { NewRepoButton } from "./NewRepo.tsx";
import { repoHref } from "./paths.ts";
import { REFUSED_CODES, refusalSentence } from "./repo/marks.ts";
import {
  kindCount,
  lastPushWords,
  listingKind,
  listingParams,
  listingSort,
  listingStatus,
  sortHeld,
  type RepoKind,
} from "./repoListing.ts";
import { useGitletSectionCounts } from "./sectionCounts.ts";
import { CheckRollupMark, Face, useWhoName } from "./ui.tsx";
import "~/styles/gitlet.css";

/**
 * How often the listing's first page is read again while it is open: a
 * MINUTE, the cadence a directory nothing pushes is polled at here (Worklet's
 * Projects). A
 * repository's own rows move at an activation and its counts at every push,
 * and the code log has no push to the dashboard, so another person's push
 * reaches the open listing by this read or not at all.
 */
const REPOS_POLL_MS = 60_000;

/** Where a repository's settings are written: Settings › Configuration. */
const CONFIG_PATH: readonly string[] = ["settings", "config"];

export function Repositories() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const held = useHeldGrants();
  // THE ENGINE'S OWN FILTERS, a `status` and a `kind`: none is its default. A filter replaces the
  // history entry rather than adding one.
  const [statusParam, setStatus] = useParam("status", "", "filter");
  const [kindParam, setKind] = useParam("kind", "", "filter");
  const [sort, setSort] = useListSort();
  const status = listingStatus(statusParam);
  const kind = listingKind(kindParam);
  const order = listingSort(status, sort);
  // THE READER'S LAST 14 DAYS, moving once a day: every row's pushes by day ride the listing.
  const bounds = useDayBounds(SPARK_DAYS);
  const params = useMemo(
    () => listingParams(status, kind, sort, bounds),
    [status, kind, sort, bounds],
  );
  // THE FIRST PAGE STAYS LIVE past Load more, and the pager joins it to the
  // pages after it (`lib/usePaged.ts`).
  const first = useCodeQuery("code_repos", params, {
    pollMs: REPOS_POLL_MS,
    enabled: bounds.length > 0,
  });
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_repos", { ...params, cursor }),
    (answer: CodeReposAnswer) => answer.repos ?? [],
    (repo: CodeRepoSummary) => repo.id,
    (answer: CodeReposAnswer) => answer.next ?? "",
  );
  // THE KIND COUNTS, the section figures' answer: its refusal costs only the figures.
  const counts = useGitletSectionCounts({ pollMs: REPOS_POLL_MS });
  const byKind = counts.data?.counts?.repos;
  const rows = paged.items;
  const total = paged.answers[0]?.repos_total;
  // THE CONFIGURATION, offered only to a reader who may open it — the rule
  // every lock is drawn by — since it is where a repository is declared.
  const canConfigure = held !== null && closedBy(CONFIG_PATH, held) === null;
  const refused = listingRefused(paged.error, paged.refusal, first.detail);
  const byNameOnly = sortHeld(status);

  return (
    <>
      <PageActions>
        <ImportRepoButton />
        <NewRepoButton />
      </PageActions>
      <PageToolbar>
        <SegmentedControl<RepoKind | "">
          semantics="radio"
          activate="manual"
          size="sm"
          label="Which kind of repository"
          value={kind}
          onValueChange={setKind}
          options={[
            { value: "", label: "All", count: kindCount(byKind, "", status) },
            { value: "project", label: "Projects", count: kindCount(byKind, "project", status) },
            { value: "scratch", label: "Scratch", count: kindCount(byKind, "scratch", status) },
          ]}
        />
        <Select
          size="sm"
          width="auto"
          value={status}
          onChange={(value) => setStatus(String(value))}
          options={REPO_STATUS_FILTERS.map((f) => ({ value: f.value, label: f.label }))}
          ariaLabel="Which repositories"
          active={status !== ""}
        />
        <span className="code-repos__sort">
          <Select
            size="sm"
            width="auto"
            value={order === "name" ? "name" : ""}
            onChange={(value) => setSort(String(value))}
            options={[
              { value: "", label: "Sort: Last push" },
              { value: "name", label: "Sort: Name" },
            ]}
            ariaLabel="Sort"
            active={order === "name" && !byNameOnly}
            disabled={byNameOnly}
            disabledReason={
              byNameOnly ? "Deleted and purging repositories are listed by name only." : undefined
            }
          />
        </span>
      </PageToolbar>
      {paged.loading && rows.length === 0 && refused === null && (
        <Skeleton variant="text" rows={6} label="Loading repositories" />
      )}
      {refused !== null ? (
        <Callout variant="warning">{refused}</Callout>
      ) : (
        <QueryState
          error={paged.error}
          refusal={paged.refusal}
          detail={first.detail}
          loading={paged.loading}
          empty={
            // A PAGE THAT HELD NONE MAY STILL HAVE A CURSOR (what the engine
            // scanned and the reader may not read), so the list is empty only
            // where nothing is left to page to.
            rows.length > 0 || paged.more
              ? undefined
              : status || kind
                ? {
                    title: emptyTitle(status, kind),
                    hint: kind
                      ? "Choose All to see every kind."
                      : "Choose Active, archived and importing to see the repositories in use.",
                  }
                : {
                    title: "No repositories yet",
                    hint: "Create one with New repository, import one from another host, or declare it in the company document's code.repos, which each activation creates.",
                    ...(canConfigure
                      ? {
                          action: (
                            <ButtonLink variant="secondary" href={href(CONFIG_PATH)}>
                              Open the configuration
                            </ButtonLink>
                          ),
                        }
                      : {}),
                  }
          }
        >
          <div className="code-repos">
            <CodeList<CodeRepoSummary>
              rows={rows}
              rowKey={(r) => r.id}
              rowHref={(r) => repoHref(r.name)}
              aria-label="Repositories"
              lead={(r) => (
                <IconTile tone="neutral" size="sm">
                  {r.kind === "scratch" ? <FileCodeGlyph /> : <SquareCodeGlyph />}
                </IconTile>
              )}
              title={(r) => <span className="code-repo-row__name">{r.name}</span>}
              titleAfter={(r) => <RepoMarks repo={r} />}
              summary={(r) =>
                r.description ? (
                  <span className="code-repo-row__description" title={r.description}>
                    {r.description}
                  </span>
                ) : (
                  <span className="code-repo-row__description" data-none="">
                    No description
                  </span>
                )
              }
              meta={(r) => <RepoMeta repo={r} unitName={unitByKey(index, r.owner.ref)?.name} />}
              facts={(r) => <RepoFacts repo={r} bounds={bounds} />}
              paged={paged}
              moreLabel="Load more repositories"
              foot={footOf(total, rows.length, paged.more, order)}
            />
          </div>
        </QueryState>
      )}
    </>
  );
}

/** The listing refused for good (an older fleet refusing a key it lacks), in the engine's words. */
function listingRefused(
  error: string | null,
  refusal: unknown,
  detail: string | undefined,
): string | null {
  if (!error || !REFUSED_CODES.has(error) || error === "unauthorized") return null;
  // A NODE CATCHING UP is QueryState's to say: waiting clears it.
  const retry = (refusal as { retryAfter?: number } | null)?.retryAfter ?? 0;
  if (error === "unavailable" && retry > 0) return null;
  const sentence = refusalSentence({
    detail,
    refusal: refusal as Parameters<typeof refusalSentence>[0]["refusal"],
  });
  const lead = "Listing repositories with their activity is unavailable on this fleet yet";
  return sentence ? `${lead}: ${sentence.replace(/\.$/, "")}.` : `${lead}.`;
}

/** What an empty listing of one state and one kind says. */
function emptyTitle(status: string, kind: RepoKind | ""): string {
  const one =
    kind === "project" ? "project" : kind === "scratch" ? "scratch repository" : "repository";
  const many =
    kind === "project" ? "projects" : kind === "scratch" ? "scratch repositories" : "repositories";
  switch (status) {
    case "":
      return `No ${many}`;
    case "importing":
      return `No ${one} is being imported`;
    case "active":
      return `No active ${many}`;
    case "archived":
      return `No archived ${many}`;
    case "deleted":
      return `No deleted ${many}`;
    case "purging":
      return `No ${one} is being purged`;
    default:
      return `No ${many} in the state “${status}”`;
  }
}

/** The footer: the whole listing and its size across its pages, or the rows loaded where not answered. */
function footOf(
  total: CodeReposTotal | undefined,
  loaded: number,
  more: boolean,
  order: "name" | "pushed",
) {
  if (!total) {
    return more
      ? `${plural(loaded, "repository", "repositories")} loaded`
      : plural(loaded, "repository", "repositories");
  }
  const all = plural(total.repos, "repository", "repositories");
  const count = more
    ? `${loaded.toLocaleString()} of ${all} loaded, ${order === "name" ? "by name" : "newest push first"}`
    : all;
  if (total.live_bytes === undefined) {
    return <span title="Their size is not summed past 10,000 repositories.">{count}</span>;
  }
  return `${count} · ${fmtBytes(total.live_bytes)}${more ? " in all" : ""}`;
}

/**
 * A repository's marks beside its name: its state where it is not active (none
 * of them a fault, so outlined), a seat's scratch repository, a mirror either
 * way, a repository only its owner's seats read. An active project carries none.
 */
function RepoMarks({ repo }: { repo: CodeRepoSummary }) {
  const state = repoStatusWord(repo.status);
  return (
    <>
      {state && (
        <Tag
          size="sm"
          appearance="outline"
          title={repo.deleted_at ? `Deleted ${fmtDateTime(repo.deleted_at)}` : undefined}
        >
          {state}
        </Tag>
      )}
      {repo.kind === "scratch" && (
        <Tag size="sm" appearance="outline">
          Scratch
        </Tag>
      )}
      {repo.mirrored && (
        <Tag size="sm" appearance="outline">
          Pull mirror
        </Tag>
      )}
      {repo.card?.push_mirrored && (
        <Tag size="sm" appearance="outline" leadingIcon={<UploadGlyph size="xs" />}>
          Push mirror
        </Tag>
      )}
      {repo.visibility === "unit" && (
        <Tag
          size="sm"
          appearance="outline"
          title="Only its owner's seats read it; every person does."
        >
          Unit
        </Tag>
      )}
    </>
  );
}

/** The quiet line: owner, default branch, size, pushes, last push, and what is not the rule. */
function RepoMeta({ repo, unitName }: { repo: CodeRepoSummary; unitName: string | undefined }) {
  const whoName = useWhoName();
  return (
    <>
      <span className="code-repo-meta">
        {repo.owner.kind === "seat" ? (
          <>
            <Face who={repo.owner.ref} />
            <span className="code-repo-meta__who">{whoName(repo.owner.ref)}</span>
          </>
        ) : (
          <>
            <NetworkGlyph size="xs" aria-hidden />
            <UnitOwner name={unitName} unitKey={repo.owner.ref} />
          </>
        )}
      </span>
      <span className="code-repo-meta">
        <GitBranchGlyph size="xs" aria-hidden />
        <span className="mono">{repo.default_branch}</span>
        {!repo.head && <span>, empty</span>}
      </span>
      <span
        className="code-repo-meta"
        title={`${plural(repo.live_packs, "pack")}, ${plural(repo.live_objects, "object")}`}
      >
        <HardDriveGlyph size="xs" aria-hidden />
        {fmtBytes(repo.live_bytes)}
      </span>
      <span className="code-repo-meta">
        <UploadGlyph size="xs" aria-hidden />
        {plural(repo.pushes, "push", "pushes")}
      </span>
      <LastPush repo={repo} />
      {repo.search === "grep" && (
        <span className="code-repo-meta" title="Code search reads it only when a search names it.">
          <SearchGlyph size="xs" aria-hidden />
          searched with git grep
        </span>
      )}
      {(repo.protections ?? []).length === 0 && (
        <span className="code-repo-meta">
          <ShieldGlyph size="xs" aria-hidden />
          no protected branch
        </span>
      )}
    </>
  );
}

/** The last push in words: who, by the movers' rule, or why who is not said. */
function LastPush({ repo }: { repo: CodeRepoSummary }) {
  const now = useNow();
  const whoName = useWhoName();
  const words = lastPushWords(repo.card, repo.last_push_at, now);
  switch (words.kind) {
    case "never":
      return (
        <span className="code-repo-meta">
          <ClockGlyph size="xs" aria-hidden />
          never pushed
        </span>
      );
    case "bare":
    case "unknown":
      return (
        <span className="code-repo-meta" title={words.kind === "unknown" ? words.title : undefined}>
          <ClockGlyph size="xs" aria-hidden />
          <span>
            pushed <AgoTime value={words.at} />
          </span>
        </span>
      );
    case "refused": {
      const by = words.push.actor_seat || words.push.actor;
      return (
        <span className="code-repo-meta" title={words.title}>
          <ClockGlyph size="xs" aria-hidden />
          <span className="code-repo-meta__by">
            <Face who={by} />
            <span className="code-repo-meta__who">{whoName(by)}</span>
          </span>
          <span>
            tried to push <AgoTime value={words.at} />
          </span>
        </span>
      );
    }
    case "applied": {
      const m = moverOf(words.push, words.at, now, repo.default_branch, whoName);
      return (
        <span className="code-repo-meta" title={m.title}>
          <ClockGlyph size="xs" aria-hidden />
          <span>
            pushed <AgoTime value={words.at} /> by
          </span>
          <span className="code-repo-meta__by">
            {m.who !== null && <Face who={m.who} />}
            <span className="code-repo-meta__who">{m.words}</span>
          </span>
        </span>
      );
    }
  }
}

/** The three facts at a row's end, each in a fixed track and empty where not answered. */
function RepoFacts({ repo, bounds }: { repo: CodeRepoSummary; bounds: readonly string[] }) {
  const now = useNow();
  const live = repo.change_counts;
  const branch = `refs/heads/${repo.default_branch}`;
  const tip = repo.card?.tip_checks;
  const spark = repo.spark?.slice(-SPARK_DAYS);
  const offset = (repo.spark?.length ?? 0) - (spark?.length ?? 0);
  return (
    <div className="code-repo-facts">
      <span className="code-repo-facts__changes">
        {live && (
          <span
            className="code-repo-facts__count"
            title={`${plural(live.live, "live change")}: ${live.open} open, ${plural(live.draft, "draft")}`}
          >
            <GitPullRequestGlyph size="sm" aria-hidden />
            <VisuallyHidden>Live changes: </VisuallyHidden>
            {live.live}
          </span>
        )}
      </span>
      <span className="code-repo-facts__checks">
        {tip ? (
          <CheckRollupMark
            rollup={tip}
            title={rollupTitle({ ...tip, target: branch }, branch, now)}
          />
        ) : repo.card && repo.head ? (
          <span
            className="code-check-mark"
            data-tone="neutral"
            title={`No check of ${repo.default_branch}'s head is kept: checks are kept ${CHECK_HISTORY_DAYS} days.`}
          >
            <CircleDashedGlyph size="sm" aria-hidden />
            <span className="code-check-mark__word">No checks</span>
          </span>
        ) : null}
      </span>
      <span className="code-repo-facts__spark">
        {spark &&
          (spark.every((n) => n === 0) ? (
            <span className="code-repo-facts__quiet" title="No push in the last 14 days.">
              <VisuallyHidden>No push in the last 14 days.</VisuallyHidden>
            </span>
          ) : (
            <PushSpark
              compact
              counts={spark}
              starts={bounds.slice(offset, offset + spark.length)}
              now={now}
              caption={`${repo.name}'s pushes, last 14 days`}
            />
          ))}
      </span>
    </div>
  );
}

/**
 * A unit that owns a repository, by its name in the chart — or its key,
 * marked, where the chart this tab holds has no such unit (a rename the
 * engine has not reconciled yet, or a chart not read yet).
 */
function UnitOwner({ name, unitKey }: { name: string | undefined; unitKey: string }) {
  if (name) return <span className="code-repo-meta__who">{name}</span>;
  return (
    <Tag
      variant="warning"
      appearance="outline"
      size="sm"
      title="The org chart this tab holds has no such unit"
    >
      {unitKey}
    </Tag>
  );
}
