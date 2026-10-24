/**
 * One third-party app's detail: its header with the engine's word for its
 * state and its counts, then Overview, Agents, Activity and Settings. Its
 * settings form opens in place of the tabs, so connecting, finishing setup and
 * replacing a key happen in the pane with the directory still beside it.
 */

import { useMemo, useState } from "react";
import { Button, List, StatusDot, Tabs } from "@crewlethq/ui";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { VendorMark } from "@crewlethq/icons";
import { useNow } from "~/lib/clock.ts";
import { asSentence, plural } from "~/lib/format.ts";
import { SetupForm } from "../SetupForm.tsx";
import { SetupPasses, SurfaceDeliveries, integrationFacts } from "./catalogue.tsx";
import { workingLine, type Intent, type Model, type Tool } from "./model.ts";
import {
  AgentList,
  BeforeYouStart,
  BucketTag,
  DocsLink,
  IssueRow,
  SettingsSummary,
} from "./parts.tsx";

/** The tool's one primary action, worded for what it opens. */
function PrimaryAction({
  tool,
  model,
  onOpen,
}: {
  tool: Tool;
  model: Model;
  onOpen: (intent: Intent) => void;
}) {
  if (tool.action.kind === "manage" || tool.action.kind === "learn") return null;
  const intent: Intent =
    tool.action.kind === "rotate"
      ? "rotate"
      : tool.action.kind === "continue"
        ? "continue"
        : "connect";
  // A ROTATION SEALS A VALUE THE DOCUMENT ALREADY NAMES and changes nothing in
  // it, so a managed document holds Connect and Continue and never Replace key.
  const held = tool.blocked ?? (intent === "rotate" ? undefined : (model.managed ?? undefined));
  const label =
    intent === "rotate" ? "Replace key" : intent === "continue" ? "Finish setup" : "Connect";
  return (
    <Button
      size="small"
      variant="primary"
      aria-label={`${label} ${tool.entry.name}`}
      disabledReason={held}
      onClick={() => {
        if (!held) onOpen(intent);
      }}
    >
      {label}
    </Button>
  );
}

export function AppPane({
  tool,
  model,
  onDisconnect,
}: {
  tool: Tool;
  model: Model;
  onDisconnect: () => void;
}) {
  const now = useNow();
  const [open, setOpen] = useState<Intent | null>(null);
  const [tab, setTab] = useState("overview");
  const facts = useMemo<Fact[] | undefined>(() => {
    if (!tool.configured) return undefined;
    const ready = tool.seats.filter((s) => s.status === "ready").length;
    return [
      ...(tool.seats.length > 0
        ? [{ label: "Agents ready", value: `${ready} of ${tool.seats.length}` }]
        : []),
      ...integrationFacts(tool.entry, model.rows, now, {
        known: model.trafficKnown,
        since: model.trafficSince,
      }),
    ];
  }, [tool, model, now]);

  const header = (
    <ObjectHeader
      kind="Integration"
      icon="plug"
      identifier={tool.entry.key}
      mark={<VendorMark vendor={tool.entry.vendor} size={36} />}
      title={tool.entry.name}
      status={<BucketTag tool={tool} />}
      description={tool.entry.description}
      facts={open ? undefined : facts}
      actions={
        open ? undefined : (
          <>
            <PrimaryAction tool={tool} model={model} onOpen={setOpen} />
            {tool.configured && (
              <Button
                size="small"
                variant="secondary"
                disabledReason={tool.blocked}
                onClick={() => !tool.blocked && setOpen("settings")}
              >
                Settings
              </Button>
            )}
            {tool.configured && (
              <Button
                size="small"
                variant="ghost"
                disabledReason={model.managed ?? undefined}
                onClick={() => {
                  if (!model.managed) onDisconnect();
                }}
              >
                Disconnect
              </Button>
            )}
          </>
        )
      }
    />
  );

  if (open) {
    const rotateIssue = tool.issues.find((i) => i.fix?.intent === "rotate");
    return (
      <>
        {header}
        <SetupForm
          sections={tool.sections}
          title={tool.entry.name}
          rotate={open === "rotate" ? (rotateIssue?.problem ?? "") : undefined}
          onClose={() => setOpen(null)}
          onDone={() => model.watch()}
        />
      </>
    );
  }

  if (!tool.configured) {
    return (
      <>
        {header}
        <BeforeYouStart tool={tool} />
        <DocsLink tool={tool} />
      </>
    );
  }

  const waiting = tool.seats.filter((s) => s.status !== "ready").length;
  // WHICH SURFACES A PASS CAN EVEN RUN AGAINST: the setup listing's kinds,
  // which the runs route is keyed on. `forge` is a surface and not a kind.
  const kinds = tool.entry.surfaces
    .filter((s) => tool.sections.some((x) => x.tool.key === s.key))
    .map((s) => s.key);
  return (
    <>
      {header}
      <Tabs
        variant="underline"
        ariaLabel={`${tool.entry.name} sections`}
        value={tab}
        onValueChange={setTab}
        items={[
          {
            value: "overview",
            label: "Overview",
            ...(tool.issues.length > 0
              ? { marker: { tone: "warning" as const, label: "Needs attention" } }
              : {}),
          },
          { value: "agents", label: "Agents", count: tool.seats.length },
          { value: "activity", label: "Activity" },
          { value: "settings", label: "Settings" },
        ]}
      />
      {tab === "overview" && (
        <div className="int-tab">
          {tool.issues.length > 0 ? (
            <div className="int-issues">
              {tool.issues.map((issue) => (
                <IssueRow key={issue.key} tool={tool} issue={issue} onFix={setOpen} />
              ))}
            </div>
          ) : (
            <StatusDot tone="success">{workingLine(tool)}</StatusDot>
          )}
          {/* ADVISORIES ARE SAID, never raised: the tool works, and what the
              loop noticed (an access wider than asked) is worth reading once. */}
          {tool.notes.length > 0 && (
            <section aria-label="Notes">
              <h3 className="t-label">Notes</h3>
              <List variant="plain">
                {tool.notes.map((n) => (
                  <List.Item key={`${n.kind}:${n.subject ?? ""}`}>
                    {asSentence(n.detail || n.kind.replace(/_/g, " "))}
                  </List.Item>
                ))}
              </List>
            </section>
          )}
          {waiting > 0 && (
            <Button size="small" variant="secondary" onClick={() => setTab("agents")}>
              {waiting === 1
                ? "One agent is not ready"
                : `${plural(waiting, "agent")} are not ready`}
            </Button>
          )}
        </div>
      )}
      {tab === "agents" && (
        <div className="int-tab">
          <AgentList tool={tool} model={model} />
        </div>
      )}
      {tab === "activity" && (
        <div className="int-tab">
          {kinds.length > 0 && (
            <SetupPasses key={`passes:${tool.entry.key}`} entry={tool.entry} kinds={kinds} />
          )}
          {/* `forge` is a relay rather than a source: its events are filed
              under the product they belong to. */}
          {tool.entry.surfaces
            .filter((s) => s.key !== "forge")
            .map((s) => (
              <SurfaceDeliveries key={s.key} surface={s.key} name={s.name} />
            ))}
        </div>
      )}
      {tab === "settings" && (
        <div className="int-tab">
          <SettingsSummary tool={tool} />
          <DocsLink tool={tool} />
        </div>
      )}
    </>
  );
}
