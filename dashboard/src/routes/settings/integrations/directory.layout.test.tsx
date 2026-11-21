// THE DIRECTORY'S GROUPS, LAID OUT at 1440, 768 and 390px in light and dark,
// drawn as the page draws them (`directoryGroups`): with every growth product
// served, and with this build's own catalogue, where the growth groups whose
// products have not landed are not drawn. Each group's heading stands over its
// lines, each name is whole on one line beside its mark, and nothing runs out
// sideways.
import { afterEach, describe, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { DirectoryList } from "@crewlethq/ui";
import type { Vendor } from "@crewlethq/icons";
import { overflowing } from "~/test/layout.tsx";
import { Router } from "~/app/router.tsx";
import { CATALOG, type Entry } from "./catalogue.tsx";
import { shownDirectory } from "./categories.ts";
import { directoryGroups } from "./Directory.tsx";
import type { BuiltInService } from "./model.ts";

const SERVICES: BuiltInService[] = (["chat", "worklet", "doclet", "gitlet"] as const).map(
  (key) => ({
    key,
    name: { chat: "Chatlet", worklet: "Worklet", doclet: "Doclet", gitlet: "Gitlet" }[key],
    vendor: "Crewlet",
    known: true,
    on: key !== "doclet",
    sentence: "",
  }),
);

/** The growth products as each will register, served beside this build's catalogue. */
const GROWTH: [key: string, name: string, vendor: Vendor][] = [
  ["google_play", "Google Play", "google-play"],
  ["google_ads", "Google Ads", "google-ads"],
  ["meta_ads", "Meta Ads", "meta-ads"],
  ["apple_ads", "Apple Ads", "apple-ads"],
  ["google_analytics", "Google Analytics", "google-analytics"],
  ["search_console", "Search Console", "google-search-console"],
  ["adsense", "AdSense", "google-adsense"],
  ["admob", "AdMob", "google-admob"],
];
const base = CATALOG.find((e) => e.key === "app_store_connect")!;
const EVERY: Entry[] = [
  ...CATALOG,
  ...GROWTH.map(([key, name, vendor]) => ({ ...base, key, name, vendor })),
];

let host: HTMLElement | null = null;
let root: Root | null = null;

afterEach(() => {
  root?.unmount();
  host?.remove();
  root = null;
  host = null;
});

async function mount(width: number, theme: "light" | "dark", catalog: readonly Entry[]) {
  await page.viewport(width, 1400);
  document.documentElement.setAttribute("data-theme", theme);
  host = document.createElement("div");
  // The rail beside the page on a wide screen; the whole screen on a phone, where it is one pane.
  host.style.cssText = `width: ${width <= 390 ? width : 300}px;`;
  document.body.append(host);
  root = createRoot(host);
  const groups = directoryGroups(shownDirectory(catalog), {
    kind: "google_ads",
    model: null,
    services: SERVICES,
    catalog,
  });
  flushSync(() =>
    root!.render(
      <Router>
        <DirectoryList label="Integrations" groups={groups} />
      </Router>,
    ),
  );
  await document.fonts.ready;
}

const HEADINGS = "h1, h2, h3, h4, h5, h6, [role='heading']";

/** Each group as drawn: its heading, and each line's name. */
function drawn(): [string, string[]][] {
  return [...host!.querySelectorAll(HEADINGS)].map((h) => [
    h.textContent ?? "",
    [...h.parentElement!.querySelectorAll(".crewlet-directory__label")].map(
      (label) => label.textContent ?? "",
    ),
  ]);
}

describe.each(["light", "dark"] as const)("on a %s document", (theme) => {
  test.each([1440, 768, 390])(
    "every growth group under its heading at %ipx, each name whole beside its mark",
    async (width) => {
      await mount(width, theme, EVERY);
      expect(drawn().slice(-4)).toEqual([
        ["Mobile apps", ["Expo", "App Store Connect", "Google Play"]],
        ["Advertising", ["Google Ads", "Meta Ads", "Apple Ads"]],
        ["Analytics", ["Google Analytics", "Search Console"]],
        ["Ad revenue", ["AdSense", "AdMob"]],
      ]);
      expect(overflowing(host!)).toEqual([]);
      for (const label of host!.querySelectorAll<HTMLElement>(".crewlet-directory__label")) {
        // Whole on one line: never cut, never wrapped.
        expect(label.scrollWidth, label.textContent ?? "").toBeLessThanOrEqual(
          label.clientWidth + 1,
        );
        expect(label.getClientRects().length, label.textContent ?? "").toBe(1);
        // The mark is centred on the name's line.
        const row = label.closest("a")!;
        const mark = row.querySelector("svg")!.getBoundingClientRect();
        const text = label.getBoundingClientRect();
        expect(
          Math.abs(mark.top + mark.height / 2 - (text.top + text.height / 2)),
          label.textContent ?? "",
        ).toBeLessThan(3);
      }
      // The groups keep one step between them, as the ones before.
      const headings = [...host!.querySelectorAll<HTMLElement>(HEADINGS)];
      const gaps = headings.slice(1).map((h, i) => {
        const above = headings[i]!.parentElement!.getBoundingClientRect();
        return Math.round(h.getBoundingClientRect().top - above.bottom);
      });
      expect(new Set(gaps).size, `gaps ${gaps}`).toBe(1);
    },
  );

  test.each([1440, 390])(
    "with this build's catalogue only the groups with a served product are drawn at %ipx",
    async (width) => {
      await mount(width, theme, CATALOG);
      const labels = drawn().map(([heading]) => heading);
      expect(labels).not.toContain("Advertising");
      expect(labels).not.toContain("Analytics");
      expect(labels).not.toContain("Ad revenue");
      expect(drawn().at(-1)).toEqual(["Mobile apps", ["Expo", "App Store Connect"]]);
      expect(overflowing(host!)).toEqual([]);
    },
  );
});
