/**
 * One third-party app's detail: its header with the engine's word for its
 * state and four facts, then Agents, Activity and Settings, with "Check now"
 * at the row's far end on every tab. Its settings form opens in place of the
 * tabs, so connecting, finishing setup and replacing a key happen in the pane
 * with the directory still beside it.
 *
 * AGENTS FIRST. What an integration is for is which agents work in it and with
 * what access, so the Agents tab opens by default and carries what used to be
 * an overview of its own: the issues that need a person above the agents, and
 * the notes the loop left below them. Its label counts the agents that are on.
 *
 * A SYNC THAT ENDS HAS BEEN CHECKED. The engine follows every access sync (a
 * save, an action at the vendor, a keep) with the same read-only pass Check
 * now runs, inside the sync's own window, so the moment the tab stops reading
 * as syncing the integration's rows are read again: its issues, notes and
 * header say what the change left without anybody pressing Check now.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button, Callout, Tabs } from "@crewlethq/ui";
import { RotateCwGlyph } from "@crewlethq/icons/glyphs";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { VendorMark } from "@crewlethq/icons";
import { QueryState } from "~/components/common.tsx";
import { SetupForm, heldReason } from "../SetupForm.tsx";
import { listed } from "~/lib/format.ts";
import { href } from "~/app/router.tsx";
import { WATCH_MS } from "../recheck.ts";
import { SetupPasses, SurfaceDeliveries, integrationFacts, seatStepFor } from "./catalogue.tsx";
import type { Intent, Model, Tool } from "./model.ts";
import {
  actionName,
  BeforeYouStart,
  BucketTag,
  DocsLink,
  IssueRow,
  Notes,
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
  // NOTHING TO CONNECT where Crewlet's own services hold every product.
  if (tool.held.length > 0 && !tool.configured) return null;
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

/**
 * What holds each product of a tool whose every product a Crewlet service
 * holds, and where that is changed, in place of a form that could connect
 * nothing.
 */
function HeldNotice({ tool }: { tool: Tool }) {
  return (
    <Callout variant="neutral" title={`Crewlet runs in ${tool.entry.name}'s place here`}>
      <span className="col gap-1">
        {tool.held.map((h) => (
          <span key={h.name}>{heldReason(h.name, h.heldBy)}</span>
        ))}
        <span>
          To use {listed(tool.held.map((h) => h.name))} instead, change it in{" "}
          <a className="prose-link" href={href(["settings", "config"])}>
            Configuration
          </a>
          .
        </span>
      </span>
    </Callout>
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
  // ONLY WHERE THE INTENT CAN BE TAKEN: an arrival asking to connect a tool
  // whose form cannot open lands on the page, which says why.
  const [open, setOpen] = useState<Intent | null>(
    arrive && arrive !== "agents" && !tool.blocked ? arrive : null,
  );
  const [tab, setTab] = useState("agents");
  const landed = useRef<(() => void) | null>(null);
  useEffect(() => {
    if (tool.configured) landed.current?.();
  }, [tool.configured]);
  useEffect(() => () => landed.current?.(), []);
  // A PERSON'S CHOICE OF OPEN OR PUT AWAY for the form's Setup steps, held
  // here so it lasts the page view rather than one opening of the form.
  const [stepsOpen, setStepsOpen] = useState<boolean | null>(null);
  const [checking, setChecking] = useState(false);
  const [refused, setRefused] = useState("");
  const access = useAccess(tool.configured ? tool.entry.key : null);
  // STABLE PROPS FOR THE ACCESS TABLE, which is `memo`: a closure made per
  // render drew every row again on each re-read of an unchanged reading.
  const accessReload = access.reload;
  const reloadAccess = useCallback(() => void accessReload({ quiet: true }), [accessReload]);
  const replaceKey = useCallback(() => setOpen("rotate"), []);
  const { guarded, managed } = model;
  const stepFor = useMemo(() => seatStepFor(tool, { guarded, managed }), [tool, guarded, managed]);
  const data = access.data;
  const readOnly = useOperatorManaged(data);

  // THE ROWS ARE READ AGAIN when a sync ends, since the engine checked the
  // integration before it let the sync end.
  const watch = model.watch;
  const syncing = data?.syncing ?? false;
  const wasSyncing = useRef(syncing);
  useEffect(() => {
    if (wasSyncing.current && !syncing) watch();
    wasSyncing.current = syncing;
  }, [syncing, watch]);

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
          return saved !== undefined && needsAttention(statusOf(l, saved, false));
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
      ...integrationFacts(tool.entry, model.rows, {
        known: model.trafficKnown,
        since: model.trafficSince,
      }),
    ];
  }, [tool, model, counts]);

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

  // A CONNECT HAS LANDED WHEN THIS PAGE READS IT. The form waits on this, so
  // the page never redraws its "not connected" view from a reading older than
  // the write; a save over a connected tool changes nothing the page draws from.
  function afterWrite(): Promise<void> | undefined {
    model.watch();
    if (tool.configured) return undefined;
    return new Promise<void>((resolve) => {
      const timer = setTimeout(done, WATCH_MS);
      function done() {
        clearTimeout(timer);
        landed.current = null;
        resolve();
      }
      landed.current = done;
    });
  }

  if (open) {
    const rotateIssue = tool.issues.find((i) => i.fix?.intent === "rotate");
    // THE HEADER IN THE FORM'S COLUMN, so the Setup steps beside the form
    // start level with it.
    return (
      <SetupForm
        sections={tool.sections}
        title={tool.entry.name}
        head={header}
        stepsOpen={stepsOpen}
        onStepsOpen={setStepsOpen}
        rotate={open === "rotate" ? (rotateIssue?.problem ?? "") : undefined}
        onClose={() => setOpen(null)}
        onDone={afterWrite}
      />
    );
  }

  if (!tool.configured) {
    return (
      <>
        {header}
        {tool.held.length > 0 ? <HeldNotice tool={tool} /> : <BeforeYouStart tool={tool} />}
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
  // rather than again as the loop's finding. AND AN AGENT'S OWN WAIT IS SAID
  // ON ITS ROW, with the step's own control, so the list above the agents
  // holds what is wrong with the integration itself.
  const issues = tool.issues.filter(
    (i) => !i.seat && !(data?.notice && i.kind === "credential_shared"),
  );
  // ONLY WHERE A PASS RUNS: a tool whose accounts are made by hand has no
  // loop to ask, so the control would only ever be refused.
  const provisioned = tool.sections.some((s) => s.tool.can_provision);
  return (
    <>
      {header}
      {/* THE TAB BAR AND ITS PANEL, one step apart (`.tab-detail`). */}
      <div className="tab-detail">
        <Tabs
          variant="underline"
          ariaLabel={`${tool.entry.name} sections`}
          value={tab}
          onValueChange={setTab}
          items={[
            {
              value: "agents",
              label: "Agents",
              // THE AGENTS THAT ARE ON, said once the access answer has.
              ...(counts ? { count: counts.on } : {}),
              ...(tool.issues.length > 0 || (counts?.waiting ?? 0) > 0 || data?.notice
                ? { marker: { tone: "warning" as const, label: "Needs attention" } }
                : {}),
            },
            { value: "activity", label: "Activity" },
            { value: "settings", label: "Settings" },
          ]}
          actions={
            model.guarded || !provisioned ? undefined : (
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
                    // A REFUSAL IS SAID in the loop's own words; the button's
                    // own busy state is the rest of the answer.
                    setRefused(await model.check(tool));
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
        {refused && (
          <p className="t-caption" role="status">
            {refused}
          </p>
        )}
        {tab === "agents" && (
          <div className="int-tab">
            {issues.length > 0 && (
              <div className="int-issues">
                {issues.map((issue) => (
                  <IssueRow key={issue.key} tool={tool} issue={issue} onFix={setOpen} />
                ))}
              </div>
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
                  reload={reloadAccess}
                  held={held}
                  readOnly={readOnly}
                  avatarOf={model.avatarOf}
                  onAnswer={access.adopt}
                  onReplaceKey={replaceKey}
                  stepFor={stepFor}
                />
              )}
            </QueryState>
            {/* ADVISORIES ARE SAID, never raised, and under the agents: the
              tool works, and what the loop noticed is worth reading once. */}
            <Notes notes={tool.notes} />
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
      </div>
    </>
  );
}
