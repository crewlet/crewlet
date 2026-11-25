/**
 * The detail of a service built into Crewlet, laid out as a third-party
 * app's is (a header with its state and one line, the counts, and Agents,
 * Activity and Settings) and filled only with what the engine answers.
 *
 * WHAT EACH PART READS, and why it is honest:
 *
 *  - WHO WORKS IN IT. Every agent seat holds Worklet and Doclet
 *    (`iam.Apps`), so the Agents tab lists every agent rather than a roster
 *    of who connected. What it adds per agent is what the engine counts:
 *    open, blocked and overdue work and its load (`work_workload`), and the
 *    space an agent writes in and the pages it reads at onboarding (the
 *    company document and the derived chart).
 *  - THE COUNTS. Projects and their open tasks (`work_projects`), spaces and
 *    their pages (`containers`), and the newest change on each log
 *    (`work_activity`, `page_activity`), in the header. The projects and the
 *    spaces counted there link to the service's own list of them, which
 *    names each with its counts: an Overview tab that listed them again here
 *    was a second copy of that page. No delivery count, because a built-in
 *    service has no inbound edge to count.
 *  - THE SETTINGS. The engine's own rows of what the company document sets
 *    for each service, read only by an Admin: Worklet's inbox horizon
 *    (`limits`), Doclet's `knowledge` block (`settings`) and Chatlet's message
 *    horizon (`limits`), each beside its default and changed in place on the
 *    one write path (`components/SettingEditor.tsx`).
 *  - CHATLET COUNTS NOTHING: the rooms it answers for are the reader's own
 *    seat's, never the company's, so its page names no count and no roster,
 *    only what it gives and the way in, which is offered to a holder of Chatlet
 *    as Worklet and Doclet offer theirs to a holder of their apps.
 *  - GITLET COUNTS NOTHING EITHER, and for the same reason: which
 *    repositories a reader sees is their own grant's answer (the chart's
 *    owners and each repository's `visibility`), never the company's count,
 *    so its page says what it gives and the way in, offered to a holder of
 *    Gitlet, and an Admin's way to Gitlet's own settings, where the code
 *    log's retention and the rest of `code` are read.
 *
 * A service the company runs elsewhere says what replaces it and what
 * running it here would mean, with no control: switching is not offered yet.
 * Gitlet is never run elsewhere: GitHub and GitLab are integrations a seat
 * calls, never a host the engine runs in its place, so Gitlet off says that
 * Crewlet hosts none of the company's code rather than naming a replacement.
 */

import { useMemo, type ReactNode } from "react";
import {
  ButtonLink,
  Callout,
  EmptyValue,
  List,
  SettingsTable,
  StatusDot,
  Tabs,
  Tag,
  type DataGridColumn,
} from "@crewlethq/ui";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { CheckGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator, useRoute } from "~/app/router.tsx";
import type { IntegrationTab } from "~/app/routes.ts";
import { tabOf } from "./address.ts";
import { PROJECT_PAGE } from "~/contract/work.ts";
import { LimitSetting } from "~/components/LimitSetting.tsx";
import { UnreadSetting } from "~/components/SettingEditor.tsx";
import { TextSetting } from "~/components/TextSetting.tsx";
import type { SettingRow } from "~/contract/settings.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { documentUnits, handleLabel, indexOrg, seatPath, type Seat } from "~/lib/seats.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { useServicesAsk } from "~/lib/services.ts";
import { useNow } from "~/lib/clock.ts";
import { CHATLET, DOCLET, GITLET, WORKLET, needsWords } from "~/lib/access.ts";
import { fmtCount, plural, relTime } from "~/lib/format.ts";
import { workloadWords } from "~/lib/flow.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { CompanyDocument } from "~/protocol/types.ts";
import { FeedRow, ServiceMark, ServiceState } from "./parts.tsx";
import type { BuiltInService, Model, ReplacingApp } from "./model.ts";

/** What a service gives the company: what running it here would give, or what Chatlet gives. */
const GIVES: Record<BuiltInService["key"], string[]> = {
  worklet: [
    "Projects and tasks, on boards and lists",
    "An inbox and My work for every person",
    "Asks that wait on a person's decision",
  ],
  doclet: [
    "Spaces and pages, with every revision kept",
    "Search across pages and work, by words and meaning",
    "Skills and diaries the agents read before they act",
  ],
  chat: [
    "Talk with any agent from inside Crewlet",
    "Keep a conversation beside the work it is about",
    "Nothing to install: your agents are already here",
  ],
  gitlet: [
    "Repositories your people clone and push over HTTPS",
    "Changes reviewed by people and agents, with checks",
    "Code your agents read and change without a token",
  ],
};

/** Where a seat works and writes, from the company document. */
function homeOf(
  doc: CompanyDocument | null | undefined,
  handle: string,
): { project?: string; space?: string } {
  for (const role of doc?.roles ?? []) {
    if (role.handle === handle) {
      return {
        ...(role.project ? { project: role.project } : {}),
        ...(role.space ? { space: role.space } : {}),
      };
    }
  }
  for (const unit of documentUnits(doc)) {
    if ((unit.roles ?? []).some((r) => r.handle === handle)) {
      return {
        ...(unit.project ? { project: unit.project } : {}),
        ...(unit.space ? { space: unit.space } : {}),
      };
    }
  }
  return {};
}

/** What each service holds, as the off state's sentences name it. */
const HOLDS: Record<BuiltInService["key"], string> = {
  worklet: "tasks",
  doclet: "pages",
  chat: "conversations",
  gitlet: "repositories",
};

/** A replacing app's name on its own line and button. */
const APP_NAMES: Record<ReplacingApp, string> = {
  jira: "Jira",
  confluence: "Confluence",
  slack: "Slack",
  mattermost: "Mattermost",
};

/**
 * The tool on the directory that connects a replacing app: Jira and
 * Confluence are one Atlassian connection, and a chat vendor is its own.
 */
function connectionOf(app: ReplacingApp): "atlassian" | "slack" | "mattermost" {
  return app === "jira" || app === "confluence" ? "atlassian" : app;
}

/** What replaces a service the company does not run here. */
function Elsewhere({ service, model }: { service: BuiltInService; model: Model | null }) {
  const by = service.replacedBy;
  const what = HOLDS[service.key];
  const apps = by?.keys ?? [];
  // THE SENTENCE NAMES THE CONNECTION THE APP RIDES: Jira and Confluence
  // through the one Atlassian integration, a chat vendor as itself. A vendor
  // nobody named (the `viewer` answer tells the names to a holder of Chatlet) is
  // still a vendor's chat, said in words.
  // GITLET OFF NAMES NOTHING IN ITS PLACE (see the file's doc).
  if (service.key === "gitlet") return <GitletOff service={service} />;
  const how =
    service.key === "chat"
      ? `Your agents talk on ${by?.name}`
      : `Your agents work in ${by?.name} through the Atlassian integration`;
  return (
    <div className="int-service-off">
      <Callout
        variant="neutral"

        title={
          by
            ? `${capitalised(by.name)} ${apps.length > 1 ? "do" : "does"} this for your company`
            : "Nothing does this for your company"
        }
      >
        {by
          ? `${how}, so ${service.name} is not in use. Running both would leave two places for the same ${what}.`
          : `The company document leaves ${service.name} out and names nothing in its place.`}
      </Callout>
      <Gives service={service} would />
      <p className="t-caption">
        The company document decides which system holds the company&apos;s {what}, so this page
        offers no switch: moving them is a change to plan, not a toggle.
      </p>
      {apps.length > 0 && (
        <div className="int-row-start">
          {apps.map((app) => (
            <ButtonLink
              key={app}
              size="small"
              variant="secondary"
              href={href(["company", "integrations", app])}
            >
              Open {APP_NAMES[app]}
            </ButtonLink>
          ))}
          {[...new Set(apps.map(connectionOf))].map((key) => {
            const tool = model?.byKey.get(key);
            return tool && !tool.configured ? (
              <span key={key} className="t-caption">
                {tool.entry.name} is not connected.
              </span>
            ) : null;
          })}
        </div>
      )}
    </div>
  );
}

/**
 * What a service gives the company: what it gives where it is in use, or what
 * running it here `would` give where it is not.
 */
function Gives({ service, would }: { service: BuiltInService; would: boolean }) {
  return (
    <section aria-label={`What ${service.name} ${would ? "would give" : "gives"} you`}>
      <h3 className="t-label">{would ? "Running it here would give you" : "What it gives you"}</h3>
      <List variant="plain" className="int-checks">
        {GIVES[service.key].map((line) => (
          <List.Item key={line} leading={<CheckGlyph size="sm" aria-hidden />}>
            {line}
          </List.Item>
        ))}
      </List>
    </section>
  );
}

/**
 * Gitlet off: Crewlet hosts none of the company's code. NEVER "nothing does
 * this": a company on GitHub or GitLab keeps its code there, and its agents
 * reach it through those integrations, with Gitlet on or off.
 */
function GitletOff({ service }: { service: BuiltInService }) {
  return (
    <div className="int-service-off">
      <Callout variant="neutral" title="Crewlet hosts none of your company's code">
        The company document leaves {service.name} off, so its repositories live elsewhere, if
        anywhere. Your agents reach GitHub and GitLab through their own integrations, with{" "}
        {service.name} on or off.
      </Callout>
      <Gives service={service} would />
      <p className="t-caption">
        The company document decides where the company&apos;s {HOLDS[service.key]} are hosted, so
        this page offers no switch: moving them is a change to plan, not a toggle.
      </p>
    </div>
  );
}

/** A phrase as the start of a sentence: "a vendor's chat" is "A vendor's chat". */
function capitalised(phrase: string): string {
  return phrase.charAt(0).toUpperCase() + phrase.slice(1);
}

/** One agent on a service's Agents tab: what it does there, and where it stands. */
interface AgentRow {
  seat: Seat;
  detail: string;
  state: ReactNode;
  /** The state's place in a sort. */
  rank: number;
}

/**
 * A service's agents as the product's data grid: the agent (its name wrapping
 * over its handle, never cut), what it does in the service, and its state,
 * each row its agent's page. The agents are every seat, so the client sorts.
 */
function ServiceAgents({
  label,
  detail,
  state,
  rows,
}: {
  label: string;
  /** The detail column's head, in the service's words. */
  detail: string;
  /** The state column's head. */
  state: string;
  rows: AgentRow[];
}) {
  const columns: DataGridColumn<AgentRow>[] = [
    {
      key: "agent",
      header: "Agent",
      floor: "12rem",
      sortValue: (r) => r.seat.name.toLowerCase(),
      cell: (r) => (
        <span className="int-agent">
          <SeatAvatar name={r.seat.name} kind="agent" avatar={r.seat.avatar} size="sm" decorative />
          <span className="int-agent-name">
            <span>{r.seat.name}</span>
            <span className="t-caption mono">{handleLabel(r.seat.handle)}</span>
          </span>
        </span>
      ),
    },
    {
      key: "detail",
      header: detail,
      label: detail,
      overflow: "wrap",
      drop: 1,
      cell: (r) => <span className="t-caption">{r.detail}</span>,
    },
    {
      key: "state",
      header: state,
      label: state,
      // ITS WHOLE WORDS: the state is the column a reader scans, so the detail
      // beside it gives way first rather than this being cut at a fifth.
      width: "max-content",
      sortValue: (r) => r.rank,
      cell: (r) => r.state,
    },
  ];
  return (
    <DataGrid<AgentRow>
      name="agents"
      aria-label={label}
      rows={rows}
      rowKey={(r) => r.seat.handle}
      rowHref={(r) => href(seatPath(r.seat))}
      columns={columns}
    />
  );
}

const VERBS: Record<string, string> = {
  created: "created",
  updated: "updated",
  commented: "commented on",
  assigned: "assigned",
  moved: "moved",
  closed: "closed",
  reopened: "reopened",
  removed: "removed",
  restored: "restored",
  asked: "asked on",
  answered: "answered on",
  edited: "edited",
  renamed: "renamed",
  page_created: "created",
  page_edited: "edited",
};

function verb(kind: string): string {
  return VERBS[kind] ?? kind.replace(/_/g, " ");
}

type Tab = IntegrationTab;

/** The tab the address names (`tab=`, `address.ts`), Agents where it names none. */
function useTab(): [Tab, (t: string) => void] {
  const route = useRoute();
  const nav = useNavigator();
  return [tabOf(route.query.get("tab")) ?? "agents", (t) => nav.to(route.path, { tab: t })];
}

function ServiceTabs({
  name,
  tab,
  onTab,
  agents,
}: {
  name: string;
  tab: Tab;
  onTab: (t: string) => void;
  agents: number;
}) {
  return (
    <Tabs
      variant="underline"
      ariaLabel={`${name} sections`}
      value={tab}
      onValueChange={onTab}
      items={[
        { value: "agents", label: "Agents", count: agents },
        { value: "activity", label: "Activity" },
        { value: "settings", label: "Settings" },
      ]}
    />
  );
}

export function ServicePane({ service, model }: { service: BuiltInService; model: Model | null }) {
  const head: Head = (line, actions, facts) => (
    <ObjectHeader
      kind="Built into Crewlet"
      icon="plug"
      mark={<ServiceMark service={service} size={36} />}
      title={service.name}
      status={<ServiceState service={service} />}
      description={line}
      facts={facts}
      actions={actions}
    />
  );
  if (!service.known) return <div className="int-detail">{head(service.sentence)}</div>;
  if (!service.on) {
    return (
      <div className="int-detail">
        {head(service.sentence)}
        <Elsewhere service={service} model={model} />
      </div>
    );
  }
  if (service.key === "chat") return <ChatPane service={service} head={head} />;
  if (service.key === "gitlet") return <GitletPane service={service} head={head} />;
  return service.key === "worklet" ? (
    <WorkletPane service={service} head={head} />
  ) : (
    <DocletPane service={service} head={head} />
  );
}

type Head = (line: ReactNode, actions?: ReactNode, facts?: Fact[]) => ReactNode;

function WorkletPane({ service, head }: { service: BuiltInService; head: Head }) {
  const viewer = useViewer();
  const ask = useServicesAsk();
  const enabled = ask.tracker && viewer.worklet;
  const now = useNow();
  const org = useOrg();
  const agents = useMemo(() => indexOrg(org).seats.filter((s) => s.kind === "agent"), [org]);
  const projects = useQuery("work_projects", { limit: PROJECT_PAGE }, { enabled, pollMs: 120_000 });
  const workload = useQuery("work_workload", undefined, { enabled, pollMs: 60_000 });
  const feed = useQuery(
    "work_activity",
    { container: "workspace", limit: 20 },
    { enabled, pollMs: 60_000 },
  );
  const config = useQuery("config", undefined, { enabled: viewer.admin });
  const [tab, setTab] = useTab();

  const rows = projects.data?.projects ?? [];
  const open = projects.data
    ? rows.reduce((n, p) => n + p.task_counts.todo + p.task_counts.active, 0)
    : null;
  const load = new Map((workload.data?.rows ?? []).map((r) => [r.handle, r]));
  // WORK THAT WANTS THE AGENT NOW (ADR-0162): waiting, blocked or held work is open and no load.
  const busy = agents.filter((a) => (load.get(a.handle)?.load ?? 0) > 0).length;
  const records = feed.data?.records ?? [];
  const latest = records[0]?.at;
  const keys = feed.data?.keys ?? {};
  const nameOf = (handle: string | undefined) =>
    agents.find((a) => a.handle === handle)?.name ?? handle ?? "Someone";
  // THE COMPANY'S HORIZON, at the top of its document (`inbox.retention_days`)
  // and not under `tracker`: a person's inbox is every native domain's
  // notices. The engine's own row (`limits`, group `inbox`) carries the value
  // in force beside its default, and the field that changes it.
  const limits = useQuery("limits", undefined, { enabled: viewer.admin });
  const horizon = limits.data?.limits.find((r) => r.key === "inbox.retention_days");

  if (!enabled) {
    return (
      <div className="int-detail">
        {head(service.sentence)}
        <Callout variant="neutral">
          {needsWords("Seeing its projects, tasks and who is working on what", [WORKLET])}
        </Callout>
      </div>
    );
  }

  return (
    <div className="int-detail">
      {/* THE LINE IS WHAT THE SERVICE IS; the counts are the facts under it,
          said once. */}
      {head(
        service.sentence,
        service.open && (
          <ButtonLink size="small" variant="primary" href={href(service.open.path)}>
            {service.open.label}
          </ButtonLink>
        ),
        [
          {
            label: "Projects",
            value: projects.data ? (
              fmtCount(projects.data.census.active)
            ) : (
              <EmptyValue label="Reading" />
            ),
            // WORKLET'S OWN LIST OF THEM, each with its open and done counts.
            ...(service.open ? { path: service.open.path } : {}),
          },
          {
            label: "Open tasks",
            value: open === null ? <EmptyValue label="Reading" /> : fmtCount(open),
          },
          {
            label: "Agents with work now",
            value: workload.data ? `${busy} of ${agents.length}` : <EmptyValue label="Reading" />,
          },
          {
            label: "Last change",
            value: latest ? relTime(latest, now) : <EmptyValue label="No change yet" />,
          },
        ],
      )}
      {/* THE TAB BAR AND ITS PANEL, one step apart (`.tab-detail`). */}
      <div className="tab-detail">
        <ServiceTabs name={service.name} tab={tab} onTab={setTab} agents={agents.length} />
        {tab === "agents" && (
          <div className="int-tab">
            <p className="t-caption">
              Every agent works in Worklet: it holds the Worklet app with its seat.
            </p>
            <ServiceAgents
              label={`Agents in ${service.name}`}
              detail="Work"
              state="Open"
              rows={agents.map((a) => {
                const row = load.get(a.handle);
                const home = homeOf(config.data, a.handle);
                const detail = [
                  home.project ? `Files work in ${home.project}` : "",
                  row
                    ? [
                        ...workloadWords(row),
                        `${row.blocked} blocked`,
                        `${row.overdue} overdue`,
                      ].join(" · ")
                    : "",
                ]
                  .filter(Boolean)
                  .join(" · ");
                const open = row?.open ?? 0;
                return {
                  seat: a,
                  detail,
                  rank: -open,
                  state: (
                    <Tag
                      size="sm"
                      variant={open > 0 ? "info" : "neutral"}
                      appearance={open > 0 ? "soft" : "outline"}
                    >
                      {open > 0 ? `${open} open` : "Nothing open"}
                    </Tag>
                  ),
                };
              })}
            />
          </div>
        )}
        {tab === "activity" && (
          <div className="int-tab">
            <ul className="int-feed">
              {records.map((r) => (
                <FeedRow
                  key={r.id}
                  who={nameOf(r.actor)}
                  what={
                    <>
                      {verb(r.kind)}{" "}
                      <span className="mono">
                        {r.subject_key ?? keys[r.subject_id] ?? "a task"}
                      </span>
                      {r.excerpt ? <span className="t-caption"> · {r.excerpt}</span> : null}
                    </>
                  }
                  at={r.at}
                  now={now}
                />
              ))}
              {feed.data && records.length === 0 && (
                <li className="t-caption">Nothing has changed yet.</li>
              )}
            </ul>
          </div>
        )}
        {tab === "settings" && (
          <div className="int-tab">
            <SettingsTable aria-label="Worklet settings">
              <SettingsTable.Row label="Work tracker" settingKey="tracker.backend">
                Worklet
              </SettingsTable.Row>
              {horizon ? (
                <LimitSetting
                  cap={horizon}
                  label="Inbox keeps a notice for"
                  area="Worklet"
                  reread="Reload to see the Worklet settings as they are now."
                  onApplied={limits.refetch}
                />
              ) : (
                <UnreadSetting label="Inbox keeps a notice for" settingKey="inbox.retention_days">
                  {viewer.admin ? <EmptyValue /> : <EmptyValue label="Admins see this" />}
                </UnreadSetting>
              )}
              <SettingsTable.Row label="Projects the chart declares">
                {viewer.admin ? (
                  // ONE PROJECT ONCE, however many units file under it.
                  [
                    ...new Set(
                      documentUnits(config.data)
                        .map((u) => u.project)
                        .filter(Boolean),
                    ),
                  ].join(", ") || <EmptyValue label="None" />
                ) : (
                  <EmptyValue label="Admins see this" />
                )}
              </SettingsTable.Row>
            </SettingsTable>
            <p className="t-caption">
              These live in the company document: the tracker under tracker, the inbox under inbox,
              and each project on its unit in Structure.
            </p>
          </div>
        )}
      </div>
    </div>
  );
}

/**
 * Doclet's four settings, each the engine's row (`settings`) changed in place:
 * what each is called here, the short line under its field, and how a value reads
 * where the plain words would mislead (an empty reach is every space).
 */
const DOCLET_SETTINGS: readonly {
  key: string;
  label: string;
  hint: string;
  words?: (value: SettingRow["value"]) => string;
}[] = [
  {
    key: "knowledge.root_space",
    label: "Company pages",
    hint: "Holds the Onboarding page every agent reads first.",
  },
  {
    key: "knowledge.skills_container",
    label: "Tool skills",
    hint: "Search leaves it out. Off turns tool skills off.",
  },
  {
    key: "knowledge.scope",
    label: "Search reaches",
    hint: "Empty reaches every space.",
    words: (value) => (Array.isArray(value) && value.length > 0 ? value.join(", ") : "Every space"),
  },
  {
    key: "knowledge.vectors",
    label: "Search by meaning",
    hint: "Off also stops embedding pages.",
  },
];

function DocletPane({ service, head }: { service: BuiltInService; head: Head }) {
  const viewer = useViewer();
  const ask = useServicesAsk();
  const enabled = ask.knowledge && viewer.doclet;
  const now = useNow();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const agents = useMemo(() => index.seats.filter((s) => s.kind === "agent"), [index]);
  const chains = useMemo(
    () => new Map((org?.derived?.seats ?? []).map((s) => [s.handle, s.onboarding_chain ?? []])),
    [org],
  );
  const containers = useQuery("containers", undefined, { enabled, pollMs: 60_000 });
  const feed = useQuery("page_activity", { limit: 20 }, { enabled, pollMs: 60_000 });
  const config = useQuery("config", undefined, { enabled: viewer.admin });
  // THE ENGINE'S OWN ROWS of the `knowledge` block, each the value in force
  // beside its default (`settings`, group `knowledge`), changed in place.
  const settings = useQuery("settings", undefined, { enabled: viewer.admin });
  const [tab, setTab] = useTab();

  const spaces = containers.data?.containers ?? [];
  const pages = containers.data ? spaces.reduce((n, c) => n + c.pages, 0) : null;
  const changes = feed.data?.changes ?? [];
  const latest = changes[0]?.at;
  const nameOf = (handle: string | undefined) =>
    index.byHandle.get(handle ?? "")?.name ?? handle ?? "Someone";
  const knowledge = new Map(
    (settings.data?.settings ?? []).filter((r) => r.group === "knowledge").map((r) => [r.key, r]),
  );

  if (!enabled) {
    return (
      <div className="int-detail">
        {head(service.sentence)}
        <Callout variant="neutral">
          {needsWords("Seeing its spaces, pages and what changed", [DOCLET])}
        </Callout>
      </div>
    );
  }

  return (
    <div className="int-detail">
      {head(
        service.sentence,
        service.open && (
          <ButtonLink size="small" variant="primary" href={href(service.open.path)}>
            {service.open.label}
          </ButtonLink>
        ),
        [
          {
            label: "Spaces",
            value: containers.data ? fmtCount(spaces.length) : <EmptyValue label="Reading" />,
            // DOCLET'S OWN LIST OF THEM, each with its pages.
            ...(service.open ? { path: service.open.path } : {}),
          },
          {
            label: "Pages",
            value: pages === null ? <EmptyValue label="Reading" /> : fmtCount(pages),
          },
          {
            label: "Last edit",
            value: latest ? relTime(latest, now) : <EmptyValue label="No edit yet" />,
          },
        ],
      )}
      {/* THE TAB BAR AND ITS PANEL, one step apart (`.tab-detail`). */}
      <div className="tab-detail">
        <ServiceTabs name={service.name} tab={tab} onTab={setTab} agents={agents.length} />
        {tab === "agents" && (
          <div className="int-tab">
            <p className="t-caption">
              Every agent reads and writes in Doclet: it holds the Doclet app with its seat.
            </p>
            <ServiceAgents
              label={`Agents in ${service.name}`}
              detail="Pages"
              state="Space"
              rows={agents.map((a) => {
                const home = homeOf(config.data, a.handle);
                const space = home.space ? spaces.find((c) => c.key === home.space) : undefined;
                const chain = chains.get(a.handle) ?? [];
                return {
                  seat: a,
                  detail: [
                    home.space
                      ? `Writes in ${home.space}${space ? ` (${plural(space.pages, "page")})` : ""}`
                      : "",
                    chain.length > 0
                      ? `Reads ${plural(chain.length, "onboarding page")} first`
                      : "",
                  ]
                    .filter(Boolean)
                    .join(" · "),
                  rank: home.space ? 0 : 1,
                  state: (
                    <StatusDot tone={home.space || !viewer.admin ? "success" : "neutral"}>
                      {home.space
                        ? "Has a space"
                        : viewer.admin
                          ? "No space of its own"
                          : "Reads and writes"}
                    </StatusDot>
                  ),
                };
              })}
            />
          </div>
        )}
        {tab === "activity" && (
          <div className="int-tab">
            <ul className="int-feed">
              {changes.map((c) => (
                <FeedRow
                  key={c.id}
                  who={nameOf(c.actor)}
                  what={
                    <>
                      {verb(c.kind)}{" "}
                      <a className="prose-link" href={href(["doclet", "pages", c.page_id])}>
                        {c.title || "a page"}
                      </a>
                      {c.container ? <span className="t-caption"> in {c.container}</span> : null}
                    </>
                  }
                  at={c.at}
                  now={now}
                />
              ))}
              {feed.data && changes.length === 0 && (
                <li className="t-caption">No page has changed yet.</li>
              )}
            </ul>
          </div>
        )}
        {tab === "settings" && (
          <div className="int-tab">
            <SettingsTable aria-label="Doclet settings">
              <SettingsTable.Row label="Knowledge base" settingKey="knowledge.backend">
                Doclet
              </SettingsTable.Row>
              {DOCLET_SETTINGS.map((d) => {
                const row = knowledge.get(d.key);
                return row ? (
                  <TextSetting
                    key={d.key}
                    row={row}
                    label={d.label}
                    area="Doclet"
                    reread="Reload to see the Doclet settings as they are now."
                    onApplied={settings.refetch}
                    hint={d.hint}
                    {...(d.words ? { words: d.words } : {})}
                    {...(d.key === "knowledge.scope" ? { choices: spaces.map((c) => c.key) } : {})}
                  />
                ) : (
                  <UnreadSetting key={d.key} label={d.label} settingKey={d.key}>
                    {viewer.admin ? <EmptyValue /> : <EmptyValue label="Admins see this" />}
                  </UnreadSetting>
                );
              })}
            </SettingsTable>
            <p className="t-caption">These live in the company document, under knowledge.</p>
          </div>
        )}
      </div>
    </div>
  );
}

/** Chatlet in use: what it gives and the way in, and no count (see the file's doc). */
function ChatPane({ service, head }: { service: BuiltInService; head: Head }) {
  // THE WAY IN IS CHATLET'S: every room is behind the Chatlet app, so a reader
  // without it is told what would open it rather than offered a link to a
  // screen that refuses them.
  const viewer = useViewer();
  // THE COMPANY'S MESSAGE HORIZON, the engine's own row (`limits`, group
  // `chat`): its value in force beside the default, changed in place. A room
  // with no horizon of its own keeps its messages this long.
  const limits = useQuery("limits", undefined, { enabled: viewer.admin });
  const horizon = limits.data?.limits.find((r) => r.key === "chat.chatlet.message_retention_days");
  return (
    <div className="int-detail">
      {head(
        service.sentence,
        viewer.chatlet && service.open && (
          <ButtonLink size="small" variant="primary" href={href(service.open.path)}>
            {service.open.label}
          </ButtonLink>
        ),
      )}
      {!viewer.chatlet && (
        <Callout variant="neutral">
          {needsWords("Talking with your agents in it", [CHATLET])}
        </Callout>
      )}
      <Gives service={service} would={false} />
      <section aria-label={`${service.name} settings`}>
        <h3 className="t-label">Settings</h3>
        <SettingsTable aria-label={`${service.name} settings`}>
          {horizon ? (
            <LimitSetting
              cap={horizon}
              label="Messages are kept for"
              area="Chatlet"
              reread="Reload to see the Chatlet settings as they are now."
              onApplied={limits.refetch}
              off="For ever"
              zero="0 to keep them for ever"
            />
          ) : (
            <UnreadSetting
              label="Messages are kept for"
              settingKey="chat.chatlet.message_retention_days"
            >
              {viewer.admin ? <EmptyValue /> : <EmptyValue label="Admins see this" />}
            </UnreadSetting>
          )}
        </SettingsTable>
        <p className="t-caption">
          A room with no horizon of its own keeps its messages this long. A room’s own horizon is
          set in the room.
        </p>
      </section>
    </div>
  );
}

/**
 * Gitlet in use: what it gives and the way in, and no count (see the file's
 * doc). The way in is the Gitlet app's, as Chatlet's is Chatlet's; Gitlet's
 * settings are an Admin's, as the company document is.
 */
function GitletPane({ service, head }: { service: BuiltInService; head: Head }) {
  const viewer = useViewer();
  const ways = (
    <>
      {viewer.gitlet && service.open && (
        <ButtonLink size="small" variant="primary" href={href(service.open.path)}>
          {service.open.label}
        </ButtonLink>
      )}
      {viewer.admin && (
        <ButtonLink size="small" variant="secondary" href={href(["settings", "gitlet"])}>
          {service.name} settings
        </ButtonLink>
      )}
    </>
  );
  return (
    <div className="int-detail">
      {head(service.sentence, (viewer.gitlet && service.open) || viewer.admin ? ways : undefined)}
      {!viewer.gitlet && (
        <Callout variant="neutral">
          {needsWords("Reading and reviewing the company's code in it", [GITLET])}
        </Callout>
      )}
      <Gives service={service} would={false} />
    </div>
  );
}
