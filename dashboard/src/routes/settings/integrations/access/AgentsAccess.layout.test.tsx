// AN AGENT'S SCOPE, LAID OUT: the first entry's chip, the count of the rest
// and the control share one line inside the table at any name's length, the
// chip ending in an ellipsis rather than pushing the count or the control out.
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { LayerHost } from "@crewlethq/ui";
import "~/test/layout.tsx";
import type { AccessAnswer } from "~/contract/access.ts";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { Router } from "~/app/router.tsx";
import { AgentsAccess } from "./AgentsAccess.tsx";
import { cloudflareGrantsAnswer, githubAnswer } from "./fixtures.ts";

// LONGER THAN THE COLUMN at either width, so the chip has to give way: the
// longest name GitHub allows, a 39 character owner and a 100 character
// repository. A shorter one stopped overflowing at 1440px once the grid drew
// its short columns at their content (@crewlethq/ui 0.130.1) and gave this
// one the room, which left the ellipsis this case is about untested there.
const LONG =
  "example-organization-holding-every-repo/" +
  "customer-onboarding-notification-delivery-service-for-enterprise-tenants-in-every-region-and-zone-xy";

function githubWithLongScope(): AccessAnswer {
  const data = githubAnswer();
  data.options[0] = {
    ...data.options[0]!,
    scope: [LONG, "example/backend", "example/frontend"],
  };
  data.seats[0]!.lanes.github!.desired.scope = [LONG, "example/backend", "example/frontend"];
  return data;
}

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

async function mount(width: number, data: AccessAnswer = githubWithLongScope()) {
  await page.viewport(width, 900);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <Router>
        <LayerHost>
          <AppAnnouncer />
          <AgentsAccess
            kind={data.kind}
            name={data.kind === "github" ? "GitHub" : "Cloudflare"}
            data={data}
            reload={() => {}}
            held={undefined}
            avatarOf={() => null}
          />
        </LayerHost>
      </Router>,
    ),
  );
  await document.fonts.ready;
}

test.each([1440, 390])(
  "a long name's chip, the count and the pencil share one line at %ipx",
  async (width) => {
    await mount(width);
    const edit = document.querySelector<HTMLElement>(
      `[aria-label="Edit QA's GitHub repositories"]`,
    )!;
    const line = edit.parentElement!;
    const chip = line.querySelector<HTMLElement>(`[title="${LONG}"]`)!;
    const count = [...line.querySelectorAll<HTMLElement>("[title]")].find((el) =>
      el.textContent?.startsWith("+2"),
    )!;
    const table = edit.closest<HTMLElement>('[role="table"]')!.getBoundingClientRect();
    const [c, n, e] = [chip, count, edit].map((el) => el.getBoundingClientRect());
    const middle = (r: DOMRect) => r.top + r.height / 2;
    expect(Math.abs(middle(c!) - middle(e!))).toBeLessThan(4);
    expect(Math.abs(middle(n!) - middle(e!))).toBeLessThan(4);
    expect(c!.right).toBeLessThanOrEqual(n!.left);
    expect(n!.right).toBeLessThanOrEqual(e!.left);
    expect(e!.right).toBeLessThanOrEqual(table.right + 0.5);
    // THE NAME IS CUT IN THE CHIP, and whole in its title.
    const label = chip.lastElementChild as HTMLElement;
    expect(label.scrollWidth).toBeGreaterThan(label.clientWidth);
  },
);

// THE LEVEL PICKER'S LIST IS READ AT A READING WIDTH, not at its cell's. Each
// level is a name over the sentence saying what it grants, and the list was
// drawn at the Access column's width (about 120px): Secure's sentence broke a
// word or two to a line, taller than the window, and scrolled in a small box.
// The screen's own min-width could not lift it, since the app's sheet, the
// design system's among it, is linked after a screen's.
test.each([1440, 390])(
  "the level picker's list is wide enough to read its levels at %ipx, and leaves its field in view",
  async (width) => {
    // With a grant beside the level, as a catalogue that offers grants draws
    // it: the Grants column is what leaves the Access column narrow.
    const data = cloudflareGrantsAnswer();
    data.seats[1]!.lanes.cloudflare!.desired.level = "secure";
    await mount(width, data);
    const trigger = document.querySelector<HTMLElement>(`[aria-label="SRE's Cloudflare access"]`)!;
    trigger.click();
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    const panel = document.querySelector<HTMLElement>(".crewlet-select__menu")!;
    const box = panel.getBoundingClientRect();
    const field = trigger.closest<HTMLElement>(".crewlet-select")!.getBoundingClientRect();
    const rem = Number.parseFloat(getComputedStyle(document.documentElement).fontSize);
    expect(box.width).toBeGreaterThanOrEqual(Math.min(20 * rem, width - 8));
    expect(box.left).toBeGreaterThanOrEqual(0);
    expect(box.right).toBeLessThanOrEqual(width);
    // No level broken a word or two to a line: Secure's 209 characters take
    // a handful of lines, where at the cell's width they took over twenty.
    const notes = [...panel.querySelectorAll<HTMLElement>(".crewlet-select__option-desc")];
    expect(notes.length).toBeGreaterThan(0);
    for (const note of notes) {
      const line = Number.parseFloat(getComputedStyle(note).lineHeight);
      expect(note.getBoundingClientRect().height / line).toBeLessThanOrEqual(6.5);
    }
    // Every level shows before the list scrolls: the kit's cap for a
    // described list holds the four of them with their notes.
    const list = panel.querySelector<HTMLElement>('[role="listbox"]')!;
    expect(list.scrollHeight).toBeLessThanOrEqual(list.clientHeight + 1);
    // The list opens beside its field, never over the answer it shows.
    expect(box.top >= field.bottom || box.bottom <= field.top).toBe(true);
    expect(panel.querySelector('[aria-selected="true"]')?.textContent).toContain("Secure");
  },
);
