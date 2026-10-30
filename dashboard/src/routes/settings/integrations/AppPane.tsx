/**
 * One third-party app's detail: its header with the engine's word for its
 * state and four facts, then Agents, Activity and Settings, with "Check now"
 * at the row's far end on every tab. Its settings form opens in place of the
 * tabs, so connecting, finishing setup and replacing a key happen in the pane
 * with the directory still beside it.
 *
 * AGENTS FIRST. What an integration is for is which agents work in it and with
 * what access, so the Agents tab opens by default and carries what used to be
 * an overview of its own: the issues that need a person and the notes the
 * loop left.
 */

import { useMemo, useState } from "react";
import { Button, List, Tabs } from "@crewlethq/ui";
import { RotateCwGlyph } from "@crewlethq/icons/glyphs";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { VendorMark } from "@crewlethq/icons";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { asSentence } from "~/lib/format.ts";
import { SetupForm } from "../SetupForm.tsx";
import { SeatStep, SetupPasses, SurfaceDeliveries, integrationFacts } from "./catalogue.tsx";
import type { Intent, Model, Tool } from "./model.ts";
import {
  actionName,
  BeforeYouStart,
  BucketTag,
  DocsLink,
  IssueRow,
  SettingsSummary,
} from "./parts.tsx";
import { useAccess, useOperatorManaged } from "./access/api.ts";
import { AgentsAccess } from "./access/AgentsAccess.tsx";
import { accessHold, lanesOf, needsAttention, statusOf } from "./access/logic.ts";

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
      aria-label={actionName(label, tool.entry.name)}
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
  arrive,
}: {
  tool: Tool;
  model: Model;
  onDisconnect: () => void;
  /** Open the settings form on arrival, or the Agents tab: the landing and the home send a reader here. */
  arrive?: Intent | undefined;
}) {
  const now = useNow();
  // ONLY WHERE THE INTENT CAN BE TAKEN: an arrival asking to connect a tool
  // whose form cannot open lands on the page, which says why.
  const [open, setOpen] = useState<Intent | null>(
    arrive && arrive !== "agents" && !tool.blocked ? arrive : null,
  );
  const [tab, setTab] = useState("agents");
  const [checking, setChecking] = useState(false);
  const access = useAccess(tool.configured ? tool.entry.key : null);
  const data = access.data;
  const readOnly = useOperatorManaged(data);

  // WHAT THE AGENTS TAB SAYS, counted once for the header and the tab's marker.
  const counts = useMemo(() => {
    if (!data) return null;
    const lanes = lanesOf(data);
    let on = 0;
    let waiting = 0;
    for (const seat of data.seats) {
      if (lanes.some((l) => seat.lanes[l.key]?.desired.enabled)) on++;
      if (
        lanes.some((l) => {
          const saved = seat.lanes[l.key];
          return (
            saved !== undefined &&
            needsAttention(statusOf(l, saved, false, data.syncing, seat.departed))
          );
        })
      )
        waiting++;
    }
    return { on, of: data.seats.length, waiting };
  }, [data]);

  const facts = useMemo<Fact[] | undefined>(() => {
    if (!tool.configured) return undefined;
    return [
      ...(counts ? [{ label: "Agents on it", value: `${counts.on} of ${counts.of}` }] : []),
      ...integrationFacts(tool.entry, model.rows, now, {
        known: model.trafficKnown,
        since: model.trafficSince,
      }),
    ];
  }, [tool, model, now, counts]);

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
                variant="danger"
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

  // WHICH SURFACES A PASS CAN EVEN RUN AGAINST: the setup listing's kinds,
  // which the runs route is keyed on. `forge` is a surface and not a kind.
  const kinds = tool.entry.surfaces
    .filter((s) => tool.sections.some((x) => x.tool.key === s.key))
    .map((s) => s.key);
  const held = accessHold(model.managed, model.admin, readOnly);
  // THE SHARED-KEY NOTICE IS SAID ONCE: the access answer's, with its repair,
  // rather than again as the loop's finding.
  const issues = data?.notice
    ? tool.issues.filter((i) => i.kind !== "credential_shared")
    : tool.issues;
  const steps = new Map(
    tool.seats.filter((r) => r.seat?.step && r.toolKey).map((r) => [r.handle, r]),
  );
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
            value: "agents",
            label: "Agents",
            count: counts?.of ?? tool.seats.length,
            ...(tool.issues.length > 0 || (counts?.waiting ?? 0) > 0 || data?.notice
              ? { marker: { tone: "warning" as const, label: "Needs attention" } }
              : {}),
          },
          { value: "activity", label: "Activity" },
          { value: "settings", label: "Settings" },
        ]}
        actions={
          // ONLY WHERE A PASS RUNS: a guarded reader has no loop to ask.
          model.guarded ? undefined : (
            <Button
              size="small"
              variant="ghost"
              leadingIcon={<RotateCwGlyph size="sm" />}
              loading={checking}
              disabledReason={model.managed ?? (data?.syncing ? "A sync is running." : undefined)}
              onClick={async () => {
                if (model.managed) return;
                setChecking(true);
                try {
                  await model.check(tool);
                } finally {
                  void access.reload({ quiet: true });
                  setChecking(false);
                }
              }}
            >
              Check now
            </Button>
          )
        }
      />
      {tab === "agents" && (
        <div className="int-tab">
          {issues.length > 0 && (
            <div className="int-issues">
              {issues.map((issue) => (
                <IssueRow key={issue.key} tool={tool} issue={issue} onFix={setOpen} />
              ))}
            </div>
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
          <QueryState
            error={access.code}
            refusal={access.refusal}
            loading={access.loading && !data}
          >
            {data && (
              <AgentsAccess
                kind={tool.entry.key}
                name={tool.entry.name}
                data={data}
                reload={() => void access.reload({ quiet: true })}
                held={held}
                readOnly={readOnly}
                avatarOf={model.avatarOf}
                onReplaceKey={() => setOpen("rotate")}
                stepFor={(handle) => {
                  const row = steps.get(handle);
                  return row?.seat && row.toolKey && !model.guarded ? (
                    <SeatStep
                      app={tool.entry.name}
                      toolKey={row.toolKey}
                      seat={row.seat}
                      managed={model.managed}
                    />
                  ) : null;
                }}
              />
            )}
          </QueryState>
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
          <Button
            size="small"
            variant="secondary"
            disabledReason={tool.blocked}
            onClick={() => !tool.blocked && setOpen("settings")}
          >
            Edit settings
          </Button>
          <DocsLink tool={tool} />
        </div>
      )}
    </>
  );
}
