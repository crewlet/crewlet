/** The rail beside Gitlet › Pushes: pushes by day, who pushes here, and the repository's protection. */

import { Skeleton } from "@crewlethq/ui";
import { GitBranchGlyph, ShieldGlyph } from "@crewlethq/icons/glyphs";
import { gateLine, pushRuleLine } from "~/lib/protection.ts";
import type { QueryResult } from "~/lib/useQuery.ts";
import type { CodeRepoAnswer } from "~/protocol/index.ts";
import { repoHref } from "./paths.ts";
import { Note, PushActivity, Section, unreadLine, type PushActivityRead } from "./railParts.tsx";
import "~/styles/gitlet.css";

/** Patterns that cover every ref, so no "every other" line is drawn under them. */
const EVERY_REF = new Set(["**", "refs/**"]);

export function PushesRail({
  repo,
  activity,
  rules,
}: {
  repo: string;
  activity: PushActivityRead;
  rules: QueryResult<CodeRepoAnswer>;
}) {
  return (
    <div className="code-repo-rail">
      <PushActivity activity={activity} sparkFirst />
      <Protection repo={repo} read={rules} />
    </div>
  );
}

/** Each rule in the settings' own words, and what is left to anybody who may write. */
function Protection({ repo, read }: { repo: string; read: QueryResult<CodeRepoAnswer> }) {
  const detail = read.data?.repo;
  const link = (
    <a
      className="t-link"
      href={repoHref(repo, { tab: "settings" })}
      aria-label="Its rules in settings"
    >
      Rules
    </a>
  );
  if (!detail) {
    return (
      <Section title="Protection" end={link}>
        {read.error ? (
          <Note>
            {unreadLine(read, "Its rules are unavailable", "Its rules could not be read.")}
          </Note>
        ) : (
          <Skeleton variant="text" rows={2} label="Loading its protection" />
        )}
      </Section>
    );
  }
  const rules = detail.protections ?? [];
  const gates = new Map((detail.gates ?? []).map((g) => [g.pattern, g.gate]));
  if (rules.length === 0) {
    return (
      <Section title="Protection" end={link}>
        <Note>No rule protects a ref: anybody who may write may push to any.</Note>
      </Section>
    );
  }
  return (
    <Section title="Protection" end={link}>
      <ul className="code-repo-rail__list">
        {rules.map((rule) => {
          const gate = gates.get(rule.pattern);
          return (
            <li key={rule.pattern} className="code-repo-rail__fact">
              <ShieldGlyph size="sm" className="code-repo-rail__glyph" />
              <span className="code-repo-rail__words">
                <strong className="mono code-pushes-rule">
                  {rule.pattern.replace(/^refs\/heads\//, "")}
                </strong>
                {pushRuleLine(rule)}
                {gate ? `; a merge into it is ${gateLine(gate)}` : ""}
              </span>
            </li>
          );
        })}
        {!rules.some((r) => EVERY_REF.has(r.pattern)) && (
          <li className="code-repo-rail__fact">
            <GitBranchGlyph size="sm" className="code-repo-rail__glyph" />
            <span className="code-repo-rail__words">
              Every other branch and tag: anybody who may write pushes to it.
            </span>
          </li>
        )}
      </ul>
    </Section>
  );
}
