/**
 * One repository's page, `#/code/repos/{name}`: what it is, its files at any
 * revision, its branches, its history, its pushes and its settings.
 *
 * # Rows, then content read by object id
 *
 * The repository itself is a row (`code_repo`): its owner, its state, its
 * default branch and where that stands, its counts and its rules, read at the
 * tab's floor on the code log and again every minute, since the code log has
 * no push to the dashboard. Everything under it is its CONTENT — a directory,
 * a file, a blame, a history — which the repository's home git node answers
 * (`code_tree`, `code_blob`, `code_blame`, `code_log`), every revision a
 * request names by a ref resolved once, at the caller's freshness. A revision
 * named by a whole commit id is answered immutably, and the hook keeps its
 * tag, so a Permalink is read once and never sent twice. Content is not polled:
 * a reader reading a file reads the revision they opened, and the Permalink
 * names it whole.
 *
 * # Nothing here writes
 *
 * A declared repository's settings are the company document's (`code.repos`),
 * settled by every activation, so Settings shows them and leads to Settings ›
 * Configuration, where they are changed. One a tool made is changed by the
 * tools' lifecycle requests, which a seat or a person's assistant makes.
 */

import { useId, type ReactNode } from "react";
import {
  ButtonLink,
  Callout,
  EmptyState,
  EmptyValue,
  Skeleton,
  Stack,
  Tabs,
  Tag,
  tabId,
} from "@crewlethq/ui";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { closedBy } from "~/app/nav.ts";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { isRepoName, repoPath, repoStatusWord, shortOID } from "~/lib/code.ts";
import { fmtBytes, fmtCount, fmtDateTime, plural } from "~/lib/format.ts";
import { indexOrg, unitByKey, useSeatBadgeOf } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useHeldGrants } from "~/lib/viewer.ts";
import type { CodeOwner, CodeRepoDetail, CodeRuleGate } from "~/protocol/index.ts";
import { PushHistory } from "../Pushes.tsx";
import { RepoChecks } from "../checks/RepoChecks.tsx";
import { REPO_TABS, compareHref, repoHref, type RepoTab } from "../paths.ts";
import { Files, Readme } from "./Files.tsx";
import { Branches, Commits } from "./History.tsx";

/**
 * How often the repository's row is read again while its page is open: a
 * MINUTE, the Code workspace's cadence on the code log, which has no push to
 * the dashboard — another person's push reaches the open page by this read.
 */
const REPO_POLL_MS = 60_000;

/** Where a repository's settings are written: Settings › Configuration. */
const CONFIG_PATH: readonly string[] = ["settings", "config"];

const REPO_TAB_LABELS: Readonly<Record<RepoTab, string>> = {
  overview: "Overview",
  files: "Files",
  branches: "Branches",
  commits: "Commits",
  pushes: "Pushes",
  checks: "Checks",
  settings: "Settings",
};

export function RepositoryPage({ repo }: { repo: string }) {
  const named = isRepoName(repo);
  const read = useCodeQuery("code_repo", { name: repo }, { enabled: named, pollMs: REPO_POLL_MS });
  const [tab, setTab] = useTab<RepoTab>("tab", REPO_TABS);
  const panelId = useId();
  if (!named) {
    return (
      <EmptyState
        title="Not a repository"
        description={`“${repo}” is not a name a repository can have.`}
      />
    );
  }
  const detail = read.data?.repo;
  if (read.loading && !detail) {
    return <Skeleton variant="text" rows={8} label={`Loading ${repo}`} />;
  }
  if (!detail) {
    if (read.error === "not_found") {
      return (
        <EmptyState
          title={`No repository named “${repo}”`}
          description={read.detail ?? "It is not declared, or it has been purged."}
        />
      );
    }
    return (
      <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
        {null}
      </QueryState>
    );
  }
  return (
    <>
      {detail.head && (
        <PageActions>
          <ButtonLink size="small" variant="secondary" href={compareHref(detail.name)}>
            Compare
          </ButtonLink>
        </PageActions>
      )}
      <header>
        <RepoHeader repo={detail} />
        <Tabs
          overflow="menu"
          variant="underline"
          ariaLabel="The repository's sections"
          value={tab}
          onValueChange={(next) => setTab(next as RepoTab)}
          panelId={panelId}
          items={REPO_TABS.map((value) => ({
            value,
            label: REPO_TAB_LABELS[value],
            ...(value === "branches" ? { count: detail.branches } : {}),
            ...(value === "pushes" ? { count: detail.pushes } : {}),
          }))}
        />
      </header>
      {detail.redirect && (
        // A RENAME LEAVES THE OLD NAME REDIRECTING for a while: the page reads
        // the repository it found, and says what it is called now.
        <Callout variant="neutral">
          {`“${repo}” is now called “${detail.name}”`}
          {detail.redirect_until
            ? `; the old name is released ${fmtDateTime(detail.redirect_until)}`
            : ""}
          .{" "}
          <a className="prose-link" href={repoHref(detail.name)}>
            Open {detail.name}
          </a>
        </Callout>
      )}
      <div
        className="tabpanel"
        role="tabpanel"
        id={panelId}
        aria-labelledby={tabId(panelId, tab)}
        tabIndex={0}
      >
        {tab === "overview" && <Overview repo={detail} />}
        {tab === "files" && <Files repo={detail} />}
        {tab === "branches" && <Branches repo={detail} />}
        {tab === "commits" && <Commits repo={detail} />}
        {tab === "pushes" && <PushHistory repo={detail.name} />}
        {tab === "checks" && <RepoChecks repo={detail.name} />}
        {tab === "settings" && <Settings repo={detail} />}
      </div>
    </>
  );
}

/** Who owns a repository, by name: a unit's off the chart (its key where the chart has none), a seat's badge. */
function useOwnerName(): (owner: CodeOwner) => string {
  const org = useOrg();
  const seatBadge = useSeatBadgeOf();
  const index = indexOrg(org);
  return (owner) =>
    owner.kind === "seat"
      ? seatBadge(owner.ref).name
      : (unitByKey(index, owner.ref)?.name ?? owner.ref);
}

/** Which seats read a repository, in words. Every person reads every repository (ADR-0058). */
function readByWord(visibility: string): string {
  return visibility === "unit" ? "Its owner's seats" : "Every seat";
}

/** The repository's header: its name, its state, and the facts a reader places it by. */
function RepoHeader({ repo }: { repo: CodeRepoDetail }) {
  const ownerName = useOwnerName();
  const state = repoStatusWord(repo.status);
  const facts: Fact[] = [
    { label: "Owner", value: ownerName(repo.owner) },
    {
      label: "Default branch",
      value: (
        <span className="mono">
          {repo.default_branch}
          {repo.head ? (
            <span className="muted" title={repo.head}>
              {" "}
              {shortOID(repo.head)}
            </span>
          ) : (
            <span className="muted"> empty</span>
          )}
        </span>
      ),
      path: repoPath(repo.name),
      query: { tab: "files" },
      token: true,
    },
    {
      label: "Read by",
      value: readByWord(repo.visibility),
      note: "Which seats read it. Every person reads every repository.",
    },
    { label: "Last push", value: repo.last_push_at ? fmtDateTime(repo.last_push_at) : "Never" },
    { label: "Size", value: fmtBytes(repo.live_bytes) },
  ];
  return (
    <ObjectHeader
      kind="Repository"
      icon="square-code"
      identifier={repo.name}
      title={repo.description || repo.name}
      status={
        state ? (
          <Tag
            appearance="outline"
            title={repo.deleted_at ? `Deleted ${fmtDateTime(repo.deleted_at)}` : undefined}
          >
            {state}
          </Tag>
        ) : undefined
      }
      facts={facts}
    />
  );
}

/** A label and its value, as the page's fact lists draw them. */
function FactList({ label, rows }: { label: string; rows: { label: string; value: ReactNode }[] }) {
  return (
    <dl className="code-facts" aria-label={label}>
      {rows.map((r) => (
        <div key={r.label}>
          <dt>{r.label}</dt>
          <dd>{r.value}</dd>
        </div>
      ))}
    </dl>
  );
}

/** What decides a repository's settings and its life, in words. */
function managedWord(managedBy: string): string {
  switch (managedBy) {
    case "config":
      return "The company document";
    case "tools":
      return "The code tools";
  }
  return managedBy;
}

/**
 * The overview: what the repository holds and what its default branch's
 * README says — the first thing a reader opening a repository looks for.
 */
function Overview({ repo }: { repo: CodeRepoDetail }) {
  return (
    <Stack gap={4}>
      <FactList
        label="The repository"
        rows={[
          {
            label: "Kind",
            value: repo.kind === "scratch" ? "A seat's scratch repository" : "Project",
          },
          { label: "Managed by", value: managedWord(repo.managed_by) },
          { label: "Branches", value: fmtCount(repo.branches) },
          { label: "Tags", value: fmtCount(repo.tags) },
          { label: "Pushes", value: fmtCount(repo.pushes) },
          {
            label: "Stored",
            value: `${fmtBytes(repo.live_bytes)} in ${plural(repo.live_packs, "pack")}, ${plural(repo.live_objects, "object")}`,
          },
          { label: "Declared", value: fmtDateTime(repo.created_at) },
          {
            label: "Last activity",
            value: repo.last_activity_at ? (
              fmtDateTime(repo.last_activity_at)
            ) : (
              <EmptyValue label="None" />
            ),
          },
          ...(repo.access ? [{ label: "Your access", value: repo.access }] : []),
        ]}
      />
      {repo.head ? (
        <Readme repo={repo} />
      ) : (
        <p className="muted">
          Nobody has pushed a branch yet: its default branch, {repo.default_branch}, holds no
          commit.
        </p>
      )}
    </Stack>
  );
}

/** What a merge gate asks, in a line. */
function gateLine(gate: CodeRuleGate["gate"]): string {
  const parts = [
    gate.merge_by === "persons"
      ? "merged by a person"
      : gate.merge_by === "maintainers"
        ? "merged by its maintainers"
        : "merged by its writers",
    gate.approvals === "one" ? "one approval" : "no approval",
  ];
  if (gate.human_paths?.length)
    parts.push(`a person approves ${plural(gate.human_paths.length, "path")}`);
  if (gate.checks?.length) {
    parts.push(`${plural(gate.checks.length, "check")} pass`);
    // A GATE REQUIRING A CHECK LANDS THROUGH ITS QUEUE (the engine's rule:
    // the queue applies to a gated branch with a required check), so that
    // the queue tests each change before it lands is part of what the gate
    // asks. The engine has one way a queue lands, so the line names it
    // rather than reading a policy off the gate.
    parts.push("landed by its queue, each change tested merged on the branch before it lands");
  }
  if (gate.owners_approve) parts.push("its path owners approve");
  return parts.join(", ");
}

/**
 * The repository's settings, as its row and its detail carry them — shown,
 * never edited here: see the file's doc.
 */
function Settings({ repo }: { repo: CodeRepoDetail }) {
  const ownerName = useOwnerName();
  const held = useHeldGrants();
  // THE CONFIGURATION, offered only to a reader who may open it — the rule
  // every lock is drawn by.
  const canConfigure = held !== null && closedBy(CONFIG_PATH, held) === null;
  const gates = new Map((repo.gates ?? []).map((g) => [g.pattern, g.gate]));
  const rules = repo.protections ?? [];
  return (
    <Stack gap={4}>
      {repo.managed_by === "config" ? (
        <Callout variant="neutral">
          <Stack gap={2}>
            <span>
              These settings are the company document’s, in <span className="mono">code.repos</span>
              : every activation settles them, so they are changed in Settings › Configuration.
            </span>
            {canConfigure && (
              <span>
                <ButtonLink variant="secondary" size="small" href={href(CONFIG_PATH)}>
                  Open the configuration
                </ButtonLink>
              </span>
            )}
          </Stack>
        </Callout>
      ) : (
        <Callout variant="neutral">
          A tool made this repository, and the code tools’ lifecycle requests change its settings —{" "}
          <span className="mono">code_update_repo</span> and{" "}
          <span className="mono">code_protect_branch</span>, which a seat or your assistant calls.
        </Callout>
      )}
      <FactList
        label="Settings"
        rows={[
          {
            label: "Description",
            value: repo.description || <EmptyValue label="None" />,
          },
          { label: "Default branch", value: <span className="mono">{repo.default_branch}</span> },
          { label: "Owner", value: ownerName(repo.owner) },
          { label: "Read by", value: readByWord(repo.visibility) },
          {
            label: "Search",
            value:
              repo.search === "indexed"
                ? "Indexed from its default branch"
                : repo.search === "grep"
                  ? "Searched only by a search naming it"
                  : repo.search,
          },
        ]}
      />
      <section aria-label="Protection rules">
        <h3 className="t-label">Protection rules</h3>
        {rules.length === 0 ? (
          <p className="muted">No rule protects a ref: anybody who may write may push to any.</p>
        ) : (
          <ul className="code-rules">
            {rules.map((rule) => {
              const gate = gates.get(rule.pattern);
              return (
                <li key={rule.pattern}>
                  <span className="mono">{rule.pattern}</span>
                  <span className="muted">
                    {rule.direct_push === "none"
                      ? "nobody pushes to it directly"
                      : rule.direct_push === "persons"
                        ? "only a person pushes to it directly"
                        : rule.direct_push}
                    {rule.allow_force ? ", forced pushes allowed" : ""}
                    {rule.allow_delete ? ", deletion allowed" : ""}
                    {gate ? `; a merge into it is ${gateLine(gate)}` : ""}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </section>
      <section aria-label="Path owners">
        <h3 className="t-label">Path owners</h3>
        {(repo.path_owners ?? []).length === 0 ? (
          <p className="muted">No path is owned: a change asks reviews of nobody by its paths.</p>
        ) : (
          <ul className="code-rules">
            {(repo.path_owners ?? []).map((p) => (
              <li key={`${p.owner.kind}:${p.owner.ref}`}>
                <span>{ownerName(p.owner)}</span>
                <span className="muted mono">{p.paths.join(", ")}</span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </Stack>
  );
}
