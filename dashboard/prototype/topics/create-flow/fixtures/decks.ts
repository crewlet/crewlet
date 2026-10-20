/**
 * The decks, as the engine's slide parser leaves them (D56): each slide a
 * canonical tree on the 1920 by 1080 stage, with its speaker notes. A deck's
 * colours are its author's, in the format's `#rrggbb`, so a deck looks the
 * same whatever the reader's theme, as a deck does on a projector.
 */

import type { DemoArtifactFile } from "../../../harness/engine/artifacts.ts";
import { deckFiles } from "../../../harness/engine/artifactKinds.ts";

/** One element of a slide as these fixtures write it: a tag, its style, its children. */
export interface SlideNode {
  t: string;
  s?: Record<string, string | number>;
  c?: (SlideNode | string)[];
}

/** A deck as these fixtures write it: its sections and every slide's tree and notes. */
export interface FixtureDeck {
  title: string;
  cover: string;
  sections: { title: string; slides: string[] }[];
  slides: { id: string; title: string; notes: string; tree: SlideNode }[];
}

/** The style properties a number is written bare for; every other number is pixels. */
const UNITLESS = new Set([
  "fontWeight",
  "lineHeight",
  "opacity",
  "flex",
  "flexGrow",
  "flexShrink",
  "zIndex",
  "order",
]);

const escapeText = (t: string) => t.replace(/[&<>]/g, (c) => `&#${c.charCodeAt(0)};`);

function styleOf(s: SlideNode["s"]): string {
  if (!s) return "";
  const decls = Object.entries(s).map(
    ([k, v]) =>
      `${k.replace(/[A-Z]/g, (m) => `-${m.toLowerCase()}`)}:${typeof v === "number" && !UNITLESS.has(k) ? `${v}px` : v}`,
  );
  return decls.length > 0 ? ` style="${decls.join(";").replace(/"/g, "&quot;")}"` : "";
}

function markup(n: SlideNode | string): string {
  if (typeof n === "string") return escapeText(n);
  return `<${n.t}${styleOf(n.s)}>${(n.c ?? []).map(markup).join("")}</${n.t}>`;
}

/**
 * A fixture deck as the files the slide format holds: `deck.json`, and each
 * slide's section with its id and its notes as the section's last child.
 */
export function fixtureDeckFiles(deck: FixtureDeck): DemoArtifactFile[] {
  const order = deck.slides.map((s) => s.id);
  return deckFiles(
    {
      v: 4,
      title: deck.title,
      order,
      cover: deck.cover,
      sections: Object.fromEntries(
        deck.sections
          .map((sec) => ({
            description: sec.title,
            start: sec.slides.find((id) => order.includes(id)),
          }))
          .filter((sec): sec is { description: string; start: string } => sec.start !== undefined)
          .map((sec, i) => [`s${i + 1}`, sec]),
      ),
    },
    deck.slides.map((slide) => {
      const root = slide.tree;
      const inner = (root.c ?? []).map(markup).join("\n");
      const notes = slide.notes ? `\n<aside>${escapeText(slide.notes)}</aside>` : "";
      return [
        slide.id,
        `<section id="${slide.id}"${styleOf(root.s)}>\n${inner}${notes}\n</section>\n`,
      ];
    }),
  );
}

interface Palette {
  ground: string;
  ink: string;
  quiet: string;
  accent: string;
  panel: string;
}

const NAVY: Palette = {
  ground: "#14213d",
  ink: "#f4f6fb",
  quiet: "#a9b4cc",
  accent: "#f2b84b",
  panel: "#1d2d52",
};

const PAPER: Palette = {
  ground: "#ffffff",
  ink: "#1b2333",
  quiet: "#5b6475",
  accent: "#0f766e",
  panel: "#eef2f5",
};

const PLAIN: Palette = {
  ground: "#fafafa",
  ink: "#202124",
  quiet: "#5f6368",
  accent: "#3b5bdb",
  panel: "#eceff3",
};

type Body = (p: Palette) => (SlideNode | string)[];

/** One slide's frame: the ground, the padding, the stack, and the deck's footer. */
function frame(
  p: Palette,
  deck: string,
  n: number,
  count: number,
  body: (SlideNode | string)[],
): SlideNode {
  return {
    t: "section",
    s: {
      width: 1920,
      height: 1080,
      background: p.ground,
      color: p.ink,
      padding: "120px 160px",
      display: "flex",
      flexDirection: "column",
      justifyContent: "center",
      gap: 40,
      position: "relative",
    },
    c: [
      ...body,
      {
        t: "div",
        s: {
          position: "absolute",
          left: 160,
          right: 160,
          bottom: 64,
          display: "flex",
          justifyContent: "space-between",
          color: p.quiet,
          fontSize: 24,
        },
        c: [
          { t: "span", c: [deck] },
          { t: "span", c: [`${n} / ${count}`] },
        ],
      },
    ],
  };
}

const kicker =
  (text: string): Body =>
  (p) => [
    {
      t: "p",
      s: {
        color: p.accent,
        fontSize: 30,
        fontWeight: 600,
        letterSpacing: "3px",
        textTransform: "uppercase",
        margin: 0,
      },
      c: [text],
    },
  ];

const heading =
  (text: string, size = 84): Body =>
  (p) => [
    {
      t: "h2",
      s: { fontSize: size, fontWeight: 700, lineHeight: 1.1, margin: 0, color: p.ink },
      c: [text],
    },
  ];

const lead =
  (text: string): Body =>
  (p) => [
    {
      t: "p",
      s: { fontSize: 44, lineHeight: 1.35, color: p.quiet, margin: 0, maxWidth: 1400 },
      c: [text],
    },
  ];

const bullets =
  (items: string[]): Body =>
  (p) => [
    {
      t: "ul",
      s: {
        display: "flex",
        flexDirection: "column",
        gap: 28,
        fontSize: 42,
        lineHeight: 1.35,
        color: p.ink,
        margin: 0,
        paddingLeft: 48,
      },
      c: items.map((i): SlideNode => ({ t: "li", c: [i] })),
    },
  ];

const stats =
  (items: [string, string][]): Body =>
  (p) => [
    {
      t: "div",
      s: {
        display: "grid",
        gridTemplateColumns: `repeat(${items.length}, 1fr)`,
        gap: 48,
        marginTop: 24,
      },
      c: items.map(([value, label]): SlideNode => ({
        t: "div",
        s: { display: "flex", flexDirection: "column", gap: 12 },
        c: [
          {
            t: "span",
            s: { fontSize: 132, fontWeight: 700, color: p.accent, lineHeight: 1 },
            c: [value],
          },
          { t: "span", s: { fontSize: 32, color: p.quiet }, c: [label] },
        ],
      })),
    },
  ];

const columns =
  (items: [string, string][]): Body =>
  (p) => [
    {
      t: "div",
      s: {
        display: "grid",
        gridTemplateColumns: `repeat(${items.length}, 1fr)`,
        gap: 40,
        marginTop: 16,
      },
      c: items.map(([head, text]): SlideNode => ({
        t: "div",
        s: {
          background: p.panel,
          borderRadius: 24,
          padding: 48,
          display: "flex",
          flexDirection: "column",
          gap: 20,
        },
        c: [
          { t: "h3", s: { fontSize: 40, fontWeight: 700, margin: 0, color: p.ink }, c: [head] },
          { t: "p", s: { fontSize: 32, lineHeight: 1.4, margin: 0, color: p.quiet }, c: [text] },
        ],
      })),
    },
  ];

const quote =
  (text: string, by: string): Body =>
  (p): SlideNode[] => [
    {
      t: "p",
      s: {
        fontSize: 72,
        lineHeight: 1.25,
        fontWeight: 600,
        margin: 0,
        color: p.ink,
        maxWidth: 1500,
      },
      c: [`“${text}”`],
    },
    { t: "p", s: { fontSize: 34, color: p.quiet, margin: 0 }, c: [by] },
  ];

interface SlideSpec {
  id: string;
  title: string;
  notes: string;
  body: Body[];
}

function deck(
  title: string,
  footer: string,
  palette: Palette,
  sections: { title: string; slides: SlideSpec[] }[],
  keep: (id: string) => boolean = () => true,
): FixtureDeck {
  sections = sections
    .map((s) => ({ ...s, slides: s.slides.filter((x) => keep(x.id)) }))
    .filter((s) => s.slides.length > 0);
  const all = sections.flatMap((s) => s.slides);
  return {
    title,
    cover: all[0]!.id,
    sections: sections.map((s) => ({ title: s.title, slides: s.slides.map((x) => x.id) })),
    slides: all.map((s, i) => ({
      id: s.id,
      title: s.title,
      notes: s.notes,
      tree: frame(
        palette,
        footer,
        i + 1,
        all.length,
        s.body.flatMap((b) => b(palette)),
      ),
    })),
  };
}

/** Which slides each version of the Q4 roadmap review holds: the draft grew a section at a time. */
const Q4_SLIDES: Record<number, string[]> = {
  1: ["s1", "s3", "s4", "s10"],
  2: ["s1", "s3", "s4", "s5", "s6", "s7", "s10"],
  3: ["s1", "s3", "s4", "s5", "s6", "s7", "s8", "s9", "s10"],
  4: ["s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9", "s10"],
};

/** The Q4 roadmap review as its version `n` published it. */
export function q4Roadmap(n: 1 | 2 | 3 | 4): FixtureDeck {
  const weekFigures: [string, string][] = [
    ["48", "groups recording expenses"],
    ["92%", "of balances settled within a week"],
    ["1.6", "payments per settle-up"],
  ];
  return deck(
    "Q4 roadmap review",
    "FairSplit, Q4 roadmap review",
    NAVY,
    [
      {
        title: "Where we are",
        slides: [
          {
            id: "s1",
            title: "FairSplit in Q4",
            notes: "Thirty seconds: the quarter in one line, then straight to the numbers.",
            body: [
              kicker("Roadmap review, October"),
              heading("FairSplit in Q4", 120),
              lead("From a command-line MVP to the first fifty groups settling up every week."),
            ],
          },
          {
            id: "s2",
            title: "Where the beta stands",
            notes:
              "Week 41 figures from the Beta health dashboard. The settle rate is the one to remember.",
            body: [heading("Where the beta stands"), stats(weekFigures)],
          },
          {
            id: "s3",
            title: "What we learned in September",
            notes: "The leftover cent is the whole story of the rounding disputes.",
            body: [
              heading("What we learned in September"),
              bullets([
                "Groups settle when the payment list is short: three payments or fewer settles 9 in 10 groups.",
                "Rounding disputes come from the leftover cent, never from the split rule itself.",
                "People record expenses on the day; nobody back-fills a week.",
              ]),
            ],
          },
        ],
      },
      {
        title: "The bets",
        slides: [
          {
            id: "s4",
            title: "Three bets for the quarter",
            notes: "Name the three, then take each in turn.",
            body: [
              heading("Three bets for the quarter"),
              columns([
                [
                  "Settle in one step",
                  "The fewest payments, exact to the cent, with the leftover cent named before anyone pays.",
                ],
                [
                  "Groups that last",
                  "Local storage that survives a reinstall, so a flat or a club keeps its history.",
                ],
                [
                  "A companion app",
                  "Record an expense from a phone and see the balance the command line computes.",
                ],
              ]),
            ],
          },
          {
            id: "s5",
            title: "Settle in one step",
            notes: "FAIR-4 is done, so this ships a week earlier than the draft said.",
            body: [
              kicker("Bet one"),
              heading("Settle in one step"),
              bullets([
                "FAIR-3 computes the fewest payments that bring every balance to zero.",
                "FAIR-4 splits exact to the cent and writes down who takes the leftover.",
                "Ships to the beta in the last week of October.",
              ]),
            ],
          },
          {
            id: "s6",
            title: "Groups that last",
            notes:
              "Both storage options are on the slide with their migration cost. Ask Miles to pick.",
            body: [
              kicker("Bet two"),
              heading("Groups that last"),
              bullets([
                "FAIR-5 keeps a group's members and expenses between runs.",
                "Option A: one JSON file per group. Migration is a copy.",
                "Option B: one SQLite file for every group. Migration is one command.",
              ]),
            ],
          },
          {
            id: "s7",
            title: "A companion app",
            notes: "Priya's concept is in Artifacts; open it if there is time.",
            body: [
              kicker("Bet three"),
              heading("A companion app"),
              bullets([
                "Priya's concept covers recording, splitting and settling up on a phone.",
                "It reads the same ledger file, so there is one source of truth.",
                "A clickable design is ready for the review on Thursday.",
              ]),
            ],
          },
        ],
      },
      {
        title: "What it takes",
        slides: [
          {
            id: "s8",
            title: "What it costs",
            notes:
              "Token ceilings are per agent per day; neither agent has hit its ceiling this sprint.",
            body: [
              heading("What it costs"),
              stats([
                ["6", "weeks of engineering"],
                ["2", "agents, one reviewer"],
                ["4M", "tokens a day, each agent's ceiling"],
              ]),
            ],
          },
          {
            id: "s9",
            title: "From the beta",
            notes: "Read the quote, then pause.",
            body: [
              quote(
                "We stopped arguing about who owes the cent. The app says it before anyone pays.",
                "Beta group, Flat 4B",
              ),
            ],
          },
          {
            id: "s10",
            title: "Decisions we need",
            notes: "Close here and wait for answers. Storage first.",
            body: [
              heading("Decisions we need from Miles"),
              bullets([
                "Storage: a JSON file per group, or one SQLite file.",
                "Whether the companion app joins the November beta or waits for January.",
                "The leftover-cent default: first names in order, or the payer.",
              ]),
            ],
          },
        ],
      },
    ],
    (id) => Q4_SLIDES[n]!.includes(id),
  );
}

export const INVESTOR_UPDATE: FixtureDeck = deck(
  "Investor update, October",
  "FairSplit investor update",
  PAPER,
  [
    {
      title: "The month",
      slides: [
        {
          id: "u1",
          title: "FairSplit",
          notes: "Keep the opening under a minute.",
          body: [
            kicker("October update"),
            heading("FairSplit", 128),
            lead("Splitting shared costs exactly, and settling them in the fewest payments."),
          ],
        },
        {
          id: "u2",
          title: "The month in numbers",
          notes: "Settled volume is from the beta ledgers, not projections.",
          body: [
            heading("The month in numbers"),
            stats([
              ["48", "active beta groups"],
              ["+31%", "expenses recorded, month on month"],
              ["€18.2k", "settled through FairSplit"],
            ]),
          ],
        },
        {
          id: "u3",
          title: "Highlights",
          notes: "",
          body: [
            heading("Highlights"),
            bullets([
              "The settle-up engine ships this month with exact cent splits.",
              "Two Crewlet agents now build and review every change.",
              "A companion app concept tested well with five groups.",
            ]),
          ],
        },
      ],
    },
    {
      title: "Asks",
      slides: [
        {
          id: "u4",
          title: "Where we need help",
          notes: "Name the two asks and stop talking.",
          body: [
            heading("Where we need help"),
            bullets([
              "Introductions to student housing operators.",
              "A payments partner for in-app settle-up in the EU.",
            ]),
          ],
        },
        {
          id: "u5",
          title: "From the beta",
          notes: "",
          body: [
            quote(
              "It is the first time our climbing club has closed a trip without a spreadsheet.",
              "Beta group, Climbing club",
            ),
          ],
        },
      ],
    },
  ],
);

export const TEMPLATE_DECK: FixtureDeck = deck("Review deck", "Review deck", PLAIN, [
  {
    title: "",
    slides: [
      {
        id: "t1",
        title: "Title",
        notes: "",
        body: [
          kicker("Kicker"),
          heading("A title that says the point", 104),
          lead("One line of context under it."),
        ],
      },
      {
        id: "t2",
        title: "Three things to know",
        notes: "",
        body: [
          heading("Three things to know"),
          bullets(["The first thing.", "The second thing.", "The third thing."]),
        ],
      },
      {
        id: "t3",
        title: "The numbers",
        notes: "",
        body: [
          heading("The numbers"),
          stats([
            ["0", "A figure"],
            ["0%", "A rate"],
          ]),
        ],
      },
    ],
  },
]);
