/**
 * Settings › Code — the company's own git host as an administrator runs it:
 * how long its log keeps history, what is wrong with it on any node, and the
 * policy its configuration sets for seats' scratch repositories and the
 * addresses the engine's own commits are written under.
 *
 * THREE ANSWERS THE ENGINE ALREADY SERVES, none a question of its own:
 *
 * - the code log's row of the RETENTION report — its ceiling, what it holds,
 *   the trim's six terms and what holds it — drawn by Backups & retention's
 *   own `LogRow`, so the two pages can never disagree about the log;
 * - the code host's ALARMS on every node, from the FLEET's node rows (each
 *   node's own table, the longest-standing first) — the kinds the engine
 *   names the code host's (`CODE_ALARM_KINDS`), never every alarm the node
 *   has, which Nodes lists;
 * - and the company DOCUMENT's `code` block: the scratch policy
 *   (`code.scratch`) and the identity domain (`code.identity_domain`), each
 *   with the default an unset field reads as, since an unset field is the one
 *   a reader most needs told what it means;
 * - and every repository's BRANCH PROTECTION — the push rules the listing
 *   carries and, as a repository's fold is opened, the merge gate of each
 *   rule from its detail, said in the repository page's own words
 *   (`lib/protection.ts`). Who set a rule and when is not on its row: the
 *   card says which surface sets the repository's rules instead.
 *
 * ADMIN'S, as the rest of the Engine group: the document is read by an admin
 * alone, and the log and the fleet are the engine's. Hidden where the company
 * hosts no code. It edits nothing: the policy is the configuration's, changed
 * where the configuration is.
 */

import { useState } from "react";
import { Card, EmptyState, EmptyValue, InlineCode } from "@crewlethq/ui";
import { CodeGlyph } from "@crewlethq/icons/glyphs";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  CODE_ALARM_KINDS,
  CODE_IDENTITY_DOMAIN,
  CODE_SCRATCH_IDLE_DAYS,
  CODE_SCRATCH_MAX_PER_SEAT,
} from "~/contract/code.ts";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime } from "~/lib/format.ts";
import { gateLine, pushRuleLine } from "~/lib/protection.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type {
  CodeProtection,
  CodeRepoSummary,
  CodeRuleGate,
  CompanyDocument,
  FleetAnswer,
  RetentionDomain,
} from "~/protocol/index.ts";
import { FLEET_POLL_MS } from "./Fleet.tsx";
import { LogRow, useRetentionReport } from "./Retention.tsx";

/** The state log the code host keeps its rows on (`code.Domain`). */
export const CODE_DOMAIN = "code";

export function CodeSettings() {
  const retention = useRetentionReport(true);
  const fleet = useQuery("fleet", undefined, { pollMs: FLEET_POLL_MS });
  const config = useQuery("config");
  const log = retention.data?.domains.find((d) => d.domain === CODE_DOMAIN);
  return (
    <div className="col gap-4">
      <QueryState error={retention.error} refusal={retention.refusal} loading={retention.loading}>
        <CodeLog log={log} answered={retention.data !== null} />
      </QueryState>
      <QueryState error={fleet.error} refusal={fleet.refusal} loading={fleet.loading}>
        {fleet.data && <CodeAlarms fleet={fleet.data} />}
      </QueryState>
      <QueryState error={config.error} refusal={config.refusal} loading={config.loading}>
        <CodePolicy doc={config.data} />
      </QueryState>
      <BranchProtection />
    </div>
  );
}

/**
 * Every repository a rule protects, each rule with what it lets a push do and
 * what its merge gate asks — the gate per pattern, read from the repository's
 * own detail as it is opened, since the listing carries the push rules and
 * not the gates. Where the rules come from is said too: a configured
 * repository's are the company document's, a tool-made one's the code tools'
 * lifecycle requests. Shown, never edited here, as the rest of this page.
 */
function BranchProtection() {
  const read = useCodeQuery("code_repos", {});
  const protectedRepos = (read.data?.repos ?? []).filter((r) => (r.protections ?? []).length > 0);
  return (
    <Card padding="none">
      <Card.Header
        count={protectedRepos.length}
        subtitle="The rules on each repository's branches, and what a merge into each must meet"
        actions={
          <a className="t-link" href={href(["code", "repos"])}>
            Every repository
          </a>
        }
      >
        <Card.Title as="h3">Branch protection</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-2">
        <QueryState
          error={read.error}
          refusal={read.refusal}
          detail={read.detail}
          loading={read.loading && !read.data}
          empty={
            read.data && protectedRepos.length === 0
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
          {read.data?.next && (
            <span className="t-caption">
              More repositories are past this page: the Code workspace lists every one.
            </span>
          )}
        </QueryState>
      </Card.Body>
    </Card>
  );
}

/**
 * One repository's fold. ITS DETAIL IS READ ONLY ONCE OPENED: a company with
 * many protected repositories would otherwise ask each one's detail on every
 * visit to this page, for gates nobody unfolded.
 */
function RepoFold({ repo }: { repo: CodeRepoSummary }) {
  const [open, setOpen] = useState(false);
  return (
    <details onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary>
        <span className="mono">{repo.name}</span>{" "}
        <span className="muted">— {plural(repo.protections?.length ?? 0, "rule")}</span>
      </summary>
      {open && <RepoProtection repo={repo} />}
    </details>
  );
}

/** One repository's rules with their gates. */
function RepoProtection({ repo }: { repo: CodeRepoSummary }) {
  const read = useCodeQuery("code_repo", { name: repo.name });
  const lines = protectionLines(repo.protections ?? [], read.data?.repo?.gates);
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
            the code tools’ lifecycle requests (<InlineCode>code_protect_branch</InlineCode>)
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
 * reads the same as a detail not yet read.
 */
export function protectionLines(
  rules: readonly CodeProtection[],
  gates: readonly CodeRuleGate[] | undefined,
): { pattern: string; says: string }[] {
  const byPattern = new Map((gates ?? []).map((g) => [g.pattern, g.gate]));
  return rules.map((rule) => {
    const gate = byPattern.get(rule.pattern);
    const merge = gate ? `; a merge into it is ${gateLine(gate)}` : gates ? "; no merge gate" : "";
    return { pattern: rule.pattern, says: `${pushRuleLine(rule)}${merge}` };
  });
}

/** The code log's retention: its row of the report, or why there is none. */
function CodeLog({ log, answered }: { log: RetentionDomain | undefined; answered: boolean }) {
  return (
    <Card padding="none">
      <Card.Header
        icon={<CodeGlyph size="sm" />}
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
          description="The node that answered keeps no copy of the code log: it runs no data role, or the code host has not started on it yet."
        />
      ) : null}
    </Card>
  );
}

/** One code-host alarm firing on one node. */
interface NodeAlarm {
  node: string;
  kind: string;
  since: string;
}

/**
 * The code host's alarms on every node the fleet lists, the longest-standing
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

function CodeAlarms({ fleet }: { fleet: FleetAnswer }) {
  const now = useNow();
  const { firing, silent } = codeAlarmsOf(fleet);
  return (
    <Card padding="none">
      <Card.Header
        count={firing.length}
        subtitle="The code host's alarms, as each node's own table found them at its last beat"
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
          <span className="t-caption">No code alarm is firing on any node that said.</span>
        ) : (
          <ul className="col gap-1" aria-label="Code alarms firing">
            {firing.map((a) => (
              <li key={`${a.node}\u0000${a.kind}`}>
                <InlineCode>{a.kind}</InlineCode> on{" "}
                <a className="t-link prose-link mono" href={href(["settings", "nodes", a.node])}>
                  {a.node}
                </a>
                {a.since && <span className="muted"> — since {relTime(a.since, now)}</span>}
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

/** A number the document sets, or the default an unset one reads as. */
function setOr(value: unknown, fallback: number): { value: number; set: boolean } {
  return typeof value === "number" ? { value, set: true } : { value: fallback, set: false };
}

/** The `code` block's scratch policy and identity domain, each with its default. */
export function codePolicyOf(doc: CompanyDocument | null) {
  const code = (doc?.code ?? {}) as Record<string, unknown>;
  const scratch = (code.scratch ?? {}) as Record<string, unknown>;
  const domain = typeof code.identity_domain === "string" ? code.identity_domain : "";
  return {
    idleDays: setOr(scratch.idle_days, CODE_SCRATCH_IDLE_DAYS),
    maxPerSeat: setOr(scratch.max_per_seat, CODE_SCRATCH_MAX_PER_SEAT),
    identityDomain: { value: domain || CODE_IDENTITY_DOMAIN, set: domain !== "" },
  };
}

function CodePolicy({ doc }: { doc: CompanyDocument | null }) {
  const policy = codePolicyOf(doc);
  const unset = (set: boolean) => (set ? "" : " (unset: the default)");
  return (
    <Card padding="none">
      <Card.Header
        subtitle="What the configuration's code block sets"
        actions={
          <a className="t-link" href={href(["settings", "config"])}>
            Configuration
          </a>
        }
      >
        <Card.Title as="h3">Scratch repositories and commit identity</Card.Title>
      </Card.Header>
      <Card.Body padding="md">
        <PropertiesRail
          groups={[
            {
              properties: [
                {
                  label: "Idle scratch deleted after",
                  value: `${policy.idleDays.value} days${unset(policy.idleDays.set)}`,
                },
                {
                  label: "Scratch repositories a seat may hold",
                  value:
                    policy.maxPerSeat.value === 0
                      ? `None: no seat may create one${unset(policy.maxPerSeat.set)}`
                      : `${policy.maxPerSeat.value}${unset(policy.maxPerSeat.set)}`,
                },
                {
                  label: "Identity domain",
                  value: policy.identityDomain.value ? (
                    <span>
                      <InlineCode>{policy.identityDomain.value}</InlineCode>
                      {unset(policy.identityDomain.set)}
                    </span>
                  ) : (
                    <EmptyValue label="Unset" />
                  ),
                },
              ],
            },
          ]}
        />
      </Card.Body>
    </Card>
  );
}
