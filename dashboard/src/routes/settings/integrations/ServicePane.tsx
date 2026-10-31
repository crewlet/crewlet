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
 *    open, blocked and overdue work (`work_workload`), and the space an agent
 *    writes in and the pages it reads at onboarding (the company document and
 *    the derived chart).
 *  - THE COUNTS. Projects and their open tasks (`work_projects`), spaces and
 *    their pages (`containers`), and the newest change on each log
 *    (`work_activity`, `page_activity`), in the header. The projects and the
 *    spaces counted there link to the service's own list of them, which
 *    names each with its counts: an Overview tab that listed them again here
 *    was a second copy of that page. No delivery count, because a built-in
 *    service has no inbound edge to count.
 *  - THE SETTINGS. The company document's `tracker` and `knowledge` blocks,
 *    with the engine's own defaults where a key is unset, read only by an
 *    Admin, as the `config` question is.
 *  - CHATLET COUNTS NOTHING: the rooms it answers for are the reader's own
 *    seat's, never the company's, so its page names no count and no roster,
 *    only what it gives and the way in, which is offered to a holder of Chatlet
 *    as Worklet and Doclet offer theirs to a holder of their apps.
 *
 * A service the company runs elsewhere says what replaces it and what
 * running it here would mean, with no control: switching is not offered yet.
 */

import { useMemo, useState, type ReactNode } from "react";
import {
  ButtonLink,
  Callout,
  DescriptionList,
  EmptyValue,
  List,
  StatusDot,
  Tabs,
  Tag,
} from "@crewlethq/ui";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { VendorMark } from "@crewlethq/icons";
import { CheckGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { documentUnits, handleLabel, indexOrg, type Seat } from "~/lib/seats.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useServicesAsk } from "~/lib/services.ts";
import { useNow } from "~/lib/clock.ts";
import { CHATLET, DOCLET, WORKLET, needsWords } from "~/lib/access.ts";
import { fmtCount, plural, relTime } from "~/lib/format.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { CompanyDocument } from "~/protocol/types.ts";
import { FeedRow, ServiceMark, ServiceState } from "./parts.tsx";
import type { BuiltInService, Model, ReplacingApp } from "./model.ts";

/** What a settings row says for a key the company document leaves unset. */
const DEFAULT = "The engine's default";

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

function block(
  doc: CompanyDocument | null | undefined,
  key: "tracker" | "knowledge",
): Record<string, unknown> {
  return asRecord((doc as Record<string, unknown> | null | undefined)?.[key]);
}

/** A value of the company document as an object, or an empty one where it is none. */
function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

/** What each service holds, as the off state's sentences name it. */
const HOLDS: Record<BuiltInService["key"], string> = {
  worklet: "tasks",
  doclet: "pages",
  chat: "conversations",
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
  const only = apps.length === 1 ? apps[0] : undefined;
  // THE SENTENCE NAMES THE CONNECTION THE APP RIDES: Jira and Confluence
  // through the one Atlassian integration, a chat vendor as itself. A vendor
  // nobody named (the `viewer` answer tells the names to a holder of Chatlet) is
  // still a vendor's chat, said in words.
  const how =
    service.key === "chat"
      ? `Your agents talk on ${by?.name}`
      : `Your agents work in ${by?.name} through the Atlassian integration`;
  return (
    <div className="int-service-off">
      <Callout
        variant="neutral"
        icon={only ? <VendorMark vendor={connectionOf(only)} size="md" /> : undefined}
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
      <section aria-label={`What ${service.name} would give you`}>
        <h3 className="t-label">Running it here would give you</h3>
        <List variant="plain" className="int-checks">
          {GIVES[service.key].map((line) => (
            <List.Item key={line} leading={<CheckGlyph size="sm" aria-hidden />}>
              {line}
            </List.Item>
          ))}
        </List>
      </section>
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

/** A phrase as the start of a sentence: "a vendor's chat" is "A vendor's chat". */
function capitalised(phrase: string): string {
  return phrase.charAt(0).toUpperCase() + phrase.slice(1);
}

/** One agent's line on a service's Agents tab. */
function AgentLine({ seat, detail, state }: { seat: Seat; detail: string; state: ReactNode }) {
  return (
    <div className="int-seat">
      <SeatAvatar name={seat.name} kind="agent" avatar={seat.avatar} size="sm" decorative />
      <div className="int-seat-id">
        <span className="int-seat-name">{seat.name}</span>
        <span className="t-caption">{detail}</span>
      </div>
      {state}
      <span className="int-seat-act" />
    </div>
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

type Tab = "agents" | "activity" | "settings";

function useTab(): [Tab, (t: string) => void] {
  const [tab, setTab] = useState<Tab>("agents");
  return [tab, (t) => setTab(t as Tab)];
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
  const projects = useQuery("work_projects", { limit: 200 }, { enabled, pollMs: 120_000 });
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
  const busy = agents.filter((a) => (load.get(a.handle)?.open ?? 0) > 0).length;
  const records = feed.data?.records ?? [];
  const latest = records[0]?.at;
  const keys = feed.data?.keys ?? {};
  const nameOf = (handle: string | undefined) =>
    agents.find((a) => a.handle === handle)?.name ?? handle ?? "Someone";
  // THE INBOX HORIZON IS WORKLET'S OWN POLICY, kept in the tracker block's
  // `worklet` section (`config.Tracker.Worklet`), never on the block itself.
  // AN UNSET KEY IS THE ENGINE'S DEFAULT, said as such: a copy of the
  // engine's constant here would be a second answer that drifts.
  const days = asRecord(block(config.data, "tracker").worklet).inbox_retention_days;
  const retention = typeof days === "number" && days > 0 ? `${days} days` : DEFAULT;

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
            label: "Agents with work",
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
            <div className="int-seats">
              {agents.map((a) => {
                const row = load.get(a.handle);
                const home = homeOf(config.data, a.handle);
                const detail = [
                  home.project ? `Files work in ${home.project}` : "",
                  row ? `${row.blocked} blocked · ${row.overdue} overdue` : "",
                ]
                  .filter(Boolean)
                  .join(" · ");
                return (
                  <AgentLine
                    key={a.handle}
                    seat={a}
                    detail={detail || handleLabel(a.handle)}
                    state={
                      <Tag
                        size="sm"
                        variant={row && row.open > 0 ? "info" : "neutral"}
                        appearance={row && row.open > 0 ? "soft" : "outline"}
                      >
                        {row && row.open > 0 ? `${row.open} open` : "Nothing open"}
                      </Tag>
                    }
                  />
                );
              })}
            </div>
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
            <DescriptionList
              dense
              items={[
                ["Work tracker", "Worklet"],
                [
                  "Inbox keeps a row for",
                  viewer.admin ? retention : <EmptyValue label="Admins see this" />,
                ],
                [
                  "Projects the chart declares",
                  viewer.admin ? (
                    documentUnits(config.data)
                      .map((u) => u.project)
                      .filter(Boolean)
                      .join(", ") || <EmptyValue label="None" />
                  ) : (
                    <EmptyValue label="Admins see this" />
                  ),
                ],
              ]}
            />
            <p className="t-caption">These live in the company document, under tracker.</p>
          </div>
        )}
      </div>
    </div>
  );
}

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
  const [tab, setTab] = useTab();

  const spaces = containers.data?.containers ?? [];
  const pages = containers.data ? spaces.reduce((n, c) => n + c.pages, 0) : null;
  const changes = feed.data?.changes ?? [];
  const latest = changes[0]?.at;
  const knowledge = block(config.data, "knowledge");
  const nameOf = (handle: string | undefined) =>
    index.byHandle.get(handle ?? "")?.name ?? handle ?? "Someone";
  const skills =
    typeof knowledge.skills_container === "string" ? knowledge.skills_container : undefined;
  const scope = Array.isArray(knowledge.scope) ? (knowledge.scope as string[]) : [];

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
            <div className="int-seats">
              {agents.map((a) => {
                const home = homeOf(config.data, a.handle);
                const space = home.space ? spaces.find((c) => c.key === home.space) : undefined;
                const chain = chains.get(a.handle) ?? [];
                return (
                  <AgentLine
                    key={a.handle}
                    seat={a}
                    detail={
                      [
                        home.space
                          ? `Writes in ${home.space}${space ? ` (${plural(space.pages, "page")})` : ""}`
                          : "",
                        chain.length > 0
                          ? `Reads ${plural(chain.length, "onboarding page")} first`
                          : "",
                      ]
                        .filter(Boolean)
                        .join(" · ") || handleLabel(a.handle)
                    }
                    state={
                      <StatusDot tone={home.space || !viewer.admin ? "success" : "neutral"}>
                        {home.space
                          ? "Has a space"
                          : viewer.admin
                            ? "No space of its own"
                            : "Reads and writes"}
                      </StatusDot>
                    }
                  />
                );
              })}
            </div>
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
            <DescriptionList
              dense
              items={[
                ["Knowledge base", "Doclet"],
                ...(viewer.admin
                  ? ([
                      [
                        "Company pages",
                        typeof knowledge.root_space === "string" ? knowledge.root_space : DEFAULT,
                      ],
                      ["Tool skills", skills === "" ? "Off" : (skills ?? DEFAULT)],
                      ["Search reaches", scope.length > 0 ? scope.join(", ") : "Every space"],
                      [
                        "Search by meaning",
                        knowledge.vectors === false
                          ? "Off"
                          : knowledge.vectors === true
                            ? "On"
                            : "When an embeddings provider is set",
                      ],
                    ] as [string, string][])
                  : ([["Everything else", "Admins see this"]] as [string, string][])),
              ]}
            />
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
      <section aria-label={`What ${service.name} gives you`}>
        <h3 className="t-label">What it gives you</h3>
        <List variant="plain" className="int-checks">
          {GIVES.chat.map((line) => (
            <List.Item key={line} leading={<CheckGlyph size="sm" aria-hidden />}>
              {line}
            </List.Item>
          ))}
        </List>
      </section>
    </div>
  );
}
