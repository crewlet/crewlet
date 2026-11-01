/**
 * Code › Repositories: every repository the engine hosts that the reader may
 * read, by name.
 *
 * # The engine's listing, a page at a time
 *
 * `code_repos` answers the repositories by name, each held to the reader's
 * grant on it: a person reads every repository (ADR-0058), and a seat what its
 * place in the chart relates it to. A page is what the engine SCANNED, not
 * what it kept, so it can be shorter than its limit — or empty with a cursor —
 * and the list pages until no cursor comes back, its first page held still
 * from the press of Load more (`lib/usePaged.ts`). The order is the engine's,
 * by name, and the grid draws it as given: a head that sorted the pages
 * loaded would answer a different question under the same arrow.
 *
 * # A row opens the repository's page
 *
 * A row leads to the repository's own page (`#/code/repos/{name}`: its files,
 * branches, history, pushes and settings), and says what the listing knows:
 * who owns it, which seats read it, its default branch and where that stands,
 * its last push, its size and the rules that protect it.
 *
 * # One write: an import
 *
 * "Import a repository" (`ImportRepo.tsx`) is the listing's one write, a
 * person's: a new project filled from another host, or a pull mirror of one.
 * A declared repository's settings are the company document's (`code.repos`,
 * its `managed_by` is `config`), and every activation settles them: the way to
 * change one is the configuration, which the empty state offers a reader who
 * may open it. One a tool made (`managed_by` `tools`) is changed on its own
 * page's Settings tab.
 */

import { useMemo } from "react";
import {
  ButtonLink,
  EmptyValue,
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
import { plainSeat } from "~/app/frame/cells.tsx";
import { closedBy } from "~/app/nav.ts";
import { href, useParam } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { REPO_STATUS_FILTERS, repoStatusWord, shortOID } from "~/lib/code.ts";
import { fmtBytes, fmtCount, fmtDateTime, plural } from "~/lib/format.ts";
import { indexOrg, unitByKey, useSeatBadgeOf } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import { useHeldGrants } from "~/lib/viewer.ts";
import type { CodeRepoSummary, CodeReposAnswer } from "~/protocol/index.ts";
import { ImportRepoButton } from "./ImportRepo.tsx";
import { repoHref } from "./paths.ts";

/**
 * How often the listing's first page is read again while it is open and
 * nothing past it is loaded: a MINUTE, the cadence a directory nothing pushes
 * is polled at here (Work's Projects). A
 * repository's own rows move at an activation and its counts at every push,
 * and the code log has no push to the dashboard, so another person's push
 * reaches the open listing by this read or not at all.
 */
const REPOS_POLL_MS = 60_000;

/** Where a repository's settings are written: Settings › Configuration. */
const CONFIG_PATH: readonly string[] = ["settings", "config"];

export function Repositories() {
  const now = useNow();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const seatBadge = useSeatBadgeOf();
  const held = useHeldGrants();
  // THE ENGINE'S OWN STATE FILTER, as the `status` it takes: none is its
  // default, the repositories in use. A filter, so it replaces the history
  // entry rather than adding one.
  const [status, setStatus] = useParam("status", "", "filter");
  const params = useMemo(() => (status ? { status } : {}), [status]);
  // THE FIRST PAGE IS HELD from the press of Load more (`lib/usePaged.ts`):
  // the cursor is the last name it scanned, and a repository declared,
  // restored or moved into the filter while it was read again would push that
  // name out of both pages.
  const hold = usePageHold<CodeReposAnswer>(params);
  const first = useCodeQuery("code_repos", params, {
    pollMs: REPOS_POLL_MS,
    hold: hold.holding,
  });
  const paged = usePaged(
    hold,
    first,
    (cursor) => codeRest.query("code_repos", { ...params, cursor }),
    (answer: CodeReposAnswer) => answer.repos ?? [],
    (repo: CodeRepoSummary) => repo.id,
    (answer: CodeReposAnswer) => answer.next ?? "",
  );
  const rows = paged.items;
  // THE CONFIGURATION, offered only to a reader who may open it — the rule
  // every lock is drawn by — since it is where a repository is declared.
  const canConfigure = held !== null && closedBy(CONFIG_PATH, held) === null;

  return (
    <>
      <div className="toolbar">
        <Select
          width="auto"
          value={status}
          onChange={(value) => setStatus(String(value))}
          options={REPO_STATUS_FILTERS.map((f) => ({ value: f.value, label: f.label }))}
          ariaLabel="Which repositories"
          active={status !== ""}
        />
        <ImportRepoButton />
      </div>
      {paged.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={6} label="Loading repositories" />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={
          // A PAGE THAT HELD NONE MAY STILL HAVE A CURSOR — what the engine
          // scanned and the reader may not read — so the list is empty only
          // where nothing is left to page to.
          rows.length > 0 || paged.more
            ? undefined
            : status
              ? {
                  title: emptyTitle(status),
                  hint: "Choose Active, archived and importing to see the repositories in use.",
                }
              : {
                  title: "No repositories yet",
                  hint: "A repository is declared in the company document's code.repos, and each activation creates the ones it names.",
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
        <DataGrid<CodeRepoSummary>
          rows={rows}
          rowKey={(r) => r.id}
          // THE ENGINE'S ORDER, by name, over the whole set.
          serverSorted
          rowHref={(r) => repoHref(r.name)}
          onLoadMore={paged.more ? paged.older : undefined}
          loadedNote={
            paged.more || paged.held
              ? `${plural(rows.length, "repository", "repositories")} loaded${
                  // SAID ONLY ONCE IT IS TRUE: until then the first page is
                  // read again every minute.
                  paged.held ? ", paused while more are loaded" : ""
                }`
              : undefined
          }
          aria-label="Repositories"
          columns={[
            {
              key: "name",
              header: "Repository",
              floor: "10rem",
              phoneLead: true,
              cell: (r) => <TableKeyCell value={r.name} />,
            },
            {
              key: "state",
              header: "State",
              shrink: true,
              // NOTHING FOR AN ACTIVE REPOSITORY, which is the state nothing
              // needs saying of; a phone's card drops the line.
              cell: (r) => <RepoState repo={r} />,
            },
            {
              key: "owner",
              header: "Owner",
              shrink: true,
              // NOT A LINK: the row around it is one, to the repository's
              // page.
              cell: (r) =>
                r.owner.kind === "seat" ? (
                  <TableEntityCell {...plainSeat({ ...seatBadge(r.owner.ref) })} />
                ) : (
                  <UnitOwner name={unitByKey(index, r.owner.ref)?.name} unitKey={r.owner.ref} />
                ),
            },
            {
              key: "visibility",
              header: "Read by",
              shrink: true,
              drop: 2,
              // WHICH SEATS: a person reads every repository whatever this
              // says (ADR-0058), and the title says so.
              cell: (r) => (
                <span title="Which seats read it. Every person reads every repository.">
                  <TableTextCell>
                    {r.visibility === "unit" ? "Its owner's seats" : "Every seat"}
                  </TableTextCell>
                </span>
              ),
            },
            {
              key: "branch",
              header: "Default branch",
              shrink: true,
              drop: 4,
              cell: (r) => <DefaultBranch repo={r} />,
            },
            {
              key: "last_push",
              header: "Last push",
              shrink: true,
              cell: (r) => <TableDateCell value={r.last_push_at} now={now} emptyLabel="Never" />,
            },
            {
              key: "pushes",
              header: "Pushes",
              shrink: true,
              align: "right",
              drop: 3,
              cell: (r) => <TableNumberCell value={r.pushes} format={fmtCount} />,
            },
            {
              key: "size",
              header: "Size",
              shrink: true,
              align: "right",
              drop: 1,
              cell: (r) => (
                <TableNumberCell
                  value={r.live_bytes}
                  format={fmtBytes}
                  title={`${plural(r.live_packs, "pack")}, ${plural(r.live_objects, "object")}`}
                />
              ),
            },
            {
              key: "protected",
              header: "Protection",
              shrink: true,
              drop: 5,
              cell: (r) =>
                r.protections && r.protections.length > 0 ? (
                  <span title={r.protections.map((p) => p.pattern).join(", ")}>
                    <TableTextCell>{plural(r.protections.length, "rule")}</TableTextCell>
                  </span>
                ) : (
                  <EmptyValue label="No rule protects it" />
                ),
            },
          ]}
        />
      </QueryState>
      {/* SAID, NOT BANNERED: the grid's Load more stays, and asks the page
          again (`PageFailed`). */}
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </>
  );
}

/** What an empty listing of one state says. */
function emptyTitle(status: string): string {
  switch (status) {
    case "importing":
      return "No repository is being imported";
    case "active":
      return "No active repositories";
    case "archived":
      return "No archived repositories";
    case "deleted":
      return "No deleted repositories";
    case "purging":
      return "No repository is being purged";
    default:
      return `No repositories in the state “${status}”`;
  }
}

/** What a repository is beyond active, or nothing. */
function RepoState({ repo }: { repo: CodeRepoSummary }) {
  const word = repoStatusWord(repo.status);
  if (!word) return null;
  // NEUTRAL: none of these is a fault. A deleted repository says when, since
  // declaring it again restores it until it is purged.
  return (
    <Tag
      appearance="outline"
      title={repo.deleted_at ? `Deleted ${fmtDateTime(repo.deleted_at)}` : undefined}
    >
      {word}
    </Tag>
  );
}

/**
 * A unit that owns a repository, by its name in the chart — or its key,
 * marked, where the chart this tab holds has no such unit (a rename the
 * engine has not reconciled yet, or a chart not read yet).
 */
function UnitOwner({ name, unitKey }: { name: string | undefined; unitKey: string }) {
  if (name) return <TableTextCell>{name}</TableTextCell>;
  return (
    <Tag
      variant="warning"
      appearance="outline"
      title="The org chart this tab holds has no such unit"
    >
      {unitKey}
    </Tag>
  );
}

/** The default branch, and the commit it stands at — or that it holds none yet. */
function DefaultBranch({ repo }: { repo: CodeRepoSummary }) {
  return (
    <span className="row gap-1 nowrap">
      <span className="mono">{repo.default_branch}</span>
      {repo.head ? (
        <span className="mono muted" title={repo.head}>
          {shortOID(repo.head)}
        </span>
      ) : (
        <span className="muted">empty</span>
      )}
    </span>
  );
}
