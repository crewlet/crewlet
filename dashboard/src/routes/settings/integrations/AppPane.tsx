/**
 * One third-party app's detail: its header with the engine's word for its
 * state and four facts, then Agents, Activity and Settings, with "Check now"
 * at the row's far end on every tab. Its settings form opens in place inside
 * the Settings tab, under the same header and tabs; each tab and the form has
 * its own address (`address.ts`).
 *
 * WHAT THE BARE ADDRESS SHOWS is decided by the setup: the form while the
 * connection is unfinished, Agents once it works, and before anything is set
 * the app's landing with Connect. What an integration is for is which agents
 * work in it, so the Agents tab carries the issues that need a person above
 * the agents and the loop's notes below them; while the connection is
 * unfinished it leads with the setup's next step instead.
 *
 * A SYNC THAT ENDS HAS BEEN CHECKED. The engine follows every access sync (a
 * save, an action at the vendor, a keep) with the same read-only pass Check
 * now runs, inside the sync's own window, so the moment the tab stops reading
 * as syncing the integration's rows are read again: its issues, notes and
 * header say what the change left without anybody pressing Check now.
 */

import { GrowthProductSection, growthDetailsOf } from "~/components/growth/GrowthProduct.tsx";
import { GROWTH_PRODUCTS } from "~/components/growth/words.ts";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button, Callout, HeadingLevelProvider, Tabs } from "@crewlethq/ui";
import { RotateCwGlyph } from "@crewlethq/icons/glyphs";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { QueryState } from "~/components/common.tsx";
import { SetupForm, heldReason } from "../SetupForm.tsx";
import { asSentence, listed } from "~/lib/format.ts";
import { href, useNavigator, useRoute } from "~/app/router.tsx";
import type { IntegrationTab } from "~/app/routes.ts";
import { GOOGLE, formOf, isGoogleProduct, tabOf, type IntegrationForm } from "./address.ts";
import { googlePlace, sharedGoogleIssue } from "./google.ts";
import { GoogleAction, GoogleRelated, GoogleStepCallout } from "./GoogleProduct.tsx";
import { WATCH_MS } from "../recheck.ts";
import {
  SetupPasses,
  SurfaceDeliveries,
  appHeading,
  integrationFacts,
  seatStepFor,
} from "./catalogue.tsx";
import { connectionUnfinished, type Intent, type Model, type Tool } from "./model.ts";
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
import { SshServers } from "./SshServers.tsx";
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
    <Callout
      variant="neutral"
      title={`Crewlet runs in ${tool.entry.name}'s place here`}
      action={
        <a className="t-link" href={href(["settings", "config"])}>
          Open Configuration
        </a>
      }
    >
      {tool.held.map((h) => heldReason(h.name, h.heldBy)).join(" ")} To use{" "}
      {listed(tool.held.map((h) => h.name))} instead, change it in Configuration.
    </Callout>
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
  const route = useRoute();
  const nav = useNavigator();
  const askedTab = tabOf(route.query.get("tab"));
  const askedForm = formOf(route.query.get("edit"));
  // A GOOGLE PRODUCT'S SHARED SETUP IS THE GOOGLE PAGE'S: its connection,
  // reader and sign-in are made there, so it opens no form of its own.
  const place = useMemo(
    () => (isGoogleProduct(tool.entry.key) ? googlePlace(model) : null),
    [tool.entry.key, model],
  );
  const googleRow = place?.products.find((p) => p.kind === tool.entry.key);
  useEffect(() => {
    if (googleRow && askedForm)
      nav.replace(["company", "integrations", GOOGLE], {
        ...(googleRow.switchable ? {} : { edit: "settings" }),
        product: googleRow.kind,
      });
  }, [googleRow, askedForm, nav]);
  const unfinished = connectionUnfinished(tool) && !googleRow;
  // ONLY WHERE THE FORM CAN OPEN: one a document holds (a block, a managed
  // document but for Replace key) lands on the page, which says why.
  const holds = (form: IntegrationForm) =>
    tool.blocked ?? (form === "key" ? undefined : (model.managed ?? undefined));
  const defaultForm = !askedTab && !askedForm && unfinished && !holds("settings");
  const open: IntegrationForm | null = googleRow
    ? null
    : askedForm && !holds(askedForm)
      ? askedForm
      : defaultForm
        ? "settings"
        : null;
  const tab: IntegrationTab = open ? "settings" : (askedTab ?? "agents");
  // THE DEFAULT IS WRITTEN INTO THE ADDRESS, so the form stays while the
  // reading moves under it, and a reload lands on it.
  useEffect(() => {
    if (defaultForm) nav.replace(route.path, { edit: "settings" });
  }, [defaultForm, nav, route.path]);
  const setTab = (next: string) => nav.to(route.path, { tab: next });
  const setOpen = (intent: Intent) =>
    intent === "agents"
      ? setTab("agents")
      : nav.replace(route.path, { edit: intent === "rotate" ? "key" : "settings" });
  // A SAVE LANDS WHERE THE SETUP NOW STANDS (the bare address's default);
  // Cancel on the Settings tab's read view, or the landing before a connect.
  const saved = useRef(false);
  const closeForm = () => {
    const back: Record<string, string> =
      saved.current || !tool.configured ? {} : { tab: "settings" };
    saved.current = false;
    nav.replace(route.path, back);
  };
  const landed = useRef<(() => void) | null>(null);
  // WHAT THE DOCUMENT HOLDS, which every save that lands changes.
  const standing = JSON.stringify(
    tool.sections.map((s) => [
      s.tool.configured,
      s.tool.satisfied,
      s.tool.requirements.map((r) => [r.present, r.value ?? ""]),
    ]),
  );
  useEffect(() => {
    if (tool.configured) landed.current?.();
  }, [tool.configured, standing]);
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
  const replaceKey = useCallback(() => nav.replace(route.path, { edit: "key" }), [nav, route.path]);
  const { guarded, managed } = model;
  const stepFor = useMemo(() => seatStepFor(tool, { guarded, managed }), [tool, guarded, managed]);
  const data = access.data;
  const readOnly = useOperatorManaged(data);
  // WHY A GROWTH PRODUCT'S ENTRIES SHOW BY ID (its connections' list unread),
  // said once over the agents and in the chooser, where the IDs are.
  const row = model.rows.get(tool.entry.key);
  const scopeNote = useMemo(() => {
    const unread = growthDetailsOf(row).connections.flatMap((c) => (c.unread ? [c.unread] : []));
    return unread.length > 0 ? [...new Set(unread)].map(asSentence).join(" ") : undefined;
  }, [row]);

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

  // A GOOGLE PRODUCT THAT IS OFF reads as one never connected: its landing, and Turn on.
  const off = googleRow ? !googleRow.enabled : !tool.configured;
  const facts = useMemo<Fact[] | undefined>(() => {
    if (off) return undefined;
    return [
      ...(counts ? [{ label: "Agents on it", value: `${counts.on} of ${counts.of}` }] : []),
      ...integrationFacts(tool.entry, model.rows, {
        known: model.trafficKnown,
        since: model.trafficSince,
      }),
      ...(googleRow
        ? [
            {
              label: "Connection",
              value: "Google",
              path: ["company", "integrations", GOOGLE],
              query: { product: googleRow.kind },
            },
          ]
        : []),
    ];
  }, [off, tool, model, counts, googleRow]);

  const header = (
    <ObjectHeader
      {...appHeading(tool.entry, 36)}
      status={off ? undefined : <BucketTag tool={tool} />}
      description={tool.entry.description}
      facts={facts}
      actions={
        <>
          {googleRow && place ? (
            <GoogleAction row={googleRow} place={place} model={model} />
          ) : (
            !open && <PrimaryAction tool={tool} model={model} onOpen={setOpen} />
          )}
          {/* A GOOGLE PRODUCT IS TURNED OFF ON THE GOOGLE PAGE, and Google Cloud
              disconnects with Google itself. */}
          {tool.configured && !googleRow && (
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
      }
    />
  );

  // A CONNECT HAS LANDED WHEN THIS PAGE READS IT. The form waits on this, so
  // the page never redraws its "not connected" view from a reading older than
  // the write; a save over a connected tool changes nothing the page draws from.
  function afterWrite(): Promise<void> | undefined {
    saved.current = true;
    model.watch();
    // AN UNFINISHED CONNECTION WAITS FOR ITS SAVE TO BE READ, since where the
    // page lands after it (the form again, or Agents) is decided from that.
    if (tool.configured && !unfinished) return undefined;
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

  const rotateIssue = tool.issues.find((i) => i.fix?.intent === "rotate");
  const form = open && (
    <SetupForm
      sections={tool.sections}
      title={tool.entry.name}
      stepsOpen={stepsOpen}
      onStepsOpen={setStepsOpen}
      rotate={open === "key" ? (rotateIssue?.problem ?? "") : undefined}
      onClose={closeForm}
      onDone={afterWrite}
    />
  );

  if (off) {
    return (
      <>
        {header}
        {form ||
          (tool.held.length > 0 ? <HeldNotice tool={tool} /> : <BeforeYouStart tool={tool} />)}
        {googleRow && <GoogleRelated row={googleRow} />}
        {!form && <DocsLink tool={tool} />}
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
  // AND A GOOGLE STEP IS SAID ONCE, by the callout that links to Google.
  const issues = tool.issues.filter(
    (i) =>
      !i.seat &&
      !(data?.notice && i.kind === "credential_shared") &&
      !(googleRow && sharedGoogleIssue(i, googleRow)),
  );
  const waitsOnGoogle = googleRow?.state === "waits";
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
        {/* A TAB'S SECTIONS HEAD ONE LEVEL UNDER THE PAGE'S TITLE. */}
        <HeadingLevelProvider level={3}>
          {tab === "agents" && (
            <div className="int-tab">
              {/* AN UNFINISHED CONNECTION LEADS WITH ITS NEXT STEP: no agent
                can be given access through it yet. */}
              {unfinished && (
                <Callout
                  variant="warning"
                  title={`Finish setting up ${tool.entry.name}`}
                  action={
                    <Button
                      size="small"
                      variant="primary"
                      disabledReason={holds("settings")}
                      onClick={() => !holds("settings") && setOpen("continue")}
                    >
                      Finish setup
                    </Button>
                  }
                >
                  Agents get access here once {tool.entry.name} is connected.
                </Callout>
              )}
              {googleRow && place && <GoogleStepCallout row={googleRow} place={place} />}
              {issues.length > 0 && (
                <div className="int-issues">
                  {issues.map((issue) => (
                    <IssueRow key={issue.key} tool={tool} issue={issue} onFix={setOpen} />
                  ))}
                </div>
              )}
              {!unfinished && !waitsOnGoogle && (
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
                      scopeNote={scopeNote}
                    />
                  )}
                </QueryState>
              )}
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
              {form || (
                <>
                  {/* A GOOGLE PRODUCT KEEPS ITS OWN SECTIONS; its shared settings are Google's. */}
                  {!googleRow && (
                    <>
                      <SettingsSummary tool={tool} />
                      {/* EDIT SETTINGS EDITS THE SUMMARY ABOVE IT, so it sits under
                        it and never under a section it does not edit (SSH's
                        servers, a growth product's connections). */}
                      <Button
                        size="small"
                        variant="secondary"
                        disabledReason={tool.blocked}
                        onClick={() => !tool.blocked && setOpen("settings")}
                      >
                        Edit settings
                      </Button>
                    </>
                  )}
                  {/* THE SERVERS, and each one's Atlas inventory: SSH's own answer. */}
                  {tool.entry.key === "ssh" && <SshServers managed={model.managed} />}
                  {/* A GROWTH PRODUCT'S connections, the vendor's roles, and a spending product's caps. */}
                  {GROWTH_PRODUCTS.has(tool.entry.key) && (
                    <GrowthProductSection
                      kind={tool.entry.key}
                      product={tool.entry.name}
                      row={model.rows.get(tool.entry.key)}
                    />
                  )}
                  {googleRow && <GoogleRelated row={googleRow} />}
                  <DocsLink tool={tool} />
                </>
              )}
            </div>
          )}
        </HeadingLevelProvider>
      </div>
    </>
  );
}
