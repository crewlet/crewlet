/**
 * FairSplit's artifacts: what its people and agents made, of every kind, as
 * the demo engine keeps them: a doc's tabs, a deck's slides, a design's canvas
 * and artboards (each board a page under `frames/`), a dashboard's board over
 * its datasets, a page (its page under `frames/`) and a sheet's store.
 *
 * Every time is relative to the company's own `now`, so the estate always
 * reads as recent.
 */

import type {
  ArtifactActor,
  ArtifactCapability,
  ArtifactKind,
  ArtifactHistoryEntry,
  ArtifactPresenceMember,
  ArtifactRole,
  ArtifactShare,
  DataCollection,
  DataDoc,
} from "~/protocol/types.ts";
import { outlineOf, textFile, type CanvasFile } from "../../../harness/engine/artifactKinds.ts";
import type {
  DemoArtifact,
  DemoArtifactFile,
  DemoArtifactTemplate,
  DemoArtifactVersion,
  ProposedComment,
} from "../../../harness/engine/artifacts.ts";
import type { DemoCompany } from "../../../harness/engine/company.ts";
import { fixtureId } from "../../../harness/engine/ids.ts";
import {
  ATTENTION,
  ATTENTION_ROWS,
  BETA_HEALTH,
  FIGURES,
  GROUP_SIZES_CSV,
  SPRINT_DELIVERY,
  TEMPLATE_DASHBOARD,
  WEEKLY,
  weeklyRows,
} from "./dashboards.ts";
import {
  fixtureDeckFiles,
  INVESTOR_UPDATE,
  q4Roadmap,
  TEMPLATE_DECK,
  type FixtureDeck,
} from "./decks.ts";
import {
  BENCHMARKS,
  INTERVIEW_NOTES,
  INVITE_LIST,
  LAUNCH_PLAN_V2,
  PRICING_NOTES,
  SETTLEMENT_DESIGN_V1,
  SETTLEMENT_DESIGN_V3,
  TEMPLATE_BRIEF,
  TEMPLATE_NOTES,
  WEEKLY_UPDATE,
} from "./docs.ts";

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

/** The unit FairSplit's team is, by its key. */
export const TEAM = "product-and-engineering";

/** Who is who, by seat. */
const PEOPLE: Record<string, ArtifactActor> = {
  miles: { name: "miles", kind: "human" },
  priya: { name: "priya", kind: "human" },
  jonas: { name: "jonas", kind: "human" },
  pm: { name: "pm", kind: "agent" },
  engineer: { name: "engineer", kind: "agent" },
  crewlet: { name: "crewlet", kind: "system" },
};

const AGENTS = new Set(["pm", "engineer"]);

/** An artifact's id, stable across loads. */
export const artifactId = (slug: string) => fixtureId(`artifact:${slug}`);

type File = DemoArtifactFile;

const md = (path: string, text: string): File => textFile(path, text);
const jsonFile = (path: string, value: unknown): File =>
  textFile(path, `${JSON.stringify(value, null, 2)}\n`);

/** A doc of one tab, or of several with their index. */
export function docFiles(tabs: [title: string, text: string][]): File[] {
  if (tabs.length === 1) return [md("index.md", tabs[0]![1])];
  const paths = tabs.map(([title], i) =>
    i === 0 ? "index.md" : `tabs/${title.toLowerCase().replace(/[^a-z0-9]+/g, "-")}.md`,
  );
  return [
    jsonFile("doc.json", { v: 1, tabs: tabs.map(([title], i) => ({ path: paths[i], title })) }),
    ...tabs.map(([, text], i) => md(paths[i]!, text)),
  ];
}

/** One version as a spec writes it: the files it publishes, and a deck's canonical form. */
interface VersionSpec {
  ago: number;
  by: string;
  label?: string;
  files?: File[];
  deck?: FixtureDeck;
}

interface Spec {
  slug: string;
  kind: ArtifactKind;
  title: string;
  description?: string;
  owner: string;
  shares?: [ArtifactShare["audience"], ArtifactRole][];
  created: number;
  versions: VersionSpec[];
  /** The page an `html` artifact is, a file under `frames/` without its extension. */
  frame?: string;
  /** A design's artboards: each board's path in `canvas.json` to its page under `frames/`. */
  boards?: Record<string, string>;
  capabilities?: ArtifactCapability[];
  /** Milliseconds ago each seat last opened it. */
  opened?: Record<string, number>;
  pinned?: boolean;
  room?: { seat: string; activity: ArtifactPresenceMember["activity"]; region?: string }[];
  comments?: (Omit<ProposedComment, "id" | "at" | "replies"> & {
    ago: number;
    replies?: { author: string; body: string; ago: number }[];
  })[];
  work_item?: string;
  data?: {
    collections: DataCollection[];
    rows: Record<string, Record<string, unknown>[]>;
    by: string[];
  };
  trashed?: number;
}

/** The entry of each kind's version. */
function entryOf(kind: ArtifactKind, files: File[]): string {
  switch (kind) {
    case "doc":
      return files.some((f) => f.path === "doc.json") ? "doc.json" : "index.md";
    case "slides":
      return "deck.json";
    case "design":
      return "canvas.json";
    case "dashboard":
      return "dashboard.json";
    case "html":
      return "index.html";
    case "sheet":
      return "";
  }
}

/** A page artifact's one file, standing for the page under `frames/`. */
function pageFile(frame: string, title: string): File {
  return {
    path: "index.html",
    content_type: "text/html; charset=utf-8",
    text: `<!doctype html>\n<title>${title}</title>\n<!-- frames/${frame}.html -->\n`,
  };
}

const audienceKey = (a: ArtifactShare["audience"]) =>
  a.kind === "company" ? "company" : `${a.kind}:${a.id ?? ""}`;

function build(spec: Spec, now: number): DemoArtifact {
  const id = artifactId(spec.slug);
  const at = (ago: number) => new Date(now - ago).toISOString();
  const versions: DemoArtifactVersion[] = spec.versions.map((v, i) => {
    const files = v.deck
      ? fixtureDeckFiles(v.deck)
      : (v.files ?? (spec.kind === "html" ? [pageFile(spec.frame ?? "", spec.title)] : []));
    return {
      number: i + 1,
      ...(v.label ? { label: v.label } : {}),
      created_at: at(v.ago),
      author: PEOPLE[v.by]!,
      capabilities: spec.capabilities ?? [],
      entry: entryOf(spec.kind, files),
      files,
      ...(spec.frame ? { frame: `frames/${spec.frame}.html` } : {}),
      ...(spec.boards
        ? {
            boards: Object.fromEntries(
              Object.entries(spec.boards).map(([path, page]) => [path, `frames/${page}.html`]),
            ),
          }
        : {}),
    };
  });
  const owner = spec.owner;
  const last = spec.versions[spec.versions.length - 1];
  const history: ArtifactHistoryEntry[] = [];
  let pos = 0;
  const entry = (
    op: string,
    ago: number,
    by: string,
    summary: ArtifactHistoryEntry["summary"],
    number?: number,
  ) =>
    history.push({
      position: ++pos,
      op_id: fixtureId(`${id}:h${pos}`),
      op,
      outcome: "applied",
      ...(number !== undefined ? { number } : {}),
      actor: PEOPLE[by]!,
      origin: AGENTS.has(by) ? "agent" : "dashboard",
      ...(spec.work_item ? { work_item: spec.work_item } : {}),
      at: at(ago),
      summary,
    });
  entry("create", spec.created, owner, {});
  versions.forEach((v, i) =>
    entry(
      "publish",
      spec.versions[i]!.ago,
      spec.versions[i]!.by,
      { files: v.files.length, ...(v.label ? { label: v.label } : {}) },
      v.number,
    ),
  );
  const shares: ArtifactShare[] = (spec.shares ?? []).map(([audience, role], i) => {
    const grantedAgo = Math.max(0, spec.created - (i + 1) * 20 * MIN);
    entry("share", grantedAgo, owner, { audience: audienceKey(audience), role });
    return { audience, role, granted_by: PEOPLE[owner]!, granted_at: at(grantedAgo) };
  });
  history.sort((a, b) => b.at.localeCompare(a.at));

  let data: DemoArtifact["data"];
  if (spec.data) {
    const docs: DataDoc[] = [];
    for (const coll of spec.data.collections) {
      for (const fields of spec.data.rows[coll.name] ?? []) {
        const by = PEOPLE[spec.data.by[docs.length % spec.data.by.length]!]!;
        const when = at(Math.max(0, spec.created - (docs.length + 1) * 37 * MIN));
        docs.push({
          collection: coll.name,
          id: `row${String(docs.length + 1).padStart(3, "0")}`,
          fields,
          version: docs.length + 1,
          created_by: by,
          created_at: when,
          updated_by: by,
          updated_at: when,
        });
      }
    }
    data = { collections: spec.data.collections, docs, history: [], version: docs.length };
  }

  return {
    id,
    kind: spec.kind,
    title: spec.title,
    ...(spec.description ? { description: spec.description } : {}),
    owner,
    owner_kind: AGENTS.has(owner) ? "agent" : "human",
    // MILES LEADS THE TEAM'S AGENTS, so he holds owner on what they make.
    leads: AGENTS.has(owner) ? ["miles"] : [],
    status: spec.trashed !== undefined ? "trashed" : "live",
    ...(spec.trashed !== undefined
      ? { trashed_by: PEOPLE[owner]!, trashed_at: at(spec.trashed) }
      : {}),
    shares,
    created_by: PEOPLE[owner]!,
    updated_by: PEOPLE[last?.by ?? owner]!,
    created_at: at(spec.created),
    updated_at: at(last?.ago ?? spec.created),
    versions,
    history,
    opened: Object.fromEntries(Object.entries(spec.opened ?? {}).map(([s, ago]) => [s, at(ago)])),
    pinnedBy: spec.pinned ? ["miles"] : [],
    room: (spec.room ?? []).map((m, i) => ({
      session: `session-${spec.slug}-${i}`,
      seat: m.seat,
      kind: AGENTS.has(m.seat) ? "agent" : "human",
      activity: m.activity,
      focus: { version: versions.length, ...(m.region ? { region: m.region } : {}) },
      at: at(0),
    })),
    comments: (spec.comments ?? []).map((t, i) => ({
      id: fixtureId(`${id}:c${i}`),
      author: t.author,
      body: t.body,
      at: at(t.ago),
      version: t.version,
      ...(t.anchor ? { anchor: t.anchor } : {}),
      ...(t.resolved ? { resolved: true } : {}),
      replies: (t.replies ?? []).map((r, k) => ({
        id: fixtureId(`${id}:c${i}:r${k}`),
        author: r.author,
        body: r.body,
        at: at(r.ago),
      })),
    })),
    ...(spec.work_item ? { work_item: spec.work_item } : {}),
    ...(data ? { data } : {}),
  };
}

const company: ArtifactShare["audience"] = { kind: "company" };
const team: ArtifactShare["audience"] = { kind: "unit", id: TEAM };
const seat = (id: string): ArtifactShare["audience"] => ({ kind: "seat", id });

/** A design's canvas: its boards where the author put them, its notes, and its cover board. */
function canvas(
  boards: [path: string, title: string, x: number, y: number, w: number, h: number][],
  notes: CanvasFile["notes"],
  cover: string,
): File {
  const index: CanvasFile = {
    v: 1,
    boards: Object.fromEntries(
      boards.map(([path, title, x, y, w, h]) => [path, { x, y, w, h, title }]),
    ),
    order: boards.map(([path]) => path),
    cover,
    notes,
  };
  return jsonFile("canvas.json", index);
}

const PHONE = 360;
const TALL = 740;

const MOBILE_CANVAS = canvas(
  [
    ["boards/group.html", "Group", 40, 60, PHONE, TALL],
    ["boards/add-expense.html", "Add expense", 440, 60, PHONE, TALL],
    ["boards/split.html", "Split", 840, 60, PHONE, TALL],
    ["boards/settle-up.html", "Settle up", 1240, 60, PHONE, TALL],
  ],
  [
    {
      id: "title",
      x: 40,
      y: -140,
      w: 1200,
      h: 80,
      kind: "title",
      text: "Recording and settling up from a phone",
    },
    {
      id: "sticky",
      x: 1640,
      y: 60,
      w: 260,
      h: 180,
      kind: "sticky",
      color: "yellow",
      text: "Settle up names the leftover cent before anyone pays (PM's note, resolved in version 3).",
    },
  ],
  "boards/settle-up.html",
);

const MOBILE_BOARDS = {
  "boards/group.html": "artboard-mobile-companion-group",
  "boards/add-expense.html": "artboard-mobile-companion-add-expense",
  "boards/split.html": "artboard-mobile-companion-split",
  "boards/settle-up.html": "artboard-mobile-companion-settle-up",
};

const STATES_CANVAS = canvas(
  [
    ["boards/nothing-to-settle.html", "Nothing to settle", 40, 60, PHONE, 520],
    ["boards/payments-due.html", "Payments due", 440, 60, PHONE, 520],
    ["boards/offline.html", "Offline", 840, 60, PHONE, 520],
  ],
  [
    {
      id: "title",
      x: 40,
      y: -120,
      w: 1100,
      h: 80,
      kind: "title",
      text: "Settle up, in its three states",
    },
  ],
  "boards/payments-due.html",
);

const STATES_BOARDS = {
  "boards/nothing-to-settle.html": "artboard-settle-states-nothing-to-settle",
  "boards/payments-due.html": "artboard-settle-states-payments-due",
  "boards/offline.html": "artboard-settle-states-offline",
};

// ---- the sheets' schemas ---------------------------------------------------

const FEEDBACK: DataCollection = {
  name: "feedback",
  title: "Feedback",
  preset: "contribute",
  read: "all",
  create: "contributor",
  delete: "editor",
  columns: [
    { key: "group", type: "text", label: "Group", primary: true, required: true },
    { key: "summary", type: "longtext", label: "What they said" },
    {
      key: "theme",
      type: "select",
      label: "Theme",
      options: [
        { id: "rounding", label: "Rounding", color: "amber" },
        { id: "reminders", label: "Reminders", color: "blue" },
        { id: "currency", label: "Currency", color: "purple" },
        { id: "recording", label: "Recording", color: "green" },
        { id: "settling", label: "Settling up", color: "cyan" },
      ],
    },
    {
      key: "status",
      type: "status",
      label: "Status",
      options: [
        { id: "open", label: "Open", category: "todo" },
        { id: "planned", label: "Planned", category: "in_progress" },
        { id: "fixed", label: "Fixed", category: "done" },
        { id: "wont", label: "Will not do", category: "blocked" },
      ],
    },
    { key: "owner", type: "person", label: "Owner" },
    { key: "votes", type: "number", label: "Groups asking", min: 0 },
    { key: "reported", type: "date", label: "Reported" },
  ],
};

const PARTNERS: DataCollection = {
  name: "partners",
  title: "Payment partners",
  preset: "shared",
  read: "all",
  create: "editor",
  delete: "editor",
  columns: [
    { key: "provider", type: "text", label: "Provider", primary: true, required: true },
    { key: "fee", type: "text", label: "Fee per payout" },
    { key: "payout", type: "number", label: "Days to payout", min: 0 },
    {
      key: "regions",
      type: "multiselect",
      label: "Regions",
      options: [
        { id: "eu", label: "EU", color: "blue" },
        { id: "uk", label: "UK", color: "purple" },
        { id: "us", label: "US", color: "green" },
      ],
    },
    {
      key: "stage",
      type: "status",
      label: "Stage",
      options: [
        { id: "research", label: "Researching", category: "todo" },
        { id: "talking", label: "In talks", category: "in_progress" },
        { id: "chosen", label: "Shortlisted", category: "done" },
        { id: "out", label: "Ruled out", category: "blocked" },
      ],
    },
    { key: "rating", type: "rating", label: "Fit", max: 5 },
  ],
};

const day = (now: number, ago: number) => new Date(now - ago * DAY).toISOString().slice(0, 10);

/** The beta groups' requests, as the feedback sheet holds them. */
function feedbackRows(now: number): Record<string, unknown>[] {
  return [
    {
      group: "Lisbon trip",
      summary: "Who takes the leftover cent should be said before anyone pays.",
      theme: "rounding",
      status: "fixed",
      owner: "engineer",
      votes: 9,
      reported: day(now, 11),
    },
    {
      group: "Flat 4B",
      summary: "A reminder the day before rent is split.",
      theme: "reminders",
      status: "planned",
      owner: "pm",
      votes: 6,
      reported: day(now, 10),
    },
    {
      group: "Climbing club",
      summary: "Expenses in euros and pounds on one trip.",
      theme: "currency",
      status: "open",
      owner: "pm",
      votes: 4,
      reported: day(now, 9),
    },
    {
      group: "Book club",
      summary: "Record a shared order from a phone at the till.",
      theme: "recording",
      status: "planned",
      owner: "priya",
      votes: 7,
      reported: day(now, 8),
    },
    {
      group: "Lisbon trip",
      summary: "Settle up asked for six payments where three would do.",
      theme: "settling",
      status: "fixed",
      owner: "engineer",
      votes: 5,
      reported: day(now, 8),
    },
    {
      group: "Flat 2A",
      summary: "Pin rent to the leaseholder before planning the rest.",
      theme: "settling",
      status: "open",
      owner: "engineer",
      votes: 3,
      reported: day(now, 6),
    },
    {
      group: "Running crew",
      summary: "Rounding showed 0.01 owed after everyone paid.",
      theme: "rounding",
      status: "fixed",
      owner: "engineer",
      votes: 2,
      reported: day(now, 5),
    },
    {
      group: "Studio share",
      summary: "Monthly subscription split automatically.",
      theme: "recording",
      status: "wont",
      owner: "pm",
      votes: 1,
      reported: day(now, 4),
    },
    {
      group: "Ski chalet",
      summary: "Split by nights stayed, not evenly.",
      theme: "settling",
      status: "planned",
      owner: "engineer",
      votes: 6,
      reported: day(now, 3),
    },
    {
      group: "Flat 4B",
      summary: "Show the balance without opening a laptop.",
      theme: "recording",
      status: "planned",
      owner: "priya",
      votes: 8,
      reported: day(now, 2),
    },
    {
      group: "Climbing club",
      summary: "Export a trip's expenses for the club treasurer.",
      theme: "recording",
      status: "open",
      owner: "pm",
      votes: 2,
      reported: day(now, 1),
    },
  ];
}

/** The payment providers Jonas is weighing. */
const PARTNER_ROWS: Record<string, unknown>[] = [
  {
    provider: "Northbank Payouts",
    fee: "€0.20 + 0.4%",
    payout: 1,
    regions: ["eu", "uk"],
    stage: "talking",
    rating: 4,
  },
  {
    provider: "Tilly Pay",
    fee: "€0.35 flat",
    payout: 2,
    regions: ["eu"],
    stage: "chosen",
    rating: 5,
  },
  {
    provider: "Carrow Money",
    fee: "0.9%",
    payout: 3,
    regions: ["eu", "uk", "us"],
    stage: "research",
    rating: 3,
  },
  {
    provider: "Ledgerline",
    fee: "€0.10 + 1.2%",
    payout: 1,
    regions: ["uk"],
    stage: "out",
    rating: 2,
  },
  {
    provider: "Parcel Finance",
    fee: "€0.25 + 0.5%",
    payout: 2,
    regions: ["eu", "us"],
    stage: "research",
    rating: 3,
  },
];

// ---- the estate ------------------------------------------------------------

function specs(now: number): Spec[] {
  return [
    {
      slug: "q4-roadmap",
      kind: "slides",
      title: "Q4 roadmap review",
      description: "The quarter's three bets, what they cost and the decisions they wait on.",
      owner: "pm",
      shares: [
        [team, "editor"],
        [company, "viewer"],
      ],
      created: 6 * DAY,
      capabilities: ["presence"],
      versions: [
        { ago: 6 * DAY, by: "pm", label: "First draft", deck: q4Roadmap(1) },
        { ago: 4 * DAY, by: "pm", label: "Added the three bets", deck: q4Roadmap(2) },
        { ago: 26 * HOUR, by: "pm", label: "Costs and decisions", deck: q4Roadmap(3) },
        { ago: 2 * HOUR, by: "pm", label: "Beta numbers for week 41", deck: q4Roadmap(4) },
      ],
      opened: { miles: 2 * DAY, engineer: 3 * HOUR },
      pinned: true,
      room: [
        { seat: "pm", activity: "editing", region: "slide:s8" },
        { seat: "engineer", activity: "viewing", region: "slide:s5" },
      ],
      comments: [
        {
          author: "miles",
          body: "Can we decide storage before Thursday's review? I lean towards one SQLite file.",
          version: 3,
          anchor: "slide:s10",
          ago: 20 * HOUR,
          replies: [
            {
              author: "pm",
              body: "Yes. Both options and their migration cost are now on slide 6.",
              ago: 3 * HOUR,
            },
          ],
        },
        {
          author: "engineer",
          body: "FAIR-4 is done, so the settle-in-one-step slide can say it ships a week earlier.",
          version: 4,
          anchor: "slide:s5",
          ago: 90 * MIN,
        },
      ],
      work_item: "FAIR-1",
    },
    {
      slug: "investor-update",
      kind: "slides",
      title: "Investor update, October",
      description: "The month in numbers, highlights and where we need introductions.",
      owner: "miles",
      shares: [[seat("priya"), "commenter"]],
      created: 3 * DAY,
      capabilities: ["presence"],
      versions: [
        { ago: 3 * DAY, by: "miles", label: "Outline", deck: INVESTOR_UPDATE },
        { ago: 5 * HOUR, by: "miles", label: "October numbers", deck: INVESTOR_UPDATE },
      ],
      opened: { miles: 5 * HOUR, priya: 4 * HOUR },
      comments: [
        {
          author: "priya",
          body: "The quote from the climbing club lands better as the closing slide.",
          version: 2,
          anchor: "slide:u5",
          ago: 4 * HOUR,
        },
      ],
    },
    {
      slug: "mobile-companion",
      kind: "design",
      title: "Mobile companion concept",
      description: "Recording, splitting and settling up from a phone, on the same ledger.",
      owner: "priya",
      shares: [
        [seat("miles"), "editor"],
        [team, "commenter"],
      ],
      created: 5 * DAY,
      boards: MOBILE_BOARDS,
      capabilities: ["presence"],
      versions: [
        { ago: 5 * DAY, by: "priya", label: "Group and add expense", files: [MOBILE_CANVAS] },
        { ago: 2 * DAY, by: "priya", label: "Split screen", files: [MOBILE_CANVAS] },
        {
          ago: 40 * MIN,
          by: "priya",
          label: "Settle up names the leftover cent",
          files: [MOBILE_CANVAS],
        },
      ],
      opened: { miles: 3 * DAY, pm: 2 * HOUR },
      room: [{ seat: "priya", activity: "editing", region: "board:boards/settle-up.html" }],
      comments: [
        {
          author: "pm",
          body: "Settle up should say who takes the leftover cent before anyone pays. The beta's rounding disputes all start there.",
          version: 2,
          anchor: "board:boards/settle-up.html",
          ago: 30 * HOUR,
          resolved: true,
          replies: [
            {
              author: "priya",
              body: "Done in version 3: a note under the payments.",
              ago: 40 * MIN,
            },
          ],
        },
        {
          author: "miles",
          body: "Can Add expense remember the last split a group used?",
          version: 3,
          anchor: "board:boards/add-expense.html",
          ago: 20 * MIN,
        },
      ],
    },
    {
      slug: "settle-states",
      kind: "design",
      title: "Settle-up screen states",
      description: "Nothing to settle, payments due and offline.",
      owner: "engineer",
      shares: [[team, "viewer"]],
      created: 30 * HOUR,
      boards: STATES_BOARDS,
      versions: [{ ago: 30 * HOUR, by: "engineer", files: [STATES_CANVAS] }],
      opened: { miles: 28 * HOUR },
      work_item: "FAIR-3",
    },
    {
      slug: "beta-health",
      kind: "dashboard",
      title: "Beta health",
      description: "Groups, expenses and how quickly balances settle, refreshed every morning.",
      owner: "pm",
      shares: [[company, "viewer"]],
      created: 9 * DAY,
      versions: [
        {
          ago: 9 * DAY,
          by: "pm",
          label: "Weekly figures",
          files: [
            jsonFile("dashboard.json", {
              ...BETA_HEALTH,
              widgets: BETA_HEALTH.widgets.slice(0, 5),
            }),
            csv(),
          ],
        },
        {
          ago: 8 * DAY,
          by: "pm",
          label: "Added groups by size",
          files: [
            jsonFile("dashboard.json", {
              ...BETA_HEALTH,
              widgets: BETA_HEALTH.widgets.slice(0, 6),
            }),
            csv(),
          ],
        },
        {
          ago: 14 * HOUR,
          by: "pm",
          label: "Groups needing a look",
          files: [jsonFile("dashboard.json", BETA_HEALTH), csv()],
        },
      ],
      opened: { miles: 20 * HOUR, priya: 2 * DAY, jonas: 3 * DAY },
      pinned: true,
      data: {
        collections: [WEEKLY, ATTENTION],
        rows: { weekly: weeklyRows(now), attention: ATTENTION_ROWS },
        by: ["pm"],
      },
    },
    {
      slug: "sprint-delivery",
      kind: "dashboard",
      title: "Sprint delivery",
      description:
        "FAIR tasks by status and assignee, and what the agents spent, read live from Crewlet.",
      owner: "engineer",
      shares: [[team, "viewer"]],
      created: 4 * DAY,
      versions: [
        {
          ago: 4 * DAY,
          by: "engineer",
          files: [
            jsonFile("dashboard.json", {
              ...SPRINT_DELIVERY,
              widgets: SPRINT_DELIVERY.widgets.slice(0, 4),
            }),
          ],
        },
        {
          ago: 50 * MIN,
          by: "engineer",
          label: "Tasks by assignee",
          files: [jsonFile("dashboard.json", SPRINT_DELIVERY)],
        },
      ],
      opened: { miles: 2 * DAY },
    },
    {
      slug: "split-calculator",
      kind: "html",
      title: "Split calculator",
      description: "Try an amount against the leftover-cent rule.",
      owner: "engineer",
      shares: [[company, "viewer"]],
      created: 3 * DAY,
      frame: "page-split-calculator",
      versions: [
        { ago: 3 * DAY, by: "engineer" },
        { ago: 22 * HOUR, by: "engineer", label: "The payer can take the leftover" },
      ],
      opened: { miles: 21 * HOUR, priya: 5 * HOUR },
      work_item: "FAIR-4",
    },
    {
      slug: "settle-explainer",
      kind: "html",
      title: "Fewest payments, explained",
      description: "The Lisbon trip's balances and the payments that settle them.",
      owner: "engineer",
      shares: [[team, "viewer"]],
      created: 9 * HOUR,
      frame: "page-settle-explainer",
      versions: [{ ago: 9 * HOUR, by: "engineer" }],
      work_item: "FAIR-3",
    },
    {
      slug: "settlement-design",
      kind: "doc",
      title: "Settlement engine design",
      description: "How the engine picks the fewest payments, and the leftover cent.",
      owner: "engineer",
      shares: [
        [team, "commenter"],
        [company, "viewer"],
      ],
      created: 8 * DAY,
      versions: [
        {
          ago: 8 * DAY,
          by: "engineer",
          label: "First pass",
          files: docFiles([["Design", SETTLEMENT_DESIGN_V1]]),
        },
        {
          ago: 3 * DAY,
          by: "engineer",
          label: "Subset search",
          files: docFiles([["Design", SETTLEMENT_DESIGN_V3]]),
        },
        {
          ago: 3 * HOUR,
          by: "engineer",
          label: "Benchmarks",
          files: docFiles([
            ["Design", SETTLEMENT_DESIGN_V3],
            ["Benchmarks", BENCHMARKS],
          ]),
        },
      ],
      opened: { miles: 3 * DAY, pm: 2 * HOUR },
      comments: [
        {
          author: "pm",
          body: "Pinned payments would help flats: rent always goes to the leaseholder. Worth a task?",
          version: 3,
          anchor: "tab:index.md#open-questions",
          ago: 2 * HOUR,
        },
      ],
      work_item: "FAIR-3",
    },
    {
      slug: "launch-plan",
      kind: "doc",
      title: "Beta launch plan",
      description: "What ships on 4 November, who is invited and what we measure.",
      owner: "pm",
      shares: [[company, "commenter"]],
      created: 5 * DAY,
      versions: [
        {
          ago: 5 * DAY,
          by: "pm",
          files: docFiles([["Plan", LAUNCH_PLAN_V2.split("## Risks")[0]!]]),
        },
        {
          ago: 31 * HOUR,
          by: "pm",
          label: "Risks and the invite list",
          files: docFiles([
            ["Plan", LAUNCH_PLAN_V2],
            ["Invite list", INVITE_LIST],
          ]),
        },
      ],
      opened: { miles: 30 * HOUR, jonas: 6 * HOUR },
      pinned: true,
      work_item: "FAIR-1",
    },
    {
      slug: "weekly-update",
      kind: "doc",
      title: "Weekly update, week 41",
      description: "Done, in progress and waiting on Miles.",
      owner: "pm",
      shares: [[company, "viewer"]],
      created: 18 * HOUR,
      versions: [{ ago: 18 * HOUR, by: "pm", files: docFiles([["Update", WEEKLY_UPDATE]]) }],
    },
    {
      slug: "interview-notes",
      kind: "doc",
      title: "Interview notes, Flat 4B",
      description: "Three flatmates, two years of shared bills.",
      owner: "priya",
      shares: [[seat("miles"), "commenter"]],
      created: 6 * DAY,
      versions: [{ ago: 6 * DAY, by: "priya", files: docFiles([["Notes", INTERVIEW_NOTES]]) }],
      opened: { miles: 5 * DAY },
    },
    {
      slug: "beta-feedback",
      kind: "sheet",
      title: "Beta feedback",
      description: "What beta groups asked for, by theme, and who owns each answer.",
      owner: "miles",
      shares: [[team, "contributor"]],
      created: 12 * DAY,
      versions: [],
      opened: { miles: 26 * HOUR, pm: 1 * HOUR },
      pinned: true,
      room: [{ seat: "pm", activity: "editing", region: "Status" }],
      data: {
        collections: [FEEDBACK],
        rows: { feedback: feedbackRows(now) },
        by: ["miles", "pm", "pm", "engineer"],
      },
    },
    {
      slug: "payment-partners",
      kind: "sheet",
      title: "Payment partners",
      description: "Who could move money for in-app settle-up, and what they charge.",
      owner: "jonas",
      shares: [[seat("miles"), "editor"]],
      created: 4 * DAY,
      versions: [],
      opened: { miles: 4 * DAY },
      data: { collections: [PARTNERS], rows: { partners: PARTNER_ROWS }, by: ["jonas"] },
    },
    {
      slug: "pricing-notes",
      kind: "doc",
      title: "Pricing notes, draft",
      owner: "miles",
      created: 10 * DAY,
      versions: [{ ago: 10 * DAY, by: "miles", files: docFiles([["Notes", PRICING_NOTES]]) }],
      trashed: 3 * DAY,
    },
  ];
}

function csv(): File {
  return { path: "data/group-sizes.csv", content_type: "text/csv", text: GROUP_SIZES_CSV };
}

/**
 * The starting points New offers (D64's section 5.1): a doc, a deck, a
 * dashboard and a sheet, each copied from a source nobody else can see. A page
 * and a design start from an upload or an agent, so they have no template.
 */
function templateSpecs(): (Spec & { template: { title: string; description: string } })[] {
  const t = (
    slug: string,
    kind: ArtifactKind,
    title: string,
    description: string,
    rest: Partial<Spec>,
  ) => ({
    slug: `template-${slug}`,
    kind,
    title,
    owner: "crewlet",
    created: 30 * DAY,
    versions: [],
    ...rest,
    template: { title, description },
  });
  return [
    t("brief", "doc", "Project brief", "Why, what, how you will know, and what is open.", {
      versions: [{ ago: 30 * DAY, by: "crewlet", files: docFiles([["Brief", TEMPLATE_BRIEF]]) }],
    }),
    t("notes", "doc", "Meeting notes", "Decisions and actions, each with an owner.", {
      versions: [{ ago: 30 * DAY, by: "crewlet", files: docFiles([["Notes", TEMPLATE_NOTES]]) }],
    }),
    t("deck", "slides", "Review deck", "A title, three points and the numbers.", {
      versions: [{ ago: 30 * DAY, by: "crewlet", deck: TEMPLATE_DECK }],
    }),
    t(
      "kpis",
      "dashboard",
      "KPI dashboard",
      "A figure over a chart, from rows you or an agent write.",
      {
        versions: [
          { ago: 30 * DAY, by: "crewlet", files: [jsonFile("dashboard.json", TEMPLATE_DASHBOARD)] },
        ],
        data: {
          collections: [FIGURES],
          rows: { figures: [{ period: "2026-10-05", value: 0 }] },
          by: ["crewlet"],
        },
      },
    ),
    t("tracker", "sheet", "Tracker", "Items with a status, an owner and a date.", {
      data: {
        collections: [
          {
            ...FEEDBACK,
            name: "items",
            title: "Items",
            columns: FEEDBACK.columns!.filter((c) =>
              ["group", "status", "owner", "reported"].includes(c.key),
            ).map((c) => (c.key === "group" ? { ...c, label: "Item" } : c)),
          },
        ],
        rows: {},
        by: ["crewlet"],
      },
    }),
    t("decisions", "sheet", "Decision log", "What was decided, by whom, and why.", {
      data: {
        collections: [
          {
            name: "decisions",
            title: "Decisions",
            read: "all",
            create: "contributor",
            delete: "editor",
            columns: [
              { key: "decision", type: "text", label: "Decision", primary: true },
              { key: "why", type: "longtext", label: "Why" },
              { key: "owner", type: "person", label: "Owner" },
              { key: "date", type: "date", label: "Date" },
            ],
          },
        ],
        rows: {},
        by: ["crewlet"],
      },
    }),
  ];
}

/** The estate, as a scenario step: every artifact and template, relative to the company's now. */
export function withArtifacts(c: DemoCompany): DemoCompany {
  const now = Date.parse(c.now);
  const out = structuredClone(c);
  const templates = templateSpecs();
  const items = [...specs(now), ...templates].map((spec) => build(spec, now));
  out.artifacts = {
    items,
    templates: templates.map((spec) => {
      const source = items.find((a) => a.id === artifactId(spec.slug))!;
      const top = source.versions[source.versions.length - 1];
      return {
        id: spec.slug,
        kind: spec.kind,
        ...spec.template,
        outline: outlineOf(source, top),
        from: artifactId(spec.slug),
      } satisfies DemoArtifactTemplate;
    }),
    uploads: {},
    seq: 100,
  };
  return out;
}

/** No artifacts at all, but the templates: the first day. */
export function noArtifacts(c: DemoCompany): DemoCompany {
  const out = withArtifacts(c);
  out.artifacts.items = out.artifacts.items.filter((a) => a.owner === "crewlet");
  return out;
}
