/**
 * Structure's seat finder in the page bar.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, expect, test } from "vitest";
import { indexOrg, nameOfIn } from "~/lib/seats.ts";
import type { LiveOrg } from "~/lib/seatLive.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { fixtureCompany } from "./builder/model/testkit.ts";
import { checkedEdit } from "./builder/testState.ts";
import { BuilderHarness, builderSpies, harnessProbe } from "./builder/viewTestkit.tsx";
import { FindSeat } from "./Structure.tsx";
import { StructureLiveProvider } from "./structureLive.tsx";

afterEach(cleanup);

function draw() {
  const index = indexOrg(withDerived(fixtureCompany()) as unknown as OrgProjection);
  const live: LiveOrg = { index, live: new Map(), loaded: true, nameOf: nameOfIn(index) };
  render(
    <BuilderHarness
      initial={checkedEdit(fixtureCompany())}
      spies={builderSpies()}
      probe={harnessProbe()}
    >
      <StructureLiveProvider value={live}>
        <FindSeat />
      </StructureLiveProvider>
    </BuilderHarness>,
  );
  return screen.getByRole("combobox", { name: "Find a seat" });
}

/*
 * ON A PHONE THE LIST WAS CLIPPED BY THE BAR'S SCROLLING CONTROLS: typing drew nothing.
 * The frame lifts that clip for a bar holding a `page-find` field and gives the field
 * the line, so the finder has to be one. Mutation: drop `page-find`.
 */
test("the finder is the page bar's find field, which nothing that scrolls clips", () => {
  const box = draw();
  expect(box.closest(".page-find")).not.toBeNull();
  const frame = readFileSync(join(process.cwd(), "src/styles/frame.css"), "utf8");
  expect(frame).toContain(".page-controls:has(.page-find)");
});

/*
 * THE FIELD HAS A NAME OF ITS OWN. The kit's `label` names the list, so the field
 * was named by its placeholder alone. Mutation: drop its `aria-label`.
 */
test("the field and its list are each named", () => {
  fireEvent.change(draw(), { target: { value: "sre" } });
  expect(screen.getByRole("listbox", { name: "Seats" })).toBeDefined();
});

/* A match says who it is and where it sits, so seats in like-named teams read apart. */
test("a match says its name, its handle and where it sits", () => {
  const box = draw();
  fireEvent.change(box, { target: { value: "sre" } });
  const option = screen.getByRole("option", { name: /SRE/ });
  expect(within(option).getByText("SRE")).toBeDefined();
  expect(within(option).getByText(/@sre · .*Platform/)).toBeDefined();
});
