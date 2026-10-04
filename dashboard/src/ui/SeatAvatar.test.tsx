/**
 * A seat's badge tells seats apart, and still says who it is.
 *
 * The kit makes initials from the first two words of a name, so a company
 * that names its agents "Agent CEO" and "Agent CTO" drew both as `AC` — on the
 * chart, side by side, and in every "Unit lead" and "Reports to" row. These
 * cases hold the two halves of the fix: the initials come from the words that
 * differ, and what a screen reader hears is still the whole name.
 */

import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { getInitials } from "@crewlethq/ui";
import { SeatAvatar, badgeName, seatBadge } from "./SeatAvatar.tsx";

afterEach(cleanup);

describe("a seat's badge", () => {
  test("two agents named for their role draw two different badges", () => {
    expect(getInitials(badgeName("Agent CEO", "agent"))).toBe("CE");
    expect(getInitials(badgeName("Agent CTO", "agent"))).toBe("CT");
    expect(getInitials(badgeName("agent-cto", "agent"))).toBe("CT");
    expect(getInitials(badgeName("Agent PM", "agent"))).toBe("PM");
  });

  test("the word is dropped only where the squircle already says it", () => {
    // A person's name is their name: the circle says nothing about "agent".
    expect(badgeName("Agent Smith", "human")).toBe("Agent Smith");
    // Only a LEADING word, and only the whole word.
    expect(badgeName("Support Agent", "agent")).toBe("Support Agent");
    expect(badgeName("Agentic Ops", "agent")).toBe("Agentic Ops");
    // A name that is nothing but the word keeps it rather than going blank.
    expect(badgeName("Agent", "agent")).toBe("Agent");
    expect(seatBadge("Agent CTO", undefined)).toEqual({ name: "CTO", kind: "agent" });
    expect(seatBadge("Agent CTO", "human")).toEqual({ name: "Agent CTO", kind: "human" });
  });

  test("the rendered badge draws the distinguishing initials", () => {
    const { container } = render(
      <>
        <SeatAvatar name="Agent CEO" kind="agent" decorative />
        <SeatAvatar name="Agent CTO" kind="agent" decorative />
      </>,
    );
    const drawn = [...container.querySelectorAll(".crewlet-avatar")].map((a) => a.textContent);
    expect(drawn).toEqual(["CE", "CT"]);
  });

  test("a badge that names its seat announces the WHOLE name", () => {
    render(<SeatAvatar name="Agent CTO" kind="agent" />);
    // One image, and it is the full name rather than the shortened words the
    // initials were made from.
    expect(screen.getAllByRole("img").map((i) => i.getAttribute("aria-label"))).toEqual([
      "Agent CTO avatar",
    ]);
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
