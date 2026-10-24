/**
 * The parts a detail pane on Company › Integrations is composed from: the
 * issues with their fixes, an agent's line, the settings an app holds, what to
 * have ready before connecting, and the engine's address at the foot of the
 * directory.
 */

import { useCallback, useState } from "react";
import {
  Button,
  ButtonLink,
  Copyable,
  DescriptionList,
  EmptyValue,
  List,
  StatusDot,
  Tag,
} from "@crewlethq/ui";
import { ProductMark, VendorMark } from "@crewlethq/icons";
import { CheckGlyph, RotateCwGlyph } from "@crewlethq/icons/glyphs";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { DisconnectDialog } from "../DisconnectDialog.tsx";
import { FindingSubjects, SeatStep, docHref } from "./catalogue.tsx";
import {
  BUCKET_WORDS,
  OWNER_WORDS,
  SEAT_WORDS,
  type BuiltInService,
  type DropPlan,
  type Intent,
  type Issue,
  type Model,
  type SeatRow,
  type Tool,
} from "./model.ts";

/** A built-in service's own mark. */
export function ServiceMark({ service, size }: { service: BuiltInService; size: "md" | number }) {
  return <ProductMark product={service.key} size={size} soon={service.soon} />;
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

/** The state pill a built-in service carries everywhere it is named. */
export function ServiceState({ service }: { service: BuiltInService }) {
  // NOTHING UNTIL THE ENGINE HAS SAID: Off on a node running no company would
  // be a guess.
  if (!service.known) return null;
  // SOON IS A FACT, CARRIED BY ITS WORD: the accent means where the reader
  // is, so it is not spent on a service that has not shipped.
  if (service.soon) {
    return (
      <Tag size="sm" variant="neutral">
        Soon
      </Tag>
    );
  }
  return service.on ? (
    <Tag size="sm" variant="success">
      On
    </Tag>
  ) : (
    <Tag size="sm" variant="neutral" appearance="outline">
      Off
    </Tag>
  );
}

/** One issue, with who owes the next move and the control that makes it. */
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
  /** Send the reader to the app's page instead of fixing it here. */
  go?: string;
}) {
  return (
    <div className="int-issue">
      {showTool && (
        <span className="int-issue-mark" aria-hidden>
          <VendorMark vendor={tool.entry.vendor} size="md" />
        </span>
      )}
      <div className="int-issue-text">
        <span className="int-issue-problem">
          {showTool && <strong>{tool.entry.name}: </strong>}
          {issue.problem}
        </span>
        <FindingSubjects subjects={issue.subjects} />
        {issue.remedy && <span className="t-caption">{issue.remedy}</span>}
        <span className="int-issue-owner">
          <StatusDot
            tone={issue.tone === "danger" ? "danger" : issue.owner === "you" ? "warning" : "info"}
          >
            {issue.owner === "you" ? "Waiting on you" : `${OWNER_WORDS[issue.owner]} is on it`}
          </StatusDot>
        </span>
      </div>
      <span className="int-issue-act">
        {go ? (
          <ButtonLink size="small" variant="secondary" href={go}>
            Open {tool.entry.name}
          </ButtonLink>
        ) : issue.fix && onFix ? (
          <Button
            size="small"
            variant={issue.owner === "you" ? "primary" : "secondary"}
            disabledReason={tool.blocked}
            onClick={() => {
              if (!tool.blocked && issue.fix) onFix?.(issue.fix.intent);
            }}
          >
            {issue.fix.label}
          </Button>
        ) : issue.external ? (
          <ButtonLink size="small" variant="secondary" external href={issue.external.href}>
            {issue.external.label}
          </ButtonLink>
        ) : null}
      </span>
    </div>
  );
}

/** One agent on one tool: its mark, its standing, and its own step at the app. */
export function SeatLine({ row, tool, model }: { row: SeatRow; tool: Tool; model: Model }) {
  const words = SEAT_WORDS[row.status];
  return (
    <div className="int-seat">
      <SeatAvatar name={row.name} kind="agent" avatar={row.avatar} size="sm" decorative />
      <div className="int-seat-id">
        <span className="int-seat-name">
          {row.name}
          {row.tier && (
            // THE ENGINE'S OWN WORDS, weighted by trust only on a scale it
            // names: a role an organization made carries no tier id, and
            // drawing it in the accent would claim what it grants.
            <span
              className={
                row.tier.id ? `int-seat-tier int-seat-tier--${row.tier.id}` : "int-seat-tier"
              }
              title={row.tier.hint}
            >
              {row.tier.label}
            </span>
          )}
        </span>
        <span className="t-caption">{row.detail}</span>
      </div>
      <Tag
        size="sm"
        variant={words.tone}
        appearance={row.status === "missing" ? "outline" : "soft"}
      >
        {words.label}
      </Tag>
      {row.seat && row.toolKey && !model.guarded ? (
        <span className="int-seat-act">
          <SeatStep
            app={tool.entry.name}
            toolKey={row.toolKey}
            seat={row.seat}
            managed={model.managed}
          />
        </span>
      ) : null}
    </div>
  );
}

/** Every agent on one tool, and the control that asks the loop to look again. */
export function AgentList({ tool, model }: { tool: Tool; model: Model }) {
  const [checking, setChecking] = useState(false);
  const [said, setSaid] = useState("");
  const provisioned = tool.sections.some((s) => s.tool.can_provision);
  const waiting = tool.seats.filter((s) => s.status === "pending").length;
  return (
    <div className="int-agents">
      <p className="t-caption">
        {tool.story.identity}
        {provisioned ? " Crewlet makes each account on its next pass." : ""}
      </p>
      {tool.seats.length > 0 ? (
        <div className="int-seats">
          {tool.seats.map((row) => (
            <SeatLine key={row.handle} row={row} tool={tool} model={model} />
          ))}
        </div>
      ) : (
        <p className="t-caption">No agent is on this app yet.</p>
      )}
      {/* ONLY WHERE A PASS RUNS: a tool whose accounts are made by hand has
          no loop to ask, so the control would only ever be refused. */}
      {!model.guarded && tool.configured && provisioned && (
        <div className="int-row-start">
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<RotateCwGlyph size="sm" />}
            disabled={checking}
            disabledReason={model.managed ?? undefined}
            onClick={async () => {
              if (model.managed) return;
              setChecking(true);
              setSaid((await model.check(tool)) || "Crewlet is looking now.");
              setChecking(false);
            }}
          >
            {checking ? "Checking" : "Check now"}
          </Button>
          <span className="t-caption">
            {said ||
              (waiting > 0
                ? `${waiting === 1 ? "One account is" : `${waiting} accounts are`} being made.`
                : "Made an app or account just now? Check picks it up.")}
          </span>
        </div>
      )}
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
