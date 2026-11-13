/**
 * Atlas answers for the suites, typed against the contract: a new company
 * that connected only its code host, a grown one with a trace and a
 * disagreement, and generated companies of any size. Every name is neutral
 * (`acme`), every address a documentation range.
 *
 * Not a suite itself, and never imported by anything that ships.
 */

import type {
  AtlasActivityAnswer,
  AtlasEdge,
  AtlasEntry,
  AtlasEntryAnswer,
  AtlasEntriesAnswer,
  AtlasFacetsAnswer,
  AtlasFactView,
  AtlasFamily,
  AtlasHealth,
  AtlasKind,
  AtlasLane,
  AtlasLogAnswer,
  AtlasMapAnswer,
  AtlasMapEdge,
  AtlasRead,
  AtlasReviewAnswer,
  AtlasReviewItem,
  AtlasRelation,
  AtlasSummary,
  AtlasTraceAnswer,
  AtlasVendor,
  AtlasWord,
} from "~/contract/atlas.ts";

export const SEEN = "2026-11-11T09:00:00Z";

/** The lane each kind stands in, as the engine's table has it. */
const LANE_OF: Readonly<Record<AtlasKind, AtlasLane>> = {
  domain: "entry",
  hostname: "entry",
  endpoint: "entry",
  certificate: "entry",
  service: "services",
  workload: "services",
  host: "runtime",
  datastore: "data",
  queue: "data",
  bucket: "data",
  vendor_account: "platforms",
  repository: "code",
  pipeline: "code",
  monitor: "observability",
  tool_server: "tools",
  model_provider: "tools",
};

const LANES: readonly AtlasLane[] = [
  "entry",
  "services",
  "runtime",
  "data",
  "platforms",
  "code",
  "observability",
  "tools",
];

const BAND: ReadonlySet<AtlasLane> = new Set(["code", "observability", "tools"]);

/** A stable uuid for a name, so a fixture's ids never move between runs. */
export function idFor(name: string): string {
  let h = 0x811c9dc5;
  for (const ch of name) h = Math.imul(h ^ ch.charCodeAt(0), 0x01000193) >>> 0;
  const hex = (n: number) => n.toString(16).padStart(8, "0");
  const a = hex(h);
  const b = hex(Math.imul(h, 0x9e3779b1) >>> 0);
  const c = hex(Math.imul(h ^ 0x5bd1e995, 0x85ebca6b) >>> 0);
  const d = hex(Math.imul(h ^ 0xc2b2ae35, 0x27d4eb2f) >>> 0);
  return `${a}-${b.slice(0, 4)}-4${b.slice(5, 8)}-8${c.slice(1, 4)}-${c.slice(4)}${d}`;
}

/** One entry as a row or a card shows it. */
export function summary(
  kind: AtlasKind,
  name: string,
  over: Partial<AtlasSummary> & { vendor?: AtlasVendor } = {},
): AtlasSummary {
  return {
    // AN ID IS ITS KIND AND KEY, as the engine derives it: two accounts both
    // called acme are two entries.
    id: idFor(`${kind}:${over.key ?? name}`),
    kind,
    lane: LANE_OF[kind],
    name,
    key: name,
    state: "confirmed",
    word: "confirmed",
    relations: { depends_on: 0, used_by: 0 },
    health: "quiet",
    signals: 0,
    review: false,
    not_used_recently: false,
    last_seen: SEEN,
    ...over,
  };
}

/** What every answer carries: a read of every source in full. */
export function read(over: Partial<AtlasRead> = {}): AtlasRead {
  return {
    revision: "r7",
    coverage: [
      { source: "settings", complete: true },
      { source: "gitlet", complete: true },
      { source: "integrations", complete: true },
    ],
    withheld: 0,
    ...over,
  };
}

/** A company's map: its entries, the relations between them, its groups. */
export interface AtlasFixture {
  nodes: AtlasSummary[];
  edges: AtlasMapEdge[];
}

/** The map answer for a fixture, every group open unless named closed. */
export function mapAnswer(
  fixture: AtlasFixture,
  over: { closed?: readonly string[]; read?: Partial<AtlasRead> } = {},
): AtlasMapAnswer {
  const closed = new Set(over.closed ?? []);
  const groups = new Map<string, AtlasMapAnswer["groups"][number]>();
  for (const node of fixture.nodes) {
    const key = node.vendor ? `${node.lane}/${node.vendor}` : node.lane;
    const group = groups.get(key) ?? {
      key,
      lane: node.lane,
      ...(node.vendor ? { vendor: node.vendor } : {}),
      count: 0,
      closed: closed.has(key),
    };
    group.count += 1;
    groups.set(key, group);
  }
  const shown = fixture.nodes.filter(
    (node) => !closed.has(node.vendor ? `${node.lane}/${node.vendor}` : node.lane),
  );
  const drawn = new Set(shown.map((node) => node.id));
  return {
    ...read(over.read),
    lanes: LANES.map((lane) => ({
      lane,
      band: BAND.has(lane),
      count: fixture.nodes.filter((node) => node.lane === lane).length,
    })),
    groups: [...groups.values()],
    nodes: shown,
    edges: fixture.edges.filter((edge) => drawn.has(edge.from) && drawn.has(edge.to)),
    collapsed: [...closed].filter((key) => groups.has(key)),
  };
}

/** The facet counts for a fixture. */
export function facetsAnswer(
  fixture: AtlasFixture,
  over: Partial<AtlasRead> = {},
): AtlasFacetsAnswer {
  const count = <K extends string>(pick: (node: AtlasSummary) => K | undefined) => {
    const out: Partial<Record<K, number>> = {};
    for (const node of fixture.nodes) {
      const key = pick(node);
      if (key) out[key] = (out[key] ?? 0) + 1;
    }
    return out;
  };
  return {
    ...read(over),
    kinds: count((node) => node.kind),
    lanes: count((node) => node.lane),
    vendors: count((node) => node.vendor),
    words: count((node) => node.word),
    health: count((node) => node.health),
    total: fixture.nodes.length,
  };
}

/** The healths, the most urgent first, as the engine's health order reads them. */
const HEALTHS: readonly AtlasHealth[] = ["alerting", "needs_look", "quiet"];

/**
 * One page of a fixture as a list, in the fixture's own order or, asked by
 * health, the most urgent first with that order inside each health.
 */
export function entriesAnswer(
  fixture: AtlasFixture,
  page = 50,
  after = 0,
  sort = "",
): AtlasEntriesAnswer {
  const ordered =
    sort === "health"
      ? HEALTHS.flatMap((health) => fixture.nodes.filter((node) => node.health === health))
      : fixture.nodes;
  const rows = ordered.slice(after, after + page);
  return {
    ...read(),
    rows,
    ...(after + page < ordered.length ? { next_cursor: String(after + page) } : {}),
    total: ordered.length,
  };
}

type Line = AtlasLogAnswer["lines"][number];

/** How many days the activity read counts over. */
const ACTIVITY_DAYS = 14;
const DAY_MS = 86_400_000;

/** The sentences a line of the log says, in the engine's own words. */
const LINE_TEXTS: readonly [Line["change"], string][] = [
  ["recorded", "Seen by its source"],
  ["fact_set", "Recorded its status"],
  ["related", "Recorded a relation: runs on"],
];

/** The lines the log holds of one entry: a few a week, the same on every run. */
function linesOf(node: AtlasSummary, index: number): Line[] {
  const now = Date.parse(SEEN);
  const count = (index * 5) % 7;
  return Array.from({ length: count }, (_, i): Line => {
    const [change, text] = LINE_TEXTS[(index + i) % LINE_TEXTS.length]!;
    return {
      id: idFor(`line:${node.id}:${i}`),
      at: new Date(now - (i * 37 + index * 3) * 3_600_000).toISOString(),
      entry: node.id,
      name: node.name,
      kind: node.kind,
      change,
      text,
      source: node.vendor
        ? { kind: "integration", name: node.vendor }
        : { kind: "person", name: "ada" },
    };
  });
}

/** One page of the log, newest first: the company's, or one entry's. */
export function logAnswer(fixture: AtlasFixture, id = ""): AtlasLogAnswer {
  const lines = fixture.nodes
    .flatMap((node, index) => (id === "" || node.id === id ? linesOf(node, index) : []))
    .sort((a, b) => (a.at < b.at ? 1 : -1));
  return { ...read(), lines };
}

/** The log counted over its last fourteen days, for the company and for each entry it holds a line of. */
export function activityAnswer(fixture: AtlasFixture): AtlasActivityAnswer {
  const end = Math.floor(Date.parse(SEEN) / DAY_MS) * DAY_MS;
  const from = end - (ACTIVITY_DAYS - 1) * DAY_MS;
  const days = Array.from({ length: ACTIVITY_DAYS }, () => 0);
  const out: AtlasActivityAnswer["entries"] = [];
  fixture.nodes.forEach((node, index) => {
    const lines = linesOf(node, index);
    if (lines.length === 0) return;
    const own = Array.from({ length: ACTIVITY_DAYS }, () => 0);
    for (const line of lines) {
      const day = Math.floor((Date.parse(line.at) - from) / DAY_MS);
      if (day < 0 || day >= ACTIVITY_DAYS) continue;
      own[day] = (own[day] ?? 0) + 1;
      days[day] = (days[day] ?? 0) + 1;
    }
    out.push({ id: node.id, ...(own.some((n) => n > 0) ? { days: own } : {}), last: lines[0]! });
  });
  return { ...read(), from: new Date(from).toISOString(), days, entries: out };
}

/** Each entry's relation counts, read off the fixture's lines the map draws at rest. */
export function counted(fixture: AtlasFixture): AtlasFixture {
  const down = new Map<string, number>();
  const up = new Map<string, number>();
  for (const one of fixture.edges) {
    if (one.family === "around") continue;
    down.set(one.from, (down.get(one.from) ?? 0) + 1);
    up.set(one.to, (up.get(one.to) ?? 0) + 1);
  }
  for (const node of fixture.nodes) {
    node.relations = { depends_on: down.get(node.id) ?? 0, used_by: up.get(node.id) ?? 0 };
  }
  return fixture;
}

/** A fact as the atlas holds it, from the settings unless named otherwise. */
export function fact(
  name: AtlasFactView["fact"],
  value: AtlasFactView["value"],
  over: Partial<AtlasFactView> = {},
): AtlasFactView {
  return {
    fact: name,
    value,
    source: { kind: "settings", revision: "r7" },
    confidence: "declared",
    word: "confirmed",
    observed_at: SEEN,
    standing: "settled",
    ...over,
  };
}

/** One entry in full, from its summary. */
export function entryAnswer(
  node: AtlasSummary,
  over: {
    facts?: AtlasFactView[];
    relations?: AtlasEdge[];
    used_by?: AtlasEntryAnswer["used_by"];
    entry?: Partial<AtlasEntry>;
    health?: AtlasHealth;
  } = {},
): AtlasEntryAnswer {
  return {
    ...read(),
    entry: {
      id: node.id,
      ref: { kind: node.kind, key: node.key },
      lane: node.lane,
      name: node.name,
      members: [{ kind: node.kind, key: node.key }],
      state: node.state,
      confidence: "declared",
      word: node.word,
      facts: over.facts ?? [],
      relations: over.relations ?? [],
      sources: [{ kind: "settings", revision: "r7" }],
      last_seen: node.last_seen,
      ...over.entry,
    },
    health: over.health ?? node.health,
    ...(node.vendor ? { vendor: node.vendor } : {}),
    used_by: over.used_by ?? [],
  };
}

/** A trace answer: what the fixture's edges reach from one entry. */
export function traceAnswer(
  fixture: AtlasFixture,
  from: AtlasSummary,
  direction: "up" | "down",
  depth = 6,
): AtlasTraceAnswer {
  const byId = new Map(fixture.nodes.map((node) => [node.id, node]));
  const seen = new Set([from.id]);
  const hops: AtlasTraceAnswer["hops"] = [];
  const edges: AtlasMapEdge[] = [];
  let frontier = [from.id];
  for (let distance = 1; distance <= depth && frontier.length > 0; distance++) {
    const next: string[] = [];
    for (const edge of fixture.edges) {
      const [near, far] = direction === "up" ? [edge.to, edge.from] : [edge.from, edge.to];
      if (!frontier.includes(near) || seen.has(far)) continue;
      seen.add(far);
      next.push(far);
      edges.push(edge);
    }
    if (next.length > 0) {
      hops.push({ distance, entries: next.map((id) => byId.get(id)!).filter(Boolean) });
    }
    frontier = next;
  }
  return { ...read(), from, direction, hops, edges, truncated: false };
}

/** The family each relation is drawn in, as the engine's `Relation.Family`. */
const FAMILY_OF: Record<AtlasRelation, AtlasFamily> = {
  depends_on: "calls",
  exposes: "calls",
  served_by: "calls",
  fronted_by: "calls",
  runs_on: "runs_on",
  resolves_to: "runs_on",
  in_zone: "around",
  built_from: "around",
  deployed_by: "around",
  monitored_by: "around",
  secured_by: "around",
  belongs_to: "around",
  documented_in: "around",
  owned_by: "around",
};

function edge(from: AtlasSummary, relation: AtlasRelation, to: AtlasSummary): AtlasMapEdge {
  return { from: from.id, to: to.id, relation, family: FAMILY_OF[relation] };
}

/**
 * A new company that connected only GitHub: its account, three repositories
 * and the model the settings declare. Every other lane is empty.
 */
export function sparseCompany(): AtlasFixture {
  const gh = summary("vendor_account", "acme", { key: "github/acme", vendor: "github" });
  const repos = ["acme/web", "acme/api", "acme/infra"].map((name) =>
    summary("repository", name, { vendor: "github" }),
  );
  const model = summary("model_provider", "Claude", { key: "claude", vendor: "anthropic" });
  return { nodes: [gh, ...repos, model], edges: [] };
}

/** The entries of a grown company, by name, for a suite to point at. */
export interface GrownCompany extends AtlasFixture {
  ordersDb: AtlasSummary;
  checkout: AtlasSummary;
  web1: AtlasSummary;
  monitor: AtlasSummary;
}

/**
 * About fifty entries after people and agents have recorded what they see:
 * a request path from the zone to the database, an alerting monitor on the
 * database, an unconfirmed service and a server whose address two sources
 * disagree on.
 */
export function grownCompany(): GrownCompany {
  const cf = summary("vendor_account", "acme", {
    key: "cloudflare/0123456789abcdef",
    vendor: "cloudflare",
  });
  const aws = summary("vendor_account", "acme-prod", { key: "aws/123456789012", vendor: "aws" });
  const gh = summary("vendor_account", "acme", { key: "github/acme", vendor: "github" });
  const dd = summary("vendor_account", "acme", { key: "datadog/acme", vendor: "datadog" });
  const zone = summary("domain", "acme.example", { vendor: "cloudflare" });
  const hosts = ["api", "app", "www", "status", "admin"].map((name) =>
    summary("hostname", `${name}.acme.example`, { vendor: "cloudflare" }),
  );
  const endpoint = summary("endpoint", "https://api.acme.example/v1", { vendor: "cloudflare" });
  const checkout = summary("service", "checkout-api", {
    health: "needs_look",
    review: true,
    reason: "Two sources disagree, and nobody has settled it",
    owner: { kind: "unit", name: "payments" },
    purpose: "Takes an order from the basket to a payment",
  });
  const services = ["orders-api", "billing-api", "search-api", "auth-api", "notify-worker"].map(
    (name) => summary("service", name),
  );
  const guess = summary("service", "legacy-reports", { word: "unconfirmed" });
  const web1 = summary("host", "web-1", {
    key: "ssh/web-1",
    vendor: "aws",
    health: "needs_look",
    reason: "Two sources disagree, and nobody has settled it",
  });
  const servers = ["web-2", "worker-1", "worker-2", "db-1", "db-2", "bastion"].map((name) =>
    summary("host", name, { key: `ssh/${name}`, vendor: "aws" }),
  );
  const ordersDb = summary("datastore", "orders-db", {
    key: "on/ssh/db-1/postgres/5432",
    vendor: "aws",
    health: "alerting",
    signals: 1,
    reason: "A standing alarm is raised",
    owner: { kind: "seat", name: "sre" },
    purpose: "Orders and their line items",
  });
  const data = [
    summary("datastore", "billing-db", { key: "on/ssh/db-2/postgres/5432", vendor: "aws" }),
    summary("queue", "orders", { key: "sqs/orders", vendor: "aws" }),
    summary("queue", "emails", { key: "sqs/emails", vendor: "aws" }),
    summary("bucket", "acme-assets", { key: "s3/acme-assets", vendor: "aws" }),
    summary("bucket", "acme-backups", { key: "s3/acme-backups", vendor: "aws" }),
  ];
  const repos = ["acme/web", "acme/api", "acme/infra", "acme/billing"].map((name) =>
    summary("repository", name, { vendor: "github" }),
  );
  const pipelines = ["deploy", "test"].map((name) =>
    summary("pipeline", `acme/api ${name}`, { key: `acme/api/${name}`, vendor: "github" }),
  );
  const monitor = summary("monitor", "orders-db cpu high", {
    key: "datadog/1234",
    vendor: "datadog",
    health: "alerting",
    signals: 1,
    reason: "A standing alarm is raised",
  });
  const monitors = ["API 5xx", "checkout latency"].map((name, i) =>
    summary("monitor", name, { key: `datadog/${2000 + i}`, vendor: "datadog" }),
  );
  const tools = ["github", "datadog", "search"].map((name) =>
    summary("tool_server", name, { key: `mcp/${name}` }),
  );
  const models = [
    summary("model_provider", "Claude", { key: "claude", vendor: "anthropic" }),
    summary("model_provider", "GPT", {
      key: "gpt",
      vendor: "openai",
      health: "needs_look",
      reason: "The model's plan is spent, so it sits out until it resets",
    }),
  ];
  const [api, app] = hosts;
  const [ordersApi, billingApi, searchApi, authApi, notify] = services;
  const [web2, worker1, worker2, db1, db2] = servers;
  const [billingDb, ordersQueue, emails, assets] = data;
  const nodes = [
    cf,
    aws,
    gh,
    dd,
    zone,
    ...hosts,
    endpoint,
    checkout,
    ...services,
    guess,
    web1,
    ...servers,
    ordersDb,
    ...data,
    ...repos,
    ...pipelines,
    monitor,
    ...monitors,
    ...tools,
    ...models,
  ];
  const edges: AtlasMapEdge[] = [
    ...hosts.map((host) => edge(host, "in_zone", zone)),
    edge(endpoint, "exposes", api!),
    edge(api!, "resolves_to", web1),
    edge(app!, "resolves_to", web2!),
    edge(checkout, "runs_on", web1),
    edge(checkout, "depends_on", ordersApi!),
    edge(checkout, "depends_on", billingApi!),
    edge(ordersApi!, "runs_on", web1),
    edge(ordersApi!, "depends_on", ordersDb),
    edge(ordersApi!, "depends_on", ordersQueue!),
    edge(billingApi!, "runs_on", web2!),
    edge(billingApi!, "depends_on", billingDb!),
    edge(searchApi!, "runs_on", worker1!),
    edge(authApi!, "runs_on", worker2!),
    edge(notify!, "depends_on", emails!),
    edge(notify!, "runs_on", worker2!),
    edge(guess, "depends_on", ordersDb),
    edge(ordersDb, "runs_on", db1!),
    edge(billingDb!, "runs_on", db2!),
    edge(searchApi!, "depends_on", assets!),
    // WHAT STANDS AROUND: drawn only for the entry a reader stands on.
    edge(ordersDb, "monitored_by", monitor),
    edge(zone, "belongs_to", cf),
    edge(web1, "belongs_to", aws),
    edge(checkout, "built_from", repos[1]!),
  ];
  return { ...counted({ nodes, edges }), ordersDb, checkout, web1, monitor };
}

/** The entry answer for web-1, whose address two sources disagree on and which is internal. */
export function disputedServer(company: GrownCompany): AtlasEntryAnswer {
  return entryAnswer(company.web1, {
    facts: [
      fact(
        "address",
        { type: "ip", text: "10.0.4.12", private: true },
        {
          source: { kind: "person", name: "ada" },
          confidence: "asserted",
          standing: "contested",
          others: [
            {
              value: { type: "ip", text: "10.0.4.19", private: true },
              source: {
                kind: "agent",
                name: "sre",
                turn: idFor("turn"),
                evidence: "call",
                tool: "ssh_read",
              },
              confidence: "observed",
              observed_at: "2026-11-11T10:30:00Z",
            },
          ],
        },
      ),
      fact("port", { type: "port", text: "22" }),
      fact(
        "region",
        { type: "region", text: "eu-west-1" },
        {
          source: { kind: "integration", name: "aws" },
          confidence: "verified",
        },
      ),
      fact("containers", { type: "word", text: "true" }),
    ],
    relations: [
      {
        relation: "belongs_to",
        to: idFor("vendor_account:aws/123456789012"),
        source: { kind: "settings", revision: "r7" },
        confidence: "declared",
        word: "confirmed",
        observed_at: SEEN,
        standing: "settled",
      },
    ],
    used_by: company.edges
      .filter((one) => one.to === company.web1.id)
      .map((one) => ({
        relation: one.relation,
        entry: company.nodes.find((node) => node.id === one.from)!,
      })),
    entry: {
      review: true,
      sources: [
        { kind: "settings", revision: "r7" },
        { kind: "person", name: "ada" },
        { kind: "agent", name: "sre", turn: idFor("turn"), evidence: "call" },
        { kind: "integration", name: "aws" },
      ],
      use: [
        {
          channel: "command",
          last: "2026-11-11T08:00:00Z",
          counted: true,
          week: 14,
          month: 52,
          seats: ["sre"],
        },
      ],
    },
  });
}

const VENDORS_BY_LANE: Readonly<Record<AtlasLane, readonly AtlasVendor[]>> = {
  entry: ["cloudflare"],
  services: [],
  runtime: ["aws", "google_cloud", "digitalocean", "scaleway"],
  data: ["aws", "google_cloud"],
  platforms: ["aws", "cloudflare", "github", "datadog", "google_cloud"],
  code: ["github", "gitlab"],
  observability: ["datadog"],
  tools: ["anthropic", "openai"],
};

const KINDS_BY_LANE: Readonly<Record<AtlasLane, readonly AtlasKind[]>> = {
  entry: ["hostname", "domain", "endpoint"],
  services: ["service"],
  runtime: ["host"],
  data: ["datastore", "queue", "bucket"],
  platforms: ["vendor_account"],
  code: ["repository", "pipeline"],
  observability: ["monitor"],
  tools: ["tool_server", "model_provider"],
};

/** The share of a generated company each lane holds, out of 100. */
const SHARE: Readonly<Record<AtlasLane, number>> = {
  entry: 22,
  services: 14,
  runtime: 20,
  data: 12,
  platforms: 4,
  code: 16,
  observability: 8,
  tools: 4,
};

/** A deterministic company of `count` entries spread over every lane, with a request path through it. */
export function generatedCompany(count: number): AtlasFixture {
  const nodes: AtlasSummary[] = [];
  const healths: AtlasHealth[] = [
    "quiet",
    "quiet",
    "quiet",
    "quiet",
    "quiet",
    "quiet",
    "needs_look",
    "alerting",
  ];
  const words: AtlasWord[] = ["confirmed", "confirmed", "confirmed", "likely"];
  for (const lane of LANES) {
    const n = Math.max(1, Math.round((count * SHARE[lane]) / 100));
    for (let i = 0; i < n && nodes.length < count; i++) {
      const kinds = KINDS_BY_LANE[lane];
      const vendors = VENDORS_BY_LANE[lane];
      const kind = kinds[i % kinds.length]!;
      const vendor = vendors.length > 0 ? vendors[i % vendors.length] : undefined;
      nodes.push(
        summary(kind, `${lane}-${String(i + 1).padStart(4, "0")}`, {
          ...(vendor ? { vendor } : {}),
          health: healths[(i * 7) % healths.length]!,
          word: words[(i * 3) % words.length]!,
        }),
      );
    }
  }
  const of = (lane: AtlasLane) => nodes.filter((node) => node.lane === lane);
  const edges: AtlasMapEdge[] = [];
  const chain: [AtlasLane, AtlasRelation, AtlasLane][] = [
    ["entry", "resolves_to", "runtime"],
    ["services", "runs_on", "runtime"],
    ["services", "depends_on", "data"],
    ["data", "runs_on", "runtime"],
  ];
  for (const [from, relation, to] of chain) {
    const a = of(from);
    const b = of(to);
    if (b.length === 0) continue;
    a.forEach((node, i) => edges.push(edge(node, relation, b[(i * 5) % b.length]!)));
  }
  return counted({ nodes, edges });
}

/** A review answer over `items`, counted by their kinds. */
export function reviewAnswer(items: AtlasReviewItem[]): AtlasReviewAnswer {
  const counts: AtlasReviewAnswer["counts"] = {};
  for (const item of items) counts[item.kind] = (counts[item.kind] ?? 0) + 1;
  return { ...read(), items, total: items.length, counts };
}

/**
 * What waits for a person in the grown company: the server's address two
 * sources disagree on, and a service only an agent reported.
 */
export function waitingItems(company: GrownCompany): AtlasReviewItem[] {
  const address = disputedServer(company).entry.facts[0]!;
  const guess = summary("service", "ledger", { word: "unconfirmed", state: "discovered" });
  return [
    {
      id: `${company.web1.id}/disagreement/fact/address`,
      kind: "disagreement",
      entry: company.web1,
      fact: address,
      since: "2026-11-11T10:30:00Z",
    },
    {
      id: `${guess.id}/unconfirmed`,
      kind: "unconfirmed",
      entry: guess,
      // WHAT THE AGENT SAID BEHIND IT: a message it read, which the engine
      // could check nothing behind.
      reports: [
        {
          source: {
            kind: "agent",
            name: "sre",
            turn: idFor("turn-2"),
            evidence: "none",
            tool: "read_channel",
          },
          observed_at: SEEN,
        },
      ],
      since: SEEN,
    },
  ];
}
