/**
 * A seat's badge tells seats apart, and still says who it is.
 *
 * A company that names its agents "Agent CEO" and "Agent CTO" drew both as
 * `AC`, on the chart side by side and in every "Unit lead" and "Reports to"
 * row. The kit's Avatar now takes an agent's initials past that word and keeps
 * the whole name as what a screen reader hears; these cases hold that the
 * seat's badge gets both, through the kit.
 */

import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { avatarInitialsName, getInitials } from "@crewlethq/ui";
import { SeatAvatar, seatBadge } from "./SeatAvatar.tsx";

afterEach(cleanup);

describe("a seat's badge", () => {
  test("two agents named for their role draw two different badges", () => {
    expect(getInitials(avatarInitialsName("Agent CEO", "agent"))).toBe("CE");
    expect(getInitials(avatarInitialsName("Agent CTO", "agent"))).toBe("CT");
    expect(getInitials(avatarInitialsName("agent-cto", "agent"))).toBe("CT");
    expect(getInitials(avatarInitialsName("Agent PM", "agent"))).toBe("PM");
  });

  test("the word is dropped only where the squircle already says it", () => {
    // A person's name is their name: the circle says nothing about "agent".
    expect(avatarInitialsName("Agent Smith", "human")).toBe("Agent Smith");
    // Only a LEADING word, and only the whole word.
    expect(avatarInitialsName("Support Agent", "agent")).toBe("Support Agent");
    expect(avatarInitialsName("Agentic Ops", "agent")).toBe("Agentic Ops");
    // A name that is nothing but the word keeps it rather than going blank.
    expect(avatarInitialsName("Agent", "agent")).toBe("Agent");
    // A stack's member keeps the whole name, and the kit draws its initials.
    expect(seatBadge("Agent CTO", undefined, undefined)).toEqual({
      name: "Agent CTO",
      kind: "agent",
    });
    expect(seatBadge("Agent CTO", "human", null)).toEqual({ name: "Agent CTO", kind: "human" });
  });

  test("the rendered badge draws the distinguishing initials", () => {
    const { container } = render(
      <>
        <SeatAvatar avatar={undefined} name="Agent CEO" kind="agent" decorative />
        <SeatAvatar avatar={undefined} name="Agent CTO" kind="agent" decorative />
      </>,
    );
    const drawn = [...container.querySelectorAll(".crewlet-avatar")].map((a) => a.textContent);
    expect(drawn).toEqual(["CE", "CT"]);
  });

  test("a badge that names its seat announces the WHOLE name", () => {
    render(<SeatAvatar avatar={undefined} name="Agent CTO" kind="agent" />);
    // One image, and it is the full name rather than the shortened words the
    // initials were made from.
    expect(screen.getAllByRole("img").map((i) => i.getAttribute("aria-label"))).toEqual([
      "Agent CTO avatar",
    ]);
  });
});

describe("an agent drawn as itself", () => {
  test("draws its character on a plate of its colour, in place of initials", () => {
    const { container } = render(
      <SeatAvatar name="Agent CTO" kind="agent" avatar={{ character: "foxlet", color: "rose" }} />,
    );
    const badge = container.querySelector(".crewlet-avatar")!;
    expect(badge.classList.contains("crewlet-avatar--hue-rose")).toBe(true);
    expect(container.querySelector('[data-character="foxlet"]')).not.toBeNull();
    expect(badge.textContent).toBe("");
    // Still announced by the whole name, the drawing being decorative.
    expect(screen.getByRole("img", { name: "Agent CTO avatar" })).toBeDefined();
  });

  test("a person is drawn as themself whatever avatar a caller hands it", () => {
    const { container } = render(
      <SeatAvatar
        name="Pat Lee"
        kind="human"
        avatar={{ character: "foxlet", color: "rose" }}
        decorative
      />,
    );
    expect(container.querySelector("[data-character]")).toBeNull();
    expect(container.querySelector(".crewlet-avatar")!.textContent).toBe("PL");
    expect(seatBadge("Pat Lee", "human", { character: "foxlet", color: "rose" })).toEqual({
      name: "Pat Lee",
      kind: "human",
    });
  });

  test("a key the chart does not hold keeps its initials", () => {
    const { container } = render(
      <SeatAvatar avatar={undefined} name="Agent CTO" kind="agent" decorative />,
    );
    expect(container.querySelector("[data-character]")).toBeNull();
    expect(container.querySelector(".crewlet-avatar")!.textContent).toBe("CT");
    expect(seatBadge("Agent CTO", "agent", { character: "octlet", color: "blue" })).toEqual({
      name: "Agent CTO",
      kind: "agent",
      character: "octlet",
      hue: "blue",
    });
  });
});

describe("what a seat is doing", () => {
  test("is the kit's corner glyph, never a ring, and is said in the name of a badge that names itself", () => {
    const { container } = render(
      <>
        <SeatAvatar avatar={undefined} name="Agent CTO" kind="agent" status="stopped" />
        <SeatAvatar avatar={undefined} name="Agent PM" kind="agent" status="needs" decorative />
      </>,
    );
    expect(container.querySelector(".crewlet-avatar__status--stopped")).not.toBeNull();
    expect(container.querySelector(".crewlet-avatar__status--needs")).not.toBeNull();
    expect(container.innerHTML).not.toContain("ring-");
    // The whole name, and the state after it; the decorative one says nothing.
    expect(screen.getAllByRole("img").map((i) => i.getAttribute("aria-label"))).toEqual([
      "Agent CTO avatar, stopped",
    ]);
  });

  // Mutation: let a status win over a verdict, or leave the verdict out of the name, and this fails.
  test("a reviewer's verdict is the kit's corner, winning over a status, and said in the name of a badge that names itself", () => {
    const { container } = render(
      <SeatAvatar
        avatar={undefined}
        name="Code Reviewer"
        kind="agent"
        status="working"
        verdict="approved"
      />,
    );
    expect(container.querySelector(".crewlet-avatar__status--approved")).not.toBeNull();
    expect(container.querySelector(".crewlet-avatar__status--working")).toBeNull();
    expect(screen.getByRole("img").getAttribute("aria-label")).toBe(
      "Code Reviewer avatar, approved",
    );
  });

  test("an idle seat carries no glyph", () => {
    const { container } = render(
      <SeatAvatar avatar={undefined} name="Agent CTO" kind="agent" decorative />,
    );
    expect(container.querySelector(".crewlet-avatar__status")).toBeNull();
  });
});

// ONE PLACE DRAWS A SEAT'S BADGE. The kit's `Avatar` makes initials from
// whatever name it is handed, so a screen that reached it directly would draw
// "Agent CTO" as `AC` again — and nothing on that screen would look wrong
// until two such seats sat side by side.
test("no module but this one renders the kit's Avatar", () => {
  const root = join(__dirname, "..");
  const offenders: string[] = [];
  const walk = (dir: string) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const at = join(dir, e.name);
      if (e.isDirectory()) walk(at);
      else if (/\.tsx$/.test(e.name) && !/\.test\.tsx$/.test(e.name)) {
        if (at.endsWith(join("ui", "SeatAvatar.tsx"))) continue;
        if (/<Avatar\b/.test(readFileSync(at, "utf8"))) offenders.push(at.slice(root.length + 1));
      }
    }
  };
  walk(root);
  expect(offenders, "render SeatAvatar (or hand OrgLabel seatBadge) instead").toEqual([]);
});

/** The modules that may hand a badge a verdict: Gitlet's face and the change row that draws it. */
const VERDICT_HOLDERS = new Set(["routes/gitlet/ui.tsx", "routes/gitlet/ChangeRow.tsx"]);

/** Where a module outside `VERDICT_HOLDERS` hands `SeatAvatar` or `Face` a verdict, as `path`. */
export function verdictSites(path: string, text: string): string[] {
  if (VERDICT_HOLDERS.has(path)) return [];
  return /<(?:SeatAvatar|Face)\b[^>]*\bverdict=/.test(text) ? [path] : [];
}

// A VERDICT ON A FACE IS GITLET'S, in a change's row: elsewhere a verdict stays words.
test("only Gitlet's face hands a badge a verdict", () => {
  const root = join(__dirname, "..");
  const offenders: string[] = [];
  const walk = (dir: string) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const at = join(dir, e.name);
      if (e.isDirectory()) walk(at);
      else if (/\.tsx$/.test(e.name) && !/\.test\.tsx$/.test(e.name)) {
        offenders.push(...verdictSites(at.slice(root.length + 1), readFileSync(at, "utf8")));
      }
    }
  };
  walk(root);
  expect(offenders).toEqual([]);
  // IT CAN TELL: a verdict handed anywhere else is reported.
  expect(verdictSites("routes/me/X.tsx", '<Face who="cto" verdict="approved" />')).toEqual([
    "routes/me/X.tsx",
  ]);
  expect(
    verdictSites("routes/gitlet/ChangeRow.tsx", '<Face who="cto" verdict="approved" />'),
  ).toEqual([]);
});

/** Every source module of the dashboard, as `[path, text]`, tests left out. */
function sources(exts: RegExp): [string, string][] {
  const root = join(__dirname, "..");
  const out: [string, string][] = [];
  const walk = (dir: string) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const at = join(dir, e.name);
      if (e.isDirectory()) walk(at);
      else if (exts.test(e.name) && !/\.test\.tsx?$/.test(e.name)) {
        out.push([at.slice(root.length + 1), readFileSync(at, "utf8")]);
      }
    }
  };
  walk(root);
  return out;
}

/** Where a module reads an agent's look part by part, which only these may. */
const LOOK_READERS: Record<string, string> = {
  "lib/avatar.ts": "the one translation, faceOf",
  "app/crumbs.ts": "a published look serialised onto a page label and read back",
  "ui/orgNodes.tsx": "a chart node's mark and tone, the org chart's own drawing",
  "routes/org/builder/AddNodeDialog.tsx": "the look an author picks, written into the seat",
  "routes/org/builder/NodeEditor.tsx": "the look an author edits",
  "routes/org/builder/editorForm.ts": "the parts of the look an edit changed",
  "routes/settings/integrations/access/Dialogs.tsx": "a timeline tinted in the seat's colour",
};

/** The look reads in a module outside [LOOK_READERS], as `path:line`. */
export function lookReads(path: string, text: string): string[] {
  if (path in LOOK_READERS) return [];
  const out: string[] = [];
  text.split("\n").forEach((line, i) => {
    if (/\bavatar\??\.(character|color)\b/.test(line)) out.push(`${path}:${i + 1}`);
  });
  return out;
}

// ONE TRANSLATION OF A LOOK. A screen that copied an agent's character into the
// kit by hand drew its own rule beside `faceOf`'s, and one of them drifted.
test("an agent's look reaches the kit through faceOf alone", () => {
  const offenders = sources(/\.tsx?$/).flatMap(([path, text]) => lookReads(path, text));
  expect(offenders, "use faceOf, seatBadge or SeatAvatar").toEqual([]);
  // IT CAN TELL.
  expect(lookReads("routes/x.tsx", "person.character = seat.avatar.character;")).toEqual([
    "routes/x.tsx:1",
  ]);
});

/** A kit tag naming a seat by a hand-written `kind`, as `path:line`: its face is then the kit's initials. */
export function handKindedSeats(path: string, text: string): string[] {
  const out: string[] = [];
  for (const m of text.matchAll(/<(EntityChip|TableEntityCell)\b([^>]*?)\/?>/gs)) {
    const attrs = m[2]!;
    if (/\bkind=/.test(attrs) && !/\bbadge=/.test(attrs)) {
      out.push(`${path}:${text.slice(0, m.index).split("\n").length}`);
    }
  }
  return out;
}

// A SEAT CHIP OR CELL TAKES ITS FACE FROM THE CHART: an EntityChip spreads
// seatBadge, a TableEntityCell spreads linkedSeat or plainSeat. Given a `kind`
// by hand, the kit drew an agent as its initials.
test("no seat chip or cell is drawn from a hand-written kind", () => {
  const offenders = sources(/\.tsx$/).flatMap(([path, text]) => handKindedSeats(path, text));
  expect(offenders, "spread seatBadge, linkedSeat or plainSeat").toEqual([]);
  expect(handKindedSeats("a.tsx", '<EntityChip name={n} kind="agent" size="xs" />')).toEqual([
    "a.tsx:1",
  ]);
  expect(handKindedSeats("a.tsx", '<EntityChip {...seatBadge(n, k, a)} size="xs" />')).toEqual([]);
});

/** Kit gaps a picker below is waiting on, with the release that closes each. */
const FACELESS_PICKERS: Record<string, string> = {};

/** Seat options with no face (`leading`, `icon`, `face`, `media`), as `path:line`. */
export function facelessSeatOptions(path: string, text: string): string[] {
  if (path in FACELESS_PICKERS) return [];
  const out: string[] = [];
  for (const m of text.matchAll(/\bvalue:\s*[\w?.!]+\.handle\s*[,}\n]/g)) {
    let i = m.index;
    for (let depth = 0; i > 0;) {
      i -= 1;
      if (text[i] === "}") depth += 1;
      else if (text[i] === "{") {
        if (depth === 0) break;
        depth -= 1;
      }
    }
    let j = m.index;
    for (let depth = 0; j < text.length; j += 1) {
      if (text[j] === "{") depth += 1;
      else if (text[j] === "}") {
        if (depth === 0) break;
        depth -= 1;
      }
    }
    if (!/\b(leading|icon|face|media)\b/.test(text.slice(i, j + 1))) {
      out.push(`${path}:${text.slice(0, m.index).split("\n").length}`);
    }
  }
  return out;
}

// EVERY PERSON AND AGENT A PICKER OFFERS WEARS ITS FACE: an option whose value
// is a seat's handle carries one, through ChoiceFace.
test("every option that picks a seat draws the seat's face", () => {
  const offenders = sources(/\.tsx$/).flatMap(([path, text]) => facelessSeatOptions(path, text));
  expect(offenders, "give the option `leading: <ChoiceFace seat={…} />`").toEqual([]);
  expect(
    facelessSeatOptions("a.tsx", "seats.map((s) => ({ value: s.handle, label: s.name }))"),
  ).toEqual(["a.tsx:1"]);
  expect(
    facelessSeatOptions(
      "a.tsx",
      "({ value: s.handle, label: s.name, leading: <ChoiceFace seat={s} /> })",
    ),
  ).toEqual([]);
});
