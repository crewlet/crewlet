/**
 * One repository's page, `#/gitlet/repos/{name}`, laid out as a code host
 * lays one out: its name under its owner's, its marks and its description,
 * then its CODE — the files at any revision, the latest commit that touched
 * them, the README and, beside them at the root, what the repository is —
 * and the changes proposed to it, its history, its branches and tags, its
 * pushes, its checks and its settings.
 *
 * # Rows, then content read by object id
 *
 * The repository itself is a row (`code_repo`): its owner, its state, its
 * default branch and where that stands, its counts and its rules, read at the
 * tab's floor on the code log and again every minute, since the code log has
 * no push to the dashboard. Everything under it is its CONTENT — a directory,
 * a file, a blame, a history — which the repository's home Gitlet node answers
 * (`code_tree`, `code_blob`, `code_blame`, `code_log`), every revision a
 * request names by a ref resolved once, at the caller's freshness. A revision
 * named by a whole commit id is answered immutably, and the tab keeps the
 * answer (`lib/useCodeQuery.ts`), so a Permalink left and come back to is
 * answered with no request while the tab keeps it. Content is not polled:
 * a reader reading a file reads the revision they opened, and the Permalink
 * names it whole.
 *
 * # Its Settings tab writes, a person's
 *
 * A declared repository's settings are the company document's (`code.repos`),
 * settled by every activation, so Settings shows them and leads to Settings ›
 * Configuration, where they are changed. One a tool or a person made is
 * changed here: its details (description, default branch, owner and who
 * reads it), its name, its archive, its search mode and its mirrors
 * (`RepoWrites.tsx`, `code_update_repo`), its path owners (`PathOwners.tsx`,
 * the same act's `path_owners`), its protection rules (`Protect.tsx`,
 * `code_protect_branch`) and its delete (`RepoLife.tsx`, `code_delete_repo`,
 * asked once its name is typed). Any
 * repository's unreachable history is purged here, after a confirmation naming
 * who keeps copies. A deleted repository says so above its tabs and offers its
 * restore (`code_restore_repo`), and its Settings draw what it holds and none
 * of those writes: it takes no change but its restore. Every write is confirmed, never
 * optimistic: the page is redrawn from the read the write's floor brings.
 * The header's state pill says `importing` while an import runs, and the
 * About rail how far it went and where its mirrors stand.
 */

import { Fragment, useId, type ReactNode } from "react";
import {
  AsideLayout,
  ButtonLink,
  Callout,
  Card,
  EmptyState,
  EmptyValue,
  Select,
  Skeleton,
  Stack,
  Tabs,
  Tag,
  VisuallyHidden,
  tabId,
} from "@crewlethq/ui";
import {
  CodeGlyph,
  GitBranchGlyph,
  GitCommitVerticalGlyph,
  GitPullRequestGlyph,
  ListChecksGlyph,
  LockGlyph,
  SettingsGlyph,
  UploadGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { closedBy } from "~/app/nav.ts";
import { href, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { CHANGE_STATE_FILTERS, isRepoName, repoStatusWord } from "~/lib/code.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { indexOrg, seatPath, unitByKey, unitPlace, useSeatBadgeOf } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useHeldGrants } from "~/lib/viewer.ts";
import { gateLine, pushRuleLine, ruleSourceLine } from "~/lib/protection.ts";
import {
  importStanding,
  pullMirrorStanding,
  purgeStanding,
  pushMirrorStanding,
} from "~/lib/repoWrites.ts";
import type { CodeOwner, CodeRepoDetail } from "~/protocol/index.ts";
import { ChangeList } from "../ChangeList.tsx";
import { CodeNotFound } from "../NotFound.tsx";
import { PushHistory } from "../Pushes.tsx";
import { RepoChecks } from "../checks/RepoChecks.tsx";
import { REPO_TABS, changesHref, compareHref, repoHref, type RepoTab } from "../paths.ts";
import { About, managedWord, readByWord, searchWord } from "./About.tsx";
import { Files } from "./Files.tsx";
import { Branches } from "./Branches.tsx";
import { Commits } from "./History.tsx";
import { AddPathOwnerButton, PathOwnerControls } from "./PathOwners.tsx";
import { ProtectButton, RuleControls } from "./Protect.tsx";
import { DeleteRepoControl, DeletedNotice } from "./RepoLife.tsx";
import {
  ArchiveControl,
  DetailsControl,
  MirrorControls,
  PurgeControl,
  RenameControl,
  SearchModeControl,
} from "./RepoWrites.tsx";
import { useDefaultChecks } from "./marks.ts";
import { useRevisions } from "./refs.tsx";
import "~/styles/gitlet.css";

/**
 * How often the repository's row is read again while its page is open: a
 * MINUTE, the Gitlet workspace's cadence on the code log, which has no push to
 * the dashboard — another person's push reaches the open page by this read.
 */
const REPO_POLL_MS = 60_000;

/** Where a repository's settings are written: Settings › Configuration. */
const CONFIG_PATH: readonly string[] = ["settings", "config"];

const REPO_TAB_LABELS: Readonly<Record<RepoTab, string>> = {
  code: "Code",
  changes: "Changes",
  commits: "Commits",
  branches: "Branches",
  pushes: "Pushes",
  checks: "Checks",
  settings: "Settings",
};

/** Each tab's glyph, as a code host marks its sections: decorative, the label names the tab. */
const REPO_TAB_GLYPHS: Readonly<Record<RepoTab, ReactNode>> = {
  code: <CodeGlyph size="sm" />,
  changes: <GitPullRequestGlyph size="sm" />,
  commits: <GitCommitVerticalGlyph size="sm" />,
  branches: <GitBranchGlyph size="sm" />,
  pushes: <UploadGlyph size="sm" />,
  checks: <ListChecksGlyph size="sm" />,
  settings: <SettingsGlyph size="sm" />,
};

export function RepositoryPage({ repo }: { repo: string }) {
  const named = isRepoName(repo);
  const read = useCodeQuery("code_repo", { name: repo }, { enabled: named, pollMs: REPO_POLL_MS });
  const [tab, setTab] = useTab<RepoTab>("tab", REPO_TABS);
  // THE BRANCHES AND TAGS ITS PICKERS LIST, read here — once, under the
  // repository's current name — while a tab that draws a picker is open, so a
  // move between the root and a path, or between Code and Commits, asks
  // neither listing again (`refs.tsx`'s useRevisions).
  const current = read.data?.repo?.name ?? "";
  const revisions = useRevisions(current, current !== "" && (tab === "code" || tab === "commits"));
  // CHANGES' COUNT AND CHECKS' MARKER on every tab: the live changes, and the default head's verdict.
  const counts = useCodeQuery(
    "code_counts",
    { repo: current, parts: "changes" },
    { enabled: current !== "", pollMs: REPO_POLL_MS },
  );
  const live = counts.data?.counts?.changes?.live;
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
        <CodeNotFound
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
        <RepoTabs
          repo={detail}
          tab={tab}
          onTab={setTab}
          panelId={panelId}
          {...(live !== undefined ? { live } : {})}
        />
      </header>
      {detail.status === "deleted" && <DeletedNotice repo={detail} />}
      {detail.redirect && (
        // A RENAME LEAVES THE OLD NAME REDIRECTING for a while: the page reads
        // the repository it found, and says what it is called now.
        <Callout
          variant="neutral"
          action={
            <a className="t-link" href={repoHref(detail.name)}>
              Open {detail.name}
            </a>
          }
        >
          {`“${repo}” is now called “${detail.name}”`}
          {detail.redirect_until
            ? `; the old name is released ${fmtDateTime(detail.redirect_until)}`
            : ""}
          .
        </Callout>
      )}
      <div
        className="tabpanel"
        role="tabpanel"
        id={panelId}
        aria-labelledby={tabId(panelId, tab)}
        tabIndex={0}
      >
        {tab === "code" && <Code repo={detail} revisions={revisions} />}
        {tab === "changes" && <RepoChanges repo={detail} />}
        {tab === "commits" && <Commits repo={detail} revisions={revisions} />}
        {tab === "branches" && <Branches repo={detail} />}
        {tab === "pushes" && <PushHistory repo={detail.name} />}
        {tab === "checks" && <RepoChecks repo={detail.name} />}
        {tab === "settings" && <Settings repo={detail} />}
      </div>
    </>
  );
}

/** The page's tabs, Changes counting the live changes and Checks marked by the default head. */
function RepoTabs({
  repo,
  tab,
  onTab,
  panelId,
  live,
}: {
  repo: CodeRepoDetail;
  tab: RepoTab;
  onTab: (tab: RepoTab) => void;
  panelId: string;
  /** The live changes, where the count was answered. */
  live?: number;
}) {
  const checks = useDefaultChecks(repo);
  return (
    <Tabs
      overflow="menu"
      variant="underline"
      ariaLabel="The repository's sections"
      value={tab}
      onValueChange={(next) => onTab(next as RepoTab)}
      panelId={panelId}
      items={REPO_TABS.map((value) => ({
        value,
        label: REPO_TAB_LABELS[value],
        icon: REPO_TAB_GLYPHS[value],
        ...(value === "changes" && live !== undefined ? { count: live } : {}),
        ...(value === "branches" ? { count: repo.branches } : {}),
        ...(value === "pushes" ? { count: repo.pushes } : {}),
        ...(value === "checks" && checks ? { marker: checks } : {}),
      }))}
    />
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

/**
 * The repository's marks beside its name: its state where it is not active,
 * what kind it is where it is not a project, a pull mirror, who reads it and
 * a push mirror. Outlined and uncoloured: each is a fact about the
 * repository, never a state of work.
 */
function RepoMarks({ repo }: { repo: CodeRepoDetail }) {
  const state = repoStatusWord(repo.status);
  return (
    <>
      {state && (
        <Tag
          appearance="outline"
          title={
            repo.deleted_at
              ? `Deleted ${fmtDateTime(repo.deleted_at)}`
              : repo.status === "importing"
                ? (importStanding(repo) ?? undefined)
                : undefined
          }
        >
          {state}
        </Tag>
      )}
      {repo.kind === "scratch" && <Tag appearance="outline">Scratch</Tag>}
      {repo.mirrored && (
        <Tag appearance="outline" title={pullMirrorStanding(repo) ?? undefined}>
          Pull mirror
        </Tag>
      )}
      {repo.visibility === "company" ? (
        <Tag
          appearance="outline"
          leadingIcon={<UsersGlyph size="xs" />}
          title="Every seat reads it; every person does."
        >
          Company
        </Tag>
      ) : repo.visibility === "unit" ? (
        <Tag
          appearance="outline"
          leadingIcon={<LockGlyph size="xs" />}
          title="Only its owner's seats read it; every person does."
        >
          Unit
        </Tag>
      ) : (
        <Tag appearance="outline">{repo.visibility}</Tag>
      )}
      {repo.remotes?.mirror_to && (
        <Tag
          appearance="outline"
          leadingIcon={<UploadGlyph size="xs" />}
          title={pushMirrorStanding(repo) ?? undefined}
        >
          Push mirror
        </Tag>
      )}
    </>
  );
}

/**
 * The repository's title as a code host writes it: its owner's path off the
 * chart (each unit a link to it, or the seat), a slash, and its name.
 */
function RepoTitle({ repo }: { repo: CodeRepoDetail }) {
  const org = useOrg();
  const seatBadge = useSeatBadgeOf();
  const { owner } = repo;
  const unit = owner.kind === "seat" ? null : unitByKey(indexOrg(org), owner.ref);
  const steps =
    owner.kind === "seat"
      ? [
          {
            key: owner.ref,
            name: seatBadge(owner.ref).name || owner.ref,
            to: href(seatPath({ handle: owner.ref, name: owner.ref })),
          },
        ]
      : unit
        ? unit.chain.map((u) => ({
            key: u.id,
            name: u.name,
            to: href(unitPlace(u).path, unitPlace(u).query),
          }))
        : [{ key: owner.ref, name: owner.ref, to: "" }];
  const slash = (
    <>
      <span className="code-repo-title__slash" aria-hidden="true">
        {" /"}
      </span>
      <VisuallyHidden>,</VisuallyHidden>{" "}
    </>
  );
  return (
    <span className="code-repo-title">
      <span className="code-repo-title__owner">
        {steps.map((step, i) => (
          <Fragment key={step.key}>
            {i > 0 && slash}
            {step.to ? (
              <a className="code-repo-title__link" href={step.to}>
                {step.name}
              </a>
            ) : (
              step.name
            )}
          </Fragment>
        ))}
      </span>
      {slash}
      <span>{repo.name}</span>
    </span>
  );
}

/**
 * The repository's header: its owner's path and its name, as a code host writes
 * `owner / name`, its marks, and its description. What it holds and where it
 * stands are the About rail's, beside its code: a header carrying the same
 * facts as the rail would be one answer given twice.
 */
function RepoHeader({ repo }: { repo: CodeRepoDetail }) {
  return (
    <ObjectHeader
      kind="Repository"
      icon="square-code"
      title={<RepoTitle repo={repo} />}
      status={<RepoMarks repo={repo} />}
      {...(repo.description ? { description: repo.description } : {})}
    />
  );
}

/**
 * The CODE tab: the files at the revision the address names. At the
 * repository's root they stand beside the About rail, as a code host's
 * landing does; under a path — a directory or a file, read wide — the files
 * take the page's width.
 */
function Code({
  repo,
  revisions,
}: {
  repo: CodeRepoDetail;
  revisions: ReturnType<typeof useRevisions>;
}) {
  const ownerName = useOwnerName();
  const [path] = useParam("path", "", "filter");
  const files = <Files repo={repo} revisions={revisions} />;
  if (path !== "") return files;
  return (
    <AsideLayout
      fit="page"
      asideLabel="About this repository"
      aside={<About repo={repo} ownerName={ownerName} />}
    >
      {files}
    </AsideLayout>
  );
}

/** The states the repository's Changes tab lists by: the engine's, every state among them. */
const REPO_CHANGE_STATES = CHANGE_STATE_FILTERS;

/**
 * The changes proposed to this repository: Gitlet › Changes narrowed to it,
 * in the grid every listing of changes draws — open until the reader picks
 * another state, and every state offered, since the repository narrows the
 * engine's walk.
 */
function RepoChanges({ repo }: { repo: CodeRepoDetail }) {
  const [state, setState] = useParam("state", "open", "filter");
  const params = { repo: repo.name, ...(state === "all" ? {} : { state }) };
  return (
    <>
      <PageToolbar>
        <Select
          size="sm"
          width="auto"
          value={state}
          onChange={(value) => setState(String(value))}
          options={REPO_CHANGE_STATES.map((f) => ({ value: f.value, label: f.label }))}
          ariaLabel="Which changes"
          active={state !== "open"}
        />
        <ButtonLink size="small" variant="ghost" href={changesHref({ repo: repo.name, state })}>
          Open in Changes
        </ButtonLink>
      </PageToolbar>
      <ChangeList
        params={params}
        empty={{
          title: state === "all" ? "No change yet" : `No ${state} change`,
          hint: "A change is opened by a seat's code_open_change, or by a push with the change option.",
        }}
      />
    </>
  );
}

/**
 * One section of the Settings tab, as a code host's settings page draws one:
 * a card titled with what it governs and a line saying so, its controls in
 * its body and its one way to add beside its title.
 */
function SettingsCard({
  title,
  description,
  actions,
  children,
}: {
  title: string;
  description: string;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Card as="section" aria-label={title}>
      <Card.Header divided subtitle={description} subtitlePlacement="below" actions={actions}>
        <Card.Title as="h2">{title}</Card.Title>
      </Card.Header>
      <Card.Body>
        <Stack gap={3}>{children}</Stack>
      </Card.Body>
    </Card>
  );
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
  // WHO OWNS ITS PATHS AND ITS RULES' MERGE GATES are this tab's alone, asked
  // by name (`path_owners`, `gates`): the page's own read of the row, polled
  // every minute, carries neither — at their bound a quarter and half a
  // mebibyte of JSON. Asked once as the tab opens, at the tab's floor, and
  // again after a write of the reader's own, as every code question is.
  const settings = useCodeQuery("code_repo", { name: repo.name, path_owners: true, gates: true });
  const full = settings.data?.repo;
  const gates = new Map((full?.gates ?? []).map((g) => [g.pattern, g.gate]));
  const sources = new Map((full?.rule_sources ?? []).map((s) => [s.pattern, s]));
  const owners = full?.path_owners ?? [];
  // A RULE MAKING PATH OWNERS APPROVE gates its merges on them, so taking a
  // glob from one loosens it, which is a person's.
  const ownersGate = (full?.gates ?? []).some((g) => g.gate.owners_approve);
  const detailState =
    settings.loading && !full ? (
      <Skeleton variant="text" rows={2} label="Loading its rules and owners" />
    ) : (
      <QueryState
        error={settings.error}
        refusal={settings.refusal}
        detail={settings.detail}
        loading={false}
      >
        {null}
      </QueryState>
    );
  const rules = repo.protections ?? [];
  // A DELETED REPOSITORY TAKES NO CHANGE BUT ITS RESTORE, which the notice
  // above the tabs offers: its cards show what it holds, and no write is
  // drawn on them. Drawn disabled, a column of writes read as something the
  // reader lacked rather than something the repository is waiting for.
  const gone = repo.status === "deleted" || repo.status === "purging";
  return (
    <Stack gap={4}>
      {gone ? (
        <Callout variant="neutral">
          While it is deleted nothing of it changes: restore it to change its settings.
        </Callout>
      ) : repo.managed_by === "config" ? (
        <Callout
          variant="neutral"
          action={
            canConfigure && (
              <ButtonLink variant="secondary" size="small" href={href(CONFIG_PATH)}>
                Open the configuration
              </ButtonLink>
            )
          }
        >
          These settings come from the company document, so they are changed in Settings ›
          Configuration.
        </Callout>
      ) : (
        <Callout variant="neutral">
          This repository was made here or by a tool, so it is changed here: its details, its name,
          its protection rules, its path owners, its search mode, its mirrors, its archive and its
          delete are below.
        </Callout>
      )}
      <SettingsCard
        title="Repository"
        description="What it is and who reads it."
        actions={
          gone ? undefined : (
            <>
              <RenameControl repo={repo} />
              <DetailsControl repo={repo} />
            </>
          )
        }
      >
        <PropertiesRail
          headings="sentence"
          groups={[
            {
              properties: [
                {
                  label: "Description",
                  value: repo.description || <EmptyValue label="None" />,
                },
                {
                  label: "Default branch",
                  value: <span className="mono">{repo.default_branch}</span>,
                },
                { label: "Owner", value: ownerName(repo.owner) },
                { label: "Read by", value: readByWord(repo.visibility) },
                { label: "Search", value: searchWord(repo.search) },
                { label: "Managed by", value: managedWord(repo.managed_by) },
              ],
            },
          ]}
        />
      </SettingsCard>
      <SettingsCard
        title="Protection rules"
        description="Who pushes to a branch directly, whether a push may force or delete it, and what a merge into it needs."
        actions={gone ? undefined : <ProtectButton repo={repo} />}
      >
        {rules.length === 0 ? (
          <p className="muted">No rule protects a ref: anybody who may write may push to any.</p>
        ) : !full ? (
          // A RULE IS NEVER DRAWN WITHOUT ITS GATE, which a line with none
          // would read as: the rules wait for the read that carries them.
          detailState
        ) : (
          <ul className="code-rules">
            {rules.map((rule) => {
              const gate = gates.get(rule.pattern);
              const source = sources.get(rule.pattern);
              return (
                <li key={rule.pattern}>
                  <span className="code-rules__what">
                    <span className="mono code-rules__pattern">{rule.pattern}</span>
                    <span className="muted">
                      {pushRuleLine(rule)}
                      {gate ? `; a merge into it is ${gateLine(gate)}` : ""}
                    </span>
                    {source && <span className="t-caption">{ruleSourceLine(source)}</span>}
                  </span>
                  {!gone && <RuleControls repo={repo} held={{ rule, gate }} />}
                </li>
              );
            })}
          </ul>
        )}
      </SettingsCard>
      <SettingsCard
        title="Path owners"
        description="Whose review a change touching their paths asks for."
        actions={gone || !full ? undefined : <AddPathOwnerButton repo={repo} owners={owners} />}
      >
        {!full ? (
          detailState
        ) : owners.length === 0 ? (
          <p className="muted">No path is owned: a change asks reviews of nobody by its paths.</p>
        ) : (
          <ul className="code-rules">
            {owners.map((p) => (
              <li key={`${p.owner.kind}:${p.owner.ref}`}>
                <span className="code-rules__what">
                  <span>{ownerName(p.owner)}</span>
                  <span className="muted mono">{p.paths.join(", ")}</span>
                </span>
                {!gone && (
                  <PathOwnerControls
                    repo={repo}
                    owner={p}
                    owners={owners}
                    name={ownerName(p.owner)}
                    gating={ownersGate}
                  />
                )}
              </li>
            ))}
          </ul>
        )}
      </SettingsCard>
      {!gone && (
        <>
          <SettingsCard
            title="Search mode"
            description="How code search reads the repository: from an index of its default branch, or with git grep when a search names it."
          >
            <SearchModeControl repo={repo} />
          </SettingsCard>
          <SettingsCard
            title="Mirrors"
            description="Another host this repository follows, or pushes to."
          >
            <p className="muted">
              {[pullMirrorStanding(repo), pushMirrorStanding(repo)].filter(Boolean).join(" ") ||
                "No mirror: it follows no other host and pushes to none."}
            </p>
            <MirrorControls repo={repo} />
          </SettingsCard>
          <SettingsCard
            title="Unreachable history"
            description="Objects no ref, change or queue reaches any longer, dropped for good."
          >
            {purgeStanding(repo) && <p className="muted">{purgeStanding(repo)}</p>}
            <PurgeControl repo={repo} />
          </SettingsCard>
          <SettingsCard
            title="Archive"
            description="An archived repository is read-only: still cloned, fetched and read, and takes no push."
          >
            <ArchiveControl repo={repo} />
          </SettingsCard>
          {/* LAST, as the one write that takes the repository away. */}
          <SettingsCard
            title="Delete repository"
            description="A deleted repository takes no clone and no push, and is restored as it was until it is purged."
          >
            <DeleteRepoControl repo={repo} />
          </SettingsCard>
        </>
      )}
    </Stack>
  );
}
