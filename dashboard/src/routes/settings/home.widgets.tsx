/**
 * Settings' widgets: what an admin wants to know without opening Settings,
 * which is what is wrong, waiting or drifting. Backups, nodes, integrations,
 * MCP servers, models, secrets, the configuration, the audit trail and the
 * directory each say their state in a few lines and link to their page.
 *
 * READ ONLY, AND ONLY WHAT THE PAGE READS. Each widget asks the question its
 * screen asks (the same query or `/iam` and `/secrets` read), reuses the
 * screen's own words for a state, and offers no action: taking a backup,
 * fixing a finding or rotating a key happens on the page, behind its own
 * step-up. A secret is named and dated, never shown.
 */

import { useMemo } from "react";
import { EMPTY_VALUE, List, ListItem, StatCard, StatGroup, Tag } from "@crewlethq/ui";
import {
  CodeGlyph,
  CpuGlyph,
  DatabaseGlyph,
  KeyGlyph,
  PlugGlyph,
  ServerGlyph,
  ShieldGlyph,
  ShieldUserGlyph,
  WrenchGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { DIRECTORY_READ, useHumanSeats } from "~/components/people.tsx";
import { alarmKindTitle, alarmsLink } from "~/lib/alarms.ts";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime } from "~/lib/format.ts";
import { configStanding, offRevision } from "~/lib/configStanding.ts";
import { standingCaption } from "./standingWords.ts";
import { authWords, modelLine, usesOf } from "~/lib/models.ts";
import { SERVER_STATE_WORDS } from "~/lib/mcpServers.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRest } from "~/lib/useRest.ts";
import { useViewer } from "~/lib/viewer.ts";
import { rest, type SecretRow } from "~/protocol/index.ts";
import { OPERATOR_SOURCE } from "~/contract/audit.ts";
import { CATALOG, entryState } from "~/routes/settings/integrations/catalogue.tsx";
import { ADMIN } from "~/lib/access.ts";
import { useRetentionReport } from "~/routes/settings/Retention.tsx";
import { useModels } from "~/routes/settings/Models.tsx";
import {
  FINDING_WORDS,
  type DirectoryCheck,
  type DirectoryRow,
} from "~/routes/settings/Access.tsx";
import { FINDING_KINDS } from "~/contract/identity.ts";
import {
  WidgetCard,
  WidgetNote,
  WidgetQuery,
  defineWidgets,
  gapsOf,
} from "~/routes/home/board/kit.tsx";

/** The rows a widget lists before it says how many more its page holds. */
const ROWS = 4;

/** Every widget here polls at its screen's slowest cadence: a Home is glanced at, not watched. */
const POLL_MS = 60_000;

/** "and 3 more" under a list cut to [ROWS], linking to the page that lists them all. */
function More({ total, to }: { total: number; to: string[] }) {
  if (total <= ROWS) return null;
  return (
    <a className="home-card-more t-link" href={href(to)}>
      {`${plural(total - ROWS, "more")} on the page`}
    </a>
  );
}

// ---- Backups ------------------------------------------------------------------

function Backups() {
  const now = useNow();
  const viewer = useViewer();
  const backups = useQuery("backups", undefined, { pollMs: POLL_MS });
  // THE ALARMS ARE THE RETENTION REPORT'S, decided on admin as the page asks it.
  const retention = useRetentionReport(viewer.admin);
  const newest = backups.data?.points.find((p) => p.newest);
  const history = backups.data?.history ?? [];
  const failed = history.filter((r) => r.outcome === "failed").length;
  const alarms = retention.data?.alarms ?? [];
  const link = alarmsLink();
  return (
    <WidgetCard title="Backups" from="settings-backups">
      <WidgetQuery
        error={backups.error}
        refusal={backups.refusal}
        detail={backups.detail ?? undefined}
        loading={backups.loading && !backups.data}
        gaps={gapsOf([backups.data])}
      >
        <StatGroup flush columns={3} className="home-admin-stats">
          <StatCard
            label="Newest backup"
            value={newest ? relTime(newest.taken_at, now) : EMPTY_VALUE}
            sub={newest ? (newest.verified ? "verified" : "not verified") : "none yet"}
          />
          {/* COUNTED OVER THE PAGE THE HISTORY HOLDS, and said to be: past it
              the count is of the newest runs, as the Backups page says. */}
          <StatCard
            label="Failed runs"
            value={failed}
            sub={backups.data?.more ? `in the newest ${history.length}` : "in the recent history"}
          />
          <StatCard
            label="Alarms"
            value={retention.data ? alarms.length : EMPTY_VALUE}
            sub={alarms.length > 0 ? "need a look" : "none standing"}
          />
        </StatGroup>
        {alarms.length > 0 && (
          <List className="home-admin-list">
            {alarms.slice(0, ROWS).map((a) => (
              <ListItem
                key={a.kind}
                href={href(link.path, link.query)}
                leading={<Tag variant="warning">Alarm</Tag>}
              >
                {alarmKindTitle(a.kind)}
              </ListItem>
            ))}
          </List>
        )}
      </WidgetQuery>
    </WidgetCard>
  );
}

// ---- Nodes ----------------------------------------------------------------------

function Nodes() {
  // The lease table's own cadence: slower would show a dead node as alive.
  const fleet = useQuery("fleet", undefined, { pollMs: 15_000 });
  const data = fleet.data;
  const nodes = data?.nodes ?? [];
  // NOT RUNNING THE ACTIVATED REVISION, the Nodes screen's own rule: a node
  // that refused the target reports the target's epoch (`configStanding`).
  const behind = data
    ? nodes
        .filter((n) => offRevision(data, n))
        .map((n) => ({ node: n, caption: standingCaption(configStanding(data, n)) }))
    : [];
  const stranded = data?.unplaceable ?? [];
  return (
    <WidgetCard title="Nodes" from="settings-nodes">
      <WidgetQuery
        error={fleet.error}
        refusal={fleet.refusal}
        loading={fleet.loading && !data}
        // NOTHING TO READ A GAP FROM: the fleet's answer carries no flag
        // saying what it could not account for.
        gaps={[]}
      >
        <StatGroup flush columns={3} className="home-admin-stats">
          <StatCard label="Live nodes" value={nodes.length} sub="holding a lease" />
          <StatCard
            label="Behind on config"
            value={behind.length}
            sub={data?.activation ? `target epoch ${data.activation.epoch}` : "no target yet"}
          />
          <StatCard
            label="Unplaceable"
            value={stranded.length}
            sub={stranded.length > 0 ? "seats with no node" : "nothing stranded"}
          />
        </StatGroup>
        {(behind.length > 0 || stranded.length > 0) && (
          <List className="home-admin-list">
            {behind.slice(0, ROWS).map(({ node: n, caption }) => (
              <ListItem
                key={`node:${n.id}`}
                href={href(["settings", "nodes", n.id])}
                leading={<StandingTag caption={caption} />}
              >
                {n.id}
              </ListItem>
            ))}
            {stranded.slice(0, ROWS).map((s) => (
              <ListItem
                key={`seat:${s.handle}`}
                href={href(["settings", "nodes"])}
                leading={<Tag variant="danger">Unplaceable</Tag>}
              >
                {s.reason ? `${s.handle}: ${s.reason}` : s.handle}
              </ListItem>
            ))}
          </List>
        )}
      </WidgetQuery>
    </WidgetCard>
  );
}

/**
 * Why a node is listed under Nodes: its standing's caption as a tag, a refusal
 * in the danger tone because it is the one a reader has to act on.
 */
function StandingTag({ caption }: { caption: string }) {
  const failed = caption === "refused" || caption === "partial";
  return (
    <Tag variant={failed ? "danger" : "warning"}>
      {caption ? caption.charAt(0).toUpperCase() + caption.slice(1) : "Behind"}
    </Tag>
  );
}

// ---- Integrations ----------------------------------------------------------------

function Integrations() {
  const read = useQuery("integrations", undefined, { pollMs: POLL_MS });
  const states = useMemo(() => {
    const rows = new Map((read.data?.integrations ?? []).map((r) => [r.key, r]));
    return CATALOG.map((entry) => ({ entry, state: entryState(entry, rows, read.data?.tools) }))
      .filter((s) => !s.state.outline || s.state.tone !== "neutral")
      .filter((s) => s.state.tag !== "");
  }, [read.data]);
  const owed = states.filter((s) => s.state.tone !== "success");
  const working = states.length - owed.length;
  return (
    <WidgetCard title="Integrations" from="integrations">
      <WidgetQuery
        error={read.error}
        refusal={read.refusal}
        loading={read.loading && !read.data}
        // THE ANSWER'S COVERAGE IS ITS TRAFFIC COUNTS', which this card does
        // not draw: whether a tool is connected and working is no fleet read.
        gaps={[]}
        empty={
          read.data && states.length === 0
            ? { title: "No tools connected", hint: "Connect one from Integrations." }
            : undefined
        }
      >
        {owed.length === 0 ? (
          <WidgetNote>{`${plural(working, "connected tool")}, all working.`}</WidgetNote>
        ) : (
          <>
            <List>
              {owed.slice(0, ROWS).map(({ entry, state }) => (
                <ListItem
                  key={entry.key}
                  href={href(["company", "integrations", entry.key])}
                  leading={<Tag variant={state.tone}>{state.tag}</Tag>}
                  title={state.reason}
                >
                  {entry.name}
                </ListItem>
              ))}
            </List>
            <More total={owed.length} to={["company", "integrations"]} />
            {working > 0 && <WidgetNote>{`${plural(working, "other tool")} working.`}</WidgetNote>}
          </>
        )}
      </WidgetQuery>
    </WidgetCard>
  );
}

// ---- MCP servers ----------------------------------------------------------------------

function McpServers() {
  const status = useQuery("mcp_servers_status", undefined, { pollMs: POLL_MS });
  const servers = useMemo(
    () =>
      [...(status.data?.servers ?? [])].sort(
        (a, b) => Number(a.state === "running") - Number(b.state === "running"),
      ),
    [status.data],
  );
  const failing = servers.filter((s) => s.state !== "running").length;
  return (
    <WidgetCard title="MCP servers" from="settings-mcp-tools">
      <WidgetQuery
        error={status.error}
        refusal={status.refusal}
        loading={status.loading && !status.data}
        gaps={[]}
        empty={
          status.data && servers.length === 0
            ? { title: "No MCP servers yet", hint: "Add one to give agents more tools." }
            : undefined
        }
      >
        <List>
          {servers.slice(0, ROWS).map((s) => {
            const words = SERVER_STATE_WORDS[s.state] ?? { label: s.state, tone: "neutral" };
            return (
              <ListItem
                key={s.name}
                href={href(["settings", "mcp-tools", "servers", s.name])}
                trailing={<Tag variant={words.tone}>{words.label}</Tag>}
              >
                {s.name}
              </ListItem>
            );
          })}
        </List>
        <More total={servers.length} to={["settings", "mcp-tools"]} />
        {failing === 0 && servers.length > 0 && (
          <WidgetNote>{`${plural(servers.length, "server")}, all running.`}</WidgetNote>
        )}
      </WidgetQuery>
    </WidgetCard>
  );
}

// ---- Models & keys ----------------------------------------------------------------------

function Models() {
  // THE PAGE'S OWN QUESTION, read the way the page reads it. A model no seat
  // names is the one thing here that drifts, so it leads.
  const models = useModels();
  const entries = useMemo(() => {
    const all = models.entries.map((entry) => ({
      entry,
      seats: usesOf(models.seats, entry.key).length,
    }));
    return [...all.filter((m) => m.seats === 0), ...all.filter((m) => m.seats > 0)];
  }, [models.entries, models.seats]);
  const unused = entries.filter((m) => m.seats === 0).length;
  return (
    <WidgetCard title="Models & keys" from="settings">
      <WidgetQuery
        error={models.error}
        refusal={models.refusal}
        // NOT "AND NO DATA": a company with nothing configured answers null,
        // which is an answer, and would otherwise read as loading for ever.
        loading={models.loading}
        gaps={[]}
        empty={
          !models.loading && !models.error && entries.length === 0
            ? { title: "No models configured", hint: "Add one under providers.llm." }
            : undefined
        }
      >
        <List>
          {entries.slice(0, ROWS).map(({ entry, seats: n }) => {
            const sign = authWords(entry.authMode);
            return (
              <ListItem
                key={entry.key}
                href={href(["settings", "models", entry.key])}
                trailing={
                  n === 0 ? (
                    <Tag variant="warning" title="No seat’s chain names this model.">
                      Unused
                    </Tag>
                  ) : (
                    <Tag title={sign.hint}>{sign.label}</Tag>
                  )
                }
              >
                {modelLine(entry)}
              </ListItem>
            );
          })}
        </List>
        <More total={entries.length} to={["settings", "models"]} />
        {unused === 0 && entries.length > 0 && (
          <WidgetNote>{`${plural(entries.length, "model")}, each run by a seat.`}</WidgetNote>
        )}
      </WidgetQuery>
    </WidgetCard>
  );
}

// ---- Secrets ----------------------------------------------------------------------------

interface SecretsRead {
  rows: SecretRow[];
  /** The names the active configuration points at, or null when that could not be read. */
  used: Set<string> | null;
}

/**
 * The secret store's NAMES and the configuration's references to them, as
 * the Secrets screen reads the pair: `/secrets` lists names and dates only,
 * and a 404 on `/config/references` is a deployment with no document yet.
 */
async function readSecrets(signal: AbortSignal): Promise<SecretsRead> {
  const list = (await rest.get("/secrets", signal)) as { secrets?: SecretRow[] } | null;
  let used: Set<string> | null = null;
  try {
    const refs = (await rest.get("/config/references", signal)) as {
      references?: { name: string }[];
    } | null;
    used = new Set((refs?.references ?? []).map((r) => r.name));
  } catch {
    used = null;
  }
  return { rows: list?.secrets ?? [], used };
}

function Secrets() {
  const now = useNow();
  const read = useRest("/secrets+/config/references", readSecrets, { pollMs: POLL_MS });
  const rows = read.data?.rows ?? [];
  // THE PAGE'S OWN ANSWER, `/config/references`: a model's credential is a
  // use like any other field's, and the engine names it there.
  const used = read.data?.used ?? null;
  const unused = used ? rows.filter((r) => !used.has(r.name)) : [];
  const newest = [...rows].sort((a, b) => b.updated_at.localeCompare(a.updated_at))[0];
  return (
    <WidgetCard title="Secrets" from="settings-secrets">
      <WidgetQuery
        error={read.code}
        refusal={read.refusal}
        loading={read.loading && !read.data}
        gaps={[]}
        empty={
          read.data && rows.length === 0
            ? { title: "No secrets stored", hint: "Store one from Secrets." }
            : undefined
        }
      >
        <StatGroup flush columns={3} className="home-admin-stats">
          <StatCard label="Stored" value={rows.length} sub="sealed in the fleet" />
          <StatCard
            label="Not used"
            value={used ? unused.length : EMPTY_VALUE}
            sub={used ? "read by no setting or model" : "could not check"}
          />
          <StatCard
            label="Last changed"
            value={newest ? relTime(newest.updated_at, now) : EMPTY_VALUE}
            sub={newest ? newest.name : "never"}
          />
        </StatGroup>
        {unused.length > 0 && (
          <>
            <List className="home-admin-list">
              {unused.slice(0, ROWS).map((r) => (
                <ListItem
                  key={r.name}
                  href={href(["settings", "secrets", r.name])}
                  leading={<Tag appearance="outline">Unused</Tag>}
                >
                  {r.name}
                </ListItem>
              ))}
            </List>
            <More total={unused.length} to={["settings", "secrets"]} />
          </>
        )}
      </WidgetQuery>
    </WidgetCard>
  );
}

// ---- Configuration ------------------------------------------------------------------------

function Configuration() {
  const now = useNow();
  const audit = useQuery("config_audit", { limit: 20 }, { pollMs: POLL_MS });
  const fleet = useQuery("fleet", undefined, { pollMs: POLL_MS });
  const revisions = audit.data ?? [];
  const active = revisions.find((r) => r.is_active);
  const nodes = fleet.data?.nodes ?? [];
  const behind = nodes.filter((n) => fleet.data && offRevision(fleet.data, n));
  return (
    <WidgetCard title="Configuration" from="settings-config">
      <WidgetQuery
        error={audit.error}
        refusal={audit.refusal}
        loading={audit.loading && !audit.data}
        gaps={[]}
        empty={
          audit.data && revisions.length === 0
            ? { title: "No configuration yet", hint: "Import one with crewlet config import." }
            : undefined
        }
      >
        <StatGroup flush columns={2} className="home-admin-stats">
          <StatCard
            label="Active revision"
            value={active ? active.revision_id.slice(0, 8) : EMPTY_VALUE}
            sub={
              active
                ? `${relTime(active.activated_at ?? active.created_at, now)} by ${
                    active.created_by || "an unnamed author"
                  }`
                : "none active"
            }
          />
          <StatCard
            label="Nodes behind"
            value={fleet.data ? behind.length : EMPTY_VALUE}
            sub={fleet.data ? `of ${plural(nodes.length, "node")}` : "not read"}
          />
        </StatGroup>
        <List className="home-admin-list">
          {revisions.slice(0, 3).map((r) => (
            <ListItem
              key={r.revision_id}
              href={href(["settings", "config", "revisions", r.revision_id])}
              trailing={<span className="t-caption">{relTime(r.created_at, now)}</span>}
            >
              {r.summary || r.revision_id.slice(0, 8)}
            </ListItem>
          ))}
        </List>
      </WidgetQuery>
    </WidgetCard>
  );
}

// ---- Audit log --------------------------------------------------------------------------

interface Change {
  id: string;
  at: string;
  who: string;
  what: string;
  path: string[];
}

function AuditLog() {
  const now = useNow();
  const config = useQuery("config_audit", { limit: ROWS }, { pollMs: POLL_MS });
  const runtime = useQuery("events", { source: OPERATOR_SOURCE, limit: ROWS }, { pollMs: POLL_MS });
  const changes = useMemo<Change[]>(() => {
    const out: Change[] = [];
    for (const r of config.data ?? []) {
      out.push({
        id: `config:${r.revision_id}`,
        at: r.created_at,
        who: r.created_by,
        what: r.summary || "Changed the configuration",
        path: ["settings", "config", "revisions", r.revision_id],
      });
    }
    for (const e of runtime.data?.events ?? []) {
      out.push({
        id: `runtime:${e.id}`,
        at: e.timestamp,
        who: e.actor ?? "",
        what: e.summary || e.type,
        path: ["settings", "audit"],
      });
    }
    return out.sort((a, b) => b.at.localeCompare(a.at)).slice(0, ROWS);
  }, [config.data, runtime.data]);
  const failed = config.error ? config : runtime.error ? runtime : null;
  return (
    <WidgetCard title="Audit log" from="settings-audit">
      <WidgetQuery
        error={failed?.error ?? null}
        refusal={failed?.refusal ?? null}
        loading={(config.loading && !config.data) || (runtime.loading && !runtime.data)}
        // THE RUNTIME HALF IS A FLEET READ, so a node that did not answer is
        // a change that may be missing; the revisions are one replicated table.
        gaps={gapsOf([runtime.data])}
        empty={
          changes.length === 0
            ? { title: "No changes recorded", hint: "Changes people make appear here." }
            : undefined
        }
      >
        <List>
          {changes.map((c) => (
            <ListItem
              key={c.id}
              href={href(c.path)}
              trailing={<span className="t-caption">{relTime(c.at, now)}</span>}
            >
              {c.who ? `${c.who}: ${c.what}` : c.what}
            </ListItem>
          ))}
        </List>
      </WidgetQuery>
    </WidgetCard>
  );
}

// ---- People ---------------------------------------------------------------------------------

const findingWords = (kind: string) =>
  (FINDING_KINDS as readonly string[]).includes(kind)
    ? FINDING_WORDS[kind as (typeof FINDING_KINDS)[number]]
    : undefined;

function PeopleWidget() {
  // One page of the directory is enough to count: a Home says "200+" past it.
  const directory = useRest(
    "/iam/people?limit=200",
    async (signal) =>
      (await rest.get("/iam/people?limit=200", signal)) as {
        people?: DirectoryRow[];
        next?: string;
      } | null,
    DIRECTORY_READ,
  );
  const check = useRest(
    "/iam/check",
    async (signal) => (await rest.get("/iam/check", signal)) as DirectoryCheck | null,
    DIRECTORY_READ,
  );
  const seats = useHumanSeats(true);
  const people = directory.data?.people ?? [];
  const active = people.filter((p) => p.stage === "active").length;
  const invited = (seats.data ?? []).filter((s) => !s.holder && s.invitation).length;
  const findings = check.data?.findings ?? [];
  // A BINDING THIS NODE COULD NOT CHECK is "could not say", which the
  // directory's own page says as well, never "no access problems found".
  const unchecked = check.data?.bindings_unchecked ?? 0;
  return (
    <WidgetCard title="People" from="settings-people">
      <WidgetQuery
        error={directory.code}
        refusal={directory.refusal}
        loading={directory.loading && !directory.data}
        gaps={
          unchecked > 0
            ? [
                `${plural(unchecked, "binding")} couldn’t be checked just now, so this may be incomplete.`,
              ]
            : []
        }
      >
        <StatGroup flush columns={3} className="home-admin-stats">
          <StatCard
            label="People"
            value={`${people.length}${directory.data?.next ? "+" : ""}`}
            sub={`${active} active`}
          />
          <StatCard
            label="Invited"
            value={seats.data ? invited : EMPTY_VALUE}
            sub="waiting to join"
          />
          <StatCard
            label="Admins"
            value={check.data ? check.data.people_with_admin : EMPTY_VALUE}
            sub="people holding Admin"
          />
        </StatGroup>
        {check.data &&
          (findings.length === 0 ? (
            unchecked > 0 ? null : (
              <WidgetNote>No access problems found.</WidgetNote>
            )
          ) : (
            <>
              <List className="home-admin-list">
                {findings.slice(0, ROWS).map((f, i) => {
                  const words = findingWords(f.kind);
                  return (
                    <ListItem
                      key={`${f.kind}:${f.person ?? f.seat ?? i}`}
                      href={href(["settings", "people"])}
                      leading={
                        <Tag variant={words?.tone ?? "neutral"}>{words?.label ?? f.kind}</Tag>
                      }
                    >
                      {f.login || f.seat || f.detail}
                    </ListItem>
                  );
                })}
              </List>
              <More total={findings.length} to={["settings", "people"]} />
            </>
          ))}
      </WidgetQuery>
    </WidgetCard>
  );
}

export const widgets = defineWidgets([
  {
    key: "settings.backups",
    from: "settings-backups",
    label: "Backups",
    description: "Last backup, failed runs and retention alarms",
    icon: DatabaseGlyph,
    hue: "cyan",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: Backups,
  },
  {
    key: "settings.nodes",
    from: "settings-nodes",
    label: "Nodes",
    description: "Live nodes, config drift and stranded seats",
    icon: ServerGlyph,
    hue: "green",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: Nodes,
  },
  {
    key: "integrations.connected",
    from: "integrations",
    label: "Integrations",
    description: "Connected tools and any that need attention",
    icon: PlugGlyph,
    // THE CONNECTED APPS ARE AN ADMIN'S READING on a page every reader opens.
    grants: [ADMIN],
    hue: "purple",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
    once: true,
    Component: Integrations,
  },
  {
    key: "settings.mcp-servers",
    from: "settings-mcp-tools",
    label: "MCP servers",
    description: "Tool servers for agents and their state",
    icon: WrenchGlyph,
    hue: "amber",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
    once: true,
    Component: McpServers,
  },
  {
    key: "settings.models",
    from: "settings",
    label: "Models & keys",
    description: "Each model’s CLI, how it signs in, and any no seat runs",
    icon: CpuGlyph,
    hue: "blue",
    size: { w: 4, minW: 3, minH: 6, vertical: true },
    once: true,
    Component: Models,
  },
  {
    key: "settings.secrets",
    from: "settings-secrets",
    label: "Secrets",
    description: "Stored secrets and any that go unused",
    icon: KeyGlyph,
    hue: "rose",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: Secrets,
  },
  {
    key: "settings.config",
    from: "settings-config",
    label: "Configuration",
    description: "The active revision and whether every node runs it",
    icon: CodeGlyph,
    hue: "cyan",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: Configuration,
  },
  {
    key: "settings.audit",
    from: "settings-audit",
    label: "Audit log",
    description: "Recent changes people made",
    icon: ShieldGlyph,
    hue: "purple",
    size: { w: 6, minW: 4, minH: 8, vertical: true },
    once: true,
    Component: AuditLog,
  },
  {
    key: "settings.people",
    from: "settings-people",
    label: "People",
    description: "People, pending invites and access problems",
    icon: ShieldUserGlyph,
    hue: "green",
    size: { w: 6, minW: 4, minH: 8, vertical: true },
    once: true,
    Component: PeopleWidget,
  },
]);
