/**
 * The parts a detail pane on Company › Integrations is composed from: the
 * issues with their fixes, the notes on a working integration, the settings an
 * app holds, what to have ready before connecting, and the engine's address at
 * the foot of the directory.
 */

import { useCallback, useId, useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Copyable,
  DescriptionList,
  EmptyValue,
  EntityChip,
  IconTile,
  List,
  StatusDot,
  Tag,
  Text,
  type CatalogTileStatus,
} from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { ProductMark } from "@crewlethq/icons/product-mark";
import { CheckGlyph, InfoGlyph } from "@crewlethq/icons/glyphs";
import type { ReconcileFinding } from "~/contract/integrations.ts";
import { asSentence, relTime } from "~/lib/format.ts";
import { marked } from "~/ui/Problems.tsx";
import { href } from "~/app/router.tsx";
import { DisconnectDialog } from "../DisconnectDialog.tsx";
// THE SCREEN'S OWN SHEET, with every module of the integrations screen (its
// directory, panes, setup form and disconnect dialog), which all reach it here.
import "~/styles/integrations.css";
import { FindingSubjects, docHref } from "./catalogue.tsx";
import {
  BUCKET_WORDS,
  OWNER_WORDS,
  type BuiltInService,
  type DropPlan,
  type Intent,
  type Issue,
  type Model,
  type Tool,
} from "./model.ts";

/**
 * What an app's action is called to a screen reader: its visible words, then
 * whose it is ("Open Worklet", "Replace key GitLab").
 *
 * A COLUMN OF CARDS EACH SAYING "Open" is a list of identical link names, and
 * a reader listing the page's links could not tell one card's from the next.
 * The visible words come first, so a person speaking what they see still
 * reaches the control by them. One spelling for the app's page and the home.
 */
export function actionName(words: string, name: string): string {
  return `${words} ${name}`;
}

/** A built-in service's own mark. */
export function ServiceMark({ service, size }: { service: BuiltInService; size: "md" | number }) {
  return <ProductMark product={service.key} size={size} />;
}

/** A tool's state as one tag, only where it carries meaning. */
export function BucketTag({ tool }: { tool: Tool }) {
  if (tool.bucket === "available") return null;
  const words = BUCKET_WORDS[tool.bucket];
  return (
    <Tag size="sm" variant={words.tone}>
      {words.label}
    </Tag>
  );
}

/**
 * A built-in service's state, in the words and tone every place that names
 * it draws: the catalogue's tile and the directory's row as a dot and the
 * word, the detail's header as its pill ([ServiceState]). An app's state is
 * the same shape (`BUCKET_WORDS`), so "In use" and "Connected" are drawn one
 * way side by side.
 *
 * NOTHING UNTIL THE ENGINE HAS SAID: "Not in use" on a node running no company
 * would be a guess. A SERVICE IS IN USE, never connected: it is Crewlet's own,
 * so there is nothing to connect.
 */
export function serviceStatus(service: BuiltInService): CatalogTileStatus | undefined {
  if (!service.known) return undefined;
  return service.on
    ? { tone: "success", label: "In use" }
    : { tone: "neutral", label: "Not in use" };
}

/** A service's state as the detail header's pill. */
export function ServiceState({ service }: { service: BuiltInService }) {
  const status = serviceStatus(service);
  if (!status) return null;
  return (
    <Tag size="sm" variant={status.tone} appearance={service.on ? undefined : "outline"}>
      {status.label}
    </Tag>
  );
}

/** A service's state as a dot and the word, as an app's row draws its own. */
export function ServiceDot({ service }: { service: BuiltInService }) {
  const status = serviceStatus(service);
  return status ? <StatusDot tone={status.tone}>{status.label}</StatusDot> : null;
}

/**
 * The notes on a working integration: what the loop noticed that is worth
 * reading once (an address the vendor cannot reach, what its events cover),
 * each its own row, led by its title in bold, then what it means, then what
 * would change it at a quieter step.
 *
 * THE ENGINE LAYS THEM OUT, not this screen: a note's lead, sentence and
 * remedy arrive as three fields, so nothing here reads one sentence apart. A
 * config key, a `${VAR}` or a backticked address in one is set as the literal
 * it is ([marked]).
 */
export function Notes({ notes }: { notes: ReconcileFinding[] }) {
  const heading = useId();
  if (notes.length === 0) return null;
  return (
    <section aria-labelledby={heading} className="int-notes">
      <h3 id={heading} className="t-label">
        Notes
      </h3>
      <List variant="divided" density="compact">
        {notes.map((n) => (
          <List.Item
            key={`${n.kind}:${n.subject ?? ""}`}
            leading={<InfoGlyph size="sm" aria-hidden />}
          >
            <span className="int-note">
              <Text>
                {n.title && <strong>{asSentence(n.title)} </strong>}
                {marked(asSentence(n.detail || n.kind.replace(/_/g, " ")))}
              </Text>
              {n.remedy && (
                <Text variant="caption" tone="secondary">
                  {marked(asSentence(n.remedy))}
                </Text>
              )}
            </span>
          </List.Item>
        ))}
      </List>
    </section>
  );
}

/**
 * The things an issue is about: each agent the chart has as its own chip, its
 * name linking to its page, and anything else the engine names (an address,
 * a project) as the plain subject it is.
 */
function IssueSubjects({ issue }: { issue: Issue }) {
  const agents = new Set(issue.agents.map((a) => a.handle));
  return (
    <>
      {issue.agents.length > 0 && (
        <span
          className="int-issue-agents"
          role="group"
          aria-label={`agents, ${issue.agents.length}`}
        >
          {issue.agents.map((agent) => (
            <EntityChip
              key={agent.handle}
              name={agent.name}
              kind={agent.kind}
              size="xs"
              href={href(["company", "org", "seats", agent.handle])}
            />
          ))}
        </span>
      )}
      <FindingSubjects subjects={issue.subjects.filter((s) => !agents.has(s))} />
    </>
  );
}

/**
 * One issue, as a tile: what is wrong, who owes the next move, the agents it
 * is about, and the one control that makes it.
 *
 * THE APP IS NAMED ONCE, by its mark and on the line under the sentence,
 * never in the sentence: the engine's sentence is the finding alone, and a
 * row that led it with the app's name read "GitHub: GitHub: ..." where the
 * finding named its lane as well.
 *
 * THE BUTTON SAYS THE FIX. A control here that answers it is the fix (Replace
 * key), and in a list across apps it opens on the app's page with that
 * control; a fix only the vendor takes goes there ("Fix on GitHub"); anything
 * else in a list across apps sends the reader to the app's page.
 */
export function IssueRow({
  tool,
  issue,
  onFix,
  showTool = false,
  go,
}: {
  tool: Tool;
  issue: Issue;
  /** Open the control that answers it here. Absent where the row links to the app's page instead. */
  onFix?: (intent: Intent) => void;
  /** Name the app the issue is about, for a list across apps. */
  showTool?: boolean;
  /** The app's page, for a list across apps: where its fix opens. */
  go?: string;
}) {
  const variant = issue.owner === "you" ? "primary" : "secondary";
  const action =
    issue.fix && onFix ? (
      <Button
        size="small"
        variant={variant}
        disabledReason={tool.blocked}
        onClick={() => {
          if (!tool.blocked && issue.fix) onFix(issue.fix.intent);
        }}
      >
        {issue.fix.label}
      </Button>
    ) : issue.fix && go ? (
      <ButtonLink
        size="small"
        variant={variant}
        href={go}
        aria-label={actionName(issue.fix.label, tool.entry.name)}
      >
        {issue.fix.label}
      </ButtonLink>
    ) : issue.external ? (
      <ButtonLink size="small" variant={variant} external href={issue.external.href}>
        {issue.external.label}
      </ButtonLink>
    ) : go ? (
      <ButtonLink size="small" variant="secondary" href={go}>
        Open {tool.entry.name}
      </ButtonLink>
    ) : null;
  return (
    <div className="int-issue" data-tone={issue.tone}>
      {showTool && (
        <IconTile size="md" tone="neutral" className="int-issue-mark">
          <VendorMark vendor={tool.entry.vendor} size="md" />
        </IconTile>
      )}
      <div className="int-issue-text">
        <span className="int-issue-problem">{issue.problem}</span>
        {issue.remedy && <span className="t-caption">{issue.remedy}</span>}
        <span className="int-issue-meta">
          <StatusDot
            tone={issue.tone === "danger" ? "danger" : issue.owner === "you" ? "warning" : "info"}
          >
            {issue.owner === "you" ? "Waiting on you" : `${OWNER_WORDS[issue.owner]} is on it`}
          </StatusDot>
          {showTool && (
            <>
              <span className="int-issue-sep" aria-hidden>
                ·
              </span>
              <span>{tool.entry.name}</span>
            </>
          )}
        </span>
        <IssueSubjects issue={issue} />
      </div>
      {action && <span className="int-issue-act">{action}</span>}
    </div>
  );
}

/** What a tool is set to, minus credentials, which are named and never shown. */
export function SettingsSummary({ tool }: { tool: Tool }) {
  const reqs = tool.sections
    .filter((s) => s.seat === undefined)
    .flatMap((s) => s.tool.requirements)
    .filter((r, i, all) => !r.hidden && all.findIndex((o) => o.field === r.field) === i);
  if (reqs.length === 0) {
    return <p className="t-caption">Nothing to set for the company as a whole.</p>;
  }
  return (
    <DescriptionList
      dense
      items={reqs.map((r) => [
        r.label,
        r.kind === "secret" ? (
          r.present ? (
            <StatusDot tone="success">{r.mintable ? "Made by Crewlet" : "Stored"}</StatusDot>
          ) : (
            <StatusDot tone="neutral">Not set</StatusDot>
          )
        ) : r.value || r.default ? (
          <span className="mono">
            {r.choices?.find((c) => c.value === (r.value || r.default))?.label ??
              (r.value || r.default)}
          </span>
        ) : (
          <EmptyValue label="Not set" />
        ),
      ])}
    />
  );
}

/**
 * What connecting gives the agents, how each gets in, and what to have ready.
 *
 * NOTHING THE HEADER ABOVE IT SAYS: the tool's mark, name and one-line
 * description are the header's, and the engine's address is said once, at
 * the foot of the directory.
 */
export function BeforeYouStart({ tool }: { tool: Tool }) {
  return (
    <div className="int-before">
      <section aria-label="What your agents can do">
        <h3 className="t-label">Your agents can</h3>
        <List variant="plain" className="int-checks">
          {tool.story.does.map((line) => (
            <List.Item key={line} leading={<CheckGlyph size="sm" aria-hidden />}>
              {line}
            </List.Item>
          ))}
        </List>
      </section>
      <section aria-label="How each agent gets in">
        <h3 className="t-label">Each agent</h3>
        <p className="t-body">{tool.story.identity}</p>
      </section>
      <section aria-label="What you need">
        <h3 className="t-label">Have ready</h3>
        <List variant="numbered">
          {tool.asks.map((r) => (
            <List.Item key={r.field}>{r.label}</List.Item>
          ))}
          {tool.story.byHand && <List.Item>{tool.story.byHand}</List.Item>}
          {tool.asks.length === 0 && !tool.story.byHand && (
            <List.Item>Nothing. Crewlet fills in the rest.</List.Item>
          )}
        </List>
      </section>
    </div>
  );
}

/** Docs for a tool, as a quiet link. */
export function DocsLink({ tool }: { tool: Tool }) {
  return (
    <ButtonLink size="small" variant="ghost" external href={docHref(tool.entry)}>
      How {tool.entry.name} works
    </ButtonLink>
  );
}

/**
 * The address apps deliver to, said once, at the foot of the directory: a
 * small label over the address, cut to fit with the whole of it in its title,
 * copied with a press.
 */
export function EngineAddress({ value }: { value: string }) {
  return (
    <div className="int-engine-address">
      <span className="t-caption">Apps reach this engine at</span>
      <Copyable
        value={value}
        display={value}
        monospace
        truncate
        title={value}
        ariaLabel={`Copy the engine's address, ${value}`}
      />
    </div>
  );
}

/** The disconnect dialog, opened from a tool's header. Nothing without a model to plan from. */
export function useDisconnect(model: Model | null) {
  const [dropping, setDropping] = useState<DropPlan | null>(null);
  const disconnect = useCallback(
    (tool: Tool) => setDropping(model ? model.dropFor(tool) : null),
    [model],
  );
  const node = model && dropping && (
    <DisconnectDialog
      name={dropping.name}
      kinds={dropping.kinds}
      stuck={dropping.stuck || undefined}
      apps={dropping.apps}
      appPath={dropping.appPath}
      avatarOf={model.avatarOf}
      onClose={() => setDropping(null)}
      onDone={() => model.watch()}
    />
  );
  return { disconnect, node };
}

/**
 * One line of an activity feed: who, what, and when, newest first. With a
 * `mark` it leads with the app's or service's own mark, and with an `href` the
 * line is a link to where the activity belongs.
 */
export function FeedRow({
  who,
  what,
  at,
  now,
  mark,
  href,
}: {
  who: string;
  what: ReactNode;
  at: string;
  now: number;
  mark?: ReactNode;
  href?: string;
}) {
  const body = (
    <>
      <strong>{who}</strong> {what}
    </>
  );
  return (
    <li className={mark === undefined ? "int-feed-row" : "int-feed-row int-feed-row--marked"}>
      {mark !== undefined && (
        <span className="int-feed-mark" aria-hidden>
          {mark}
        </span>
      )}
      {href ? (
        <a className="int-feed-what" href={href}>
          {body}
        </a>
      ) : (
        <span className="int-feed-what">{body}</span>
      )}
      <span className="t-caption int-feed-at">{relTime(at, now)}</span>
    </li>
  );
}
