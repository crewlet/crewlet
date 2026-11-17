/**
 * Settings › Gitlet — Gitlet, the engine's own git host, as an administrator
 * runs it: how long its log keeps history, what is wrong with it on any node,
 * and the policy its configuration sets for seats' scratch repositories and
 * the addresses the engine's own commits are written under.
 *
 * FOUR ANSWERS THE ENGINE ALREADY SERVES, none a question of its own:
 *
 * - the code log's row of the RETENTION report — its ceiling, what it holds,
 *   the trim's six terms and what holds it — drawn by Backups & retention's
 *   own `LogRow`, so the two pages can never disagree about the log;
 * - Gitlet's ALARMS on every node, from the FLEET's node rows (each
 *   node's own table, the longest-standing first) — the kinds the engine
 *   names Gitlet's (`CODE_ALARM_KINDS`), never every alarm the node
 *   has, which Nodes lists;
 * - the scratch policy as the engine's own rows (`limits`, group `code`):
 *   `code.scratch.idle_days` and `code.scratch.max_per_seat`, each value
 *   beside the engine's default and changed in place by the field Settings ›
 *   Limits changes a cap with, and the identity domain
 *   (`code.identity_domain`) as the engine's row of it (`settings`, group
 *   `code`), beside its default and changed in place by its text field;
 * - and every repository's BRANCH PROTECTION — the engine's listing of the
 *   protected repositories, a page at a time, with the push rules it carries
 *   and, as a repository's fold is first opened, the merge gate of each rule
 *   from its detail, asked for by name, said in the repository page's own
 *   words (`lib/protection.ts`), with who last changed each rule and when,
 *   which the same detail answers beside the gates (`rule_sources`), and
 *   which surface sets the repository's rules.
 *
 * ADMIN'S, as the rest of the Engine group: the document is read by an admin
 * alone, and the log and the fleet are the engine's. Hidden where the company
 * hosts no code. The scratch policy and the identity domain are changed
 * where they are read.
 */

import { useState } from "react";
import { Button, Card, Disclosure, EmptyState, InlineCode, SettingsTable } from "@crewlethq/ui";
import { BellGlyph, GitBranchGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { alarmTitle } from "~/lib/alarms.ts";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { CODE_ALARM_KINDS } from "~/contract/code.ts";
import { LimitSetting } from "~/components/LimitSetting.tsx";
import { TextSetting } from "~/components/TextSetting.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural } from "~/lib/format.ts";
import { gateLine, pushRuleLine, ruleSourceLine } from "~/lib/protection.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type {
  CodeProtection,
  CodeRepoSummary,
  CodeReposAnswer,
  CodeRuleGate,
  CodeRuleSource,
  FleetAnswer,
  RetentionDomain,
} from "~/protocol/index.ts";
import { FLEET_POLL_MS } from "./Fleet.tsx";
import { LogRow, useRetentionReport } from "./Retention.tsx";
import "~/styles/gitlet.css";
import { ago } from "~/lib/ago.ts";

/** The state log Gitlet keeps its rows on (`gitlet.Domain`). */
export const CODE_DOMAIN = "code";

export function GitletSettings() {
  const retention = useRetentionReport(true);
  const fleet = useQuery("fleet", undefined, { pollMs: FLEET_POLL_MS });
  const log = retention.data?.domains.find((d) => d.domain === CODE_DOMAIN);
  return (
    <div className="col gap-4">
      <QueryState error={retention.error} refusal={retention.refusal} loading={retention.loading}>
        <CodeLog log={log} answered={retention.data !== null} />
      </QueryState>
      <QueryState error={fleet.error} refusal={fleet.refusal} loading={fleet.loading}>
        {fleet.data && <CodeAlarms fleet={fleet.data} />}
      </QueryState>
      <CodePolicy />
      <BranchProtection />
    </div>
  );
}

/**
 * The listing of the repositories a rule protects, as the card asks it: the
 * engine's own filter (`protected`), walked by the index of those alone, so
 * every page holds protected repositories whatever the company holds besides.
 */
export const PROTECTED_REPOS = { protected: true } as const;

/**
 * Every repository a rule protects, each rule with what it lets a push do and
 * what its merge gate asks — the gate per pattern, read from the repository's
 * own detail as it is opened, since the listing carries the push rules and
 * not the gates. Where the rules come from is said too: a configured
 * repository's are the company document's, a tool-made one's the code tools'
 * lifecycle requests. Shown, never edited here, as the rest of this page.
 *
 * A PAGE AT A TIME, every page the engine's: the listing asks for the
 * protected repositories alone (`PROTECTED_REPOS`) and Load more asks for
 * the page after, so a company with more of them than a page sees every one —
 * filtered here instead, a protected repository past the first page of EVERY
 * repository was never shown. The count is said once nothing is left to page
 * to: before then it is only what has loaded.
 */
export function BranchProtection() {
  const read = useCodeQuery("code_repos", PROTECTED_REPOS);
  const paged = usePaged(
    read,
    PROTECTED_REPOS,
    (cursor) => codeRest.query("code_repos", { ...PROTECTED_REPOS, cursor }),
    (answer: CodeReposAnswer) => answer.repos ?? [],
    (repo: CodeRepoSummary) => repo.id,
    (answer: CodeReposAnswer) => answer.next ?? "",
  );
  const protectedRepos = paged.items;
  return (
    <Card padding="none">
      <Card.Header
        count={paged.more ? undefined : protectedRepos.length}
        subtitle="The rules on each repository's branches, and what a merge into each must meet"
        actions={
          <a className="t-link" href={href(["gitlet", "repos"])}>
            Every repository
          </a>
        }
      >
        <Card.Title as="h3">Branch protection</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-2">
        <QueryState
          error={paged.error}
          refusal={paged.refusal}
          detail={read.detail}
          loading={paged.loading && protectedRepos.length === 0}
          empty={
            // A PAGE THAT HELD NONE MAY STILL HAVE A CURSOR — what the engine
            // scanned and the reader may not read — so nothing is protected
            // only where nothing is left to page to.
            read.data && protectedRepos.length === 0 && !paged.more
              ? {
                  title: "No branch is protected",
                  hint: "A protection is declared under code.repos, or set with code_protect_branch.",
                }
              : undefined
          }
        >
          {protectedRepos.map((repo) => (
            <RepoFold key={repo.id} repo={repo} />
          ))}
        </QueryState>
        {(paged.more || paged.pageError) && (
          <div className="row gap-2 wrap">
            {paged.more && (
              <Button
                size="small"
                variant="secondary"
                onClick={paged.next}
                disabled={paged.paging}
                loading={paged.paging}
              >
                Load more
              </Button>
            )}
            {paged.pageError && <PageFailed error={paged.pageError} />}
          </div>
        )}
      </Card.Body>
    </Card>
  );
}

/**
 * One repository's fold, on the kit's Disclosure as the code log's row above
 * it is (one chevron, hover row and focus ring on the page, where a native
 * `<details>` drew the browser's own triangle). ITS DETAIL IS READ ONLY ONCE
 * OPENED: a company with many protected repositories would otherwise ask each
 * one's detail on every visit, for gates nobody unfolded. AND KEPT ONCE READ:
 * closed again, it stays mounted, so opening it again asks nothing.
 */
function RepoFold({ repo }: { repo: CodeRepoSummary }) {
  const [opened, setOpened] = useState(false);
  return (
    <Disclosure
      title={repo.name}
      mono
      size="compact"
      meta={plural(repo.protections?.length ?? 0, "rule")}
      onOpenChange={(open) => {
        if (open) setOpened(true);
      }}
    >
      {opened && <RepoProtection repo={repo} />}
    </Disclosure>
  );
}

/**
 * One repository's rules with their gates — asked for by name (`gates`): a
 * repository's question answers its row alone unless asked for more, and
 * this fold reads no path owner.
 */
export function RepoProtection({ repo }: { repo: CodeRepoSummary }) {
  const read = useCodeQuery("code_repo", { name: repo.name, gates: true });
  const detail = read.data?.repo;
  const lines = protectionLines(repo.protections ?? [], detail?.gates, detail?.rule_sources);
  return (
    <div className="col gap-1">
      <span className="t-caption">
        Set by{" "}
        {repo.managed_by === "config" ? (
          <>
            the company document, in <InlineCode>code.repos</InlineCode>
          </>
        ) : (
          <>
            the code tools’ lifecycle requests{" "}
            <InlineCode open="(" close=")">
              code_protect_branch
            </InlineCode>
          </>
        )}
        .
      </span>
      <QueryState error={read.error} refusal={read.refusal} detail={read.detail} loading={false}>
        <ul className="code-rules" aria-label={`${repo.name}'s protection rules`}>
          {lines.map((line) => (
            <li key={line.pattern}>
              <span className="mono">{line.pattern}</span>
              <span className="muted">{line.says}</span>
              {line.source && <span className="t-caption">{line.source}</span>}
            </li>
          ))}
        </ul>
      </QueryState>
    </div>
  );
}

/**
 * Each rule of a repository in a line: what it lets a push do and, once the
 * repository's detail has answered (`gates`), what a merge into it must meet
 * — a rule with no gate SAID to have none, never left blank, since a blank
 * reads the same as a detail not yet read — and who last changed it and
 * when, from the same detail (`rule_sources`), absent until it answers.
 */
export function protectionLines(
  rules: readonly CodeProtection[],
  gates: readonly CodeRuleGate[] | undefined,
  sources?: readonly CodeRuleSource[],
): { pattern: string; says: string; source?: string }[] {
  const byPattern = new Map((gates ?? []).map((g) => [g.pattern, g.gate]));
  const sourceOf = new Map((sources ?? []).map((s) => [s.pattern, s]));
  return rules.map((rule) => {
    const gate = byPattern.get(rule.pattern);
    const merge = gate ? `; a merge into it is ${gateLine(gate)}` : gates ? "; no merge gate" : "";
    const source = sourceOf.get(rule.pattern);
    return {
      pattern: rule.pattern,
      says: `${pushRuleLine(rule)}${merge}`,
      ...(source ? { source: ruleSourceLine(source) } : {}),
    };
  });
}

/** The code log's retention: its row of the report, or why there is none. */
function CodeLog({ log, answered }: { log: RetentionDomain | undefined; answered: boolean }) {
  return (
    <Card padding="none">
      <Card.Header
        subtitle="Its ceiling, what it holds, and the six checks its trim waits on"
        actions={
          <a className="t-link" href={href(["settings", "backups"])}>
            Every log
          </a>
        }
      >
        <Card.Title as="h3">The code log</Card.Title>
      </Card.Header>
      {log ? (
        <LogRow domain={log} />
      ) : answered ? (
        <EmptyState
          size="compact"
          headingLevel="none"
          title="No code log on this node"
          description="The node that answered keeps no copy of the code log: it runs no data role, or Gitlet has not started on it yet."
        />
      ) : null}
    </Card>
  );
}

/** One Gitlet alarm firing on one node. */
interface NodeAlarm {
  node: string;
  kind: string;
  since: string;
}

/**
 * Gitlet's alarms on every node the fleet lists, the longest-standing
 * first — and the nodes that could not say, which are not "no alarms".
 */
export function codeAlarmsOf(fleet: FleetAnswer): { firing: NodeAlarm[]; silent: string[] } {
  const kinds = new Set<string>(CODE_ALARM_KINDS);
  const firing: NodeAlarm[] = [];
  const silent: string[] = [];
  for (const node of fleet.nodes) {
    if (!node.alarms) {
      silent.push(node.id);
      continue;
    }
    for (const a of node.alarms) {
      if (kinds.has(a.kind)) firing.push({ node: node.id, kind: a.kind, since: a.since });
    }
  }
  firing.sort((a, b) => (a.since || "￿").localeCompare(b.since || "￿"));
  return { firing, silent };
}

/**
 * Nothing firing, said of the nodes it is true of: every node, or those that
 * have evaluated their table (the rest are named under it).
 */
export function quietWords(silent: readonly string[]): string {
  return silent.length === 0
    ? "No code alarm is firing on any node."
    : "No code alarm is firing on the nodes that have reported.";
}

function CodeAlarms({ fleet }: { fleet: FleetAnswer }) {
  const now = useNow();
  const { firing, silent } = codeAlarmsOf(fleet);
  return (
    <Card padding="none">
      <Card.Header
        icon={<BellGlyph size="sm" />}
        count={firing.length}
        subtitle="Gitlet's alarms, as each node's own table found them at its last beat"
        actions={
          <a className="t-link" href={href(["settings", "nodes"])}>
            Every node
          </a>
        }
      >
        <Card.Title as="h3">Code alarms</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-2">
        {firing.length === 0 ? (
          // A WHOLE SENTENCE, and one that is true of the nodes it covers:
          // "on any node that said" left its reader to work out which ones.
          <span className="t-caption">{quietWords(silent)}</span>
        ) : (
          <ul className="col gap-1" aria-label="Code alarms firing">
            {firing.map((a) => (
              <li key={`${a.node}\u0000${a.kind}`}>
                {alarmTitle(a)} on{" "}
                <a className="t-link prose-link mono" href={href(["settings", "nodes", a.node])}>
                  {a.node}
                </a>
                {a.since && (
                  <span className="muted">, since {ago(a.since, now, { since: true })}</span>
                )}
              </li>
            ))}
          </ul>
        )}
        {silent.length > 0 && (
          <span className="t-caption">
            {silent.length === 1 ? "One node has" : `${silent.length} nodes have`} evaluated no
            alarm table yet: {silent.join(", ")}.
          </span>
        )}
      </Card.Body>
    </Card>
  );
}

/** What each scratch setting is called, by its key in the company document. */
const CODE_POLICY_LABELS: Readonly<Record<string, string>> = {
  "code.scratch.idle_days": "Idle scratch repositories are deleted after",
  "code.scratch.max_per_seat": "Scratch repositories a seat may hold",
};

/**
 * The `code` block's scratch policy and identity domain, each the engine's own
 * row changed in place: the two numbers by the field Settings › Limits changes
 * a cap with (`limits`, group `code`), and the identity domain by its text
 * field (`settings`, group `code`), each checked by the engine first.
 */
export function CodePolicy() {
  const limits = useQuery("limits");
  const settings = useQuery("settings");
  const rows = (limits.data?.limits ?? []).filter((r) => r.group === "code");
  const domain = settings.data?.settings.find((r) => r.key === "code.identity_domain");
  const refetch = () => {
    limits.refetch();
    settings.refetch();
  };
  return (
    <Card padding="none">
      <Card.Header
        icon={<GitBranchGlyph size="sm" />}
        count={rows.length + (domain ? 1 : 0)}
        subtitle="What the configuration's code block sets"
        actions={
          <a className="t-link" href={href(["settings", "config"])}>
            Configuration
          </a>
        }
      >
        <Card.Title as="h3" id="code-policy">
          Scratch repositories and commit identity
        </Card.Title>
      </Card.Header>
      <QueryState
        error={limits.error ?? settings.error}
        refusal={limits.refusal ?? settings.refusal}
        loading={(limits.loading && !limits.data) || (settings.loading && !settings.data)}
      >
        <SettingsTable aria-labelledby="code-policy">
          {rows.map((cap) => (
            <LimitSetting
              key={cap.key}
              cap={cap}
              label={CODE_POLICY_LABELS[cap.key] ?? cap.key}
              area="Gitlet"
              reread="Reload to see the code policy as it is now."
              onApplied={refetch}
            />
          ))}
          {domain && (
            <TextSetting
              row={domain}
              label="Identity domain"
              area="Gitlet"
              reread="Reload to see the code policy as it is now."
              onApplied={refetch}
              hint="Commits are addressed engine@ and handle@ it, for a seat with no email."
            />
          )}
        </SettingsTable>
      </QueryState>
    </Card>
  );
}
