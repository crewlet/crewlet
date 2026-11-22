// THE DIRECTORY'S GROUPS, LAID OUT at 1440, 768 and 390px in light and dark,
// drawn as the page draws them (`directoryGroups`): with every growth product
// served, and with this build's own catalogue, where the growth groups whose
// products have not landed are not drawn. Each group's heading stands over its
// lines, each name is whole on one line beside its mark and the widest state a
// line draws, in the page's own 17rem list column, and nothing runs out sideways.
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
import type { BuiltInService, Model } from "./model.ts";

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

/**
 * Every growth app connected and needing attention: the widest state a line
 * draws ("Needs attention") beside the longest names.
 */
const ATTENTION = {
  byKey: new Map(
    GROWTH.map(([key]) => [key, { configured: true, bucket: "attention", sections: [] }]),
  ),
} as unknown as Model;

let host: HTMLElement | null = null;
let root: Root | null = null;

afterEach(() => {
  root?.unmount();
  host?.remove();
  root = null;
  host = null;
});

async function mount(
  width: number,
  theme: "light" | "dark",
  catalog: readonly Entry[],
  model: Model | null = null,
) {
  await page.viewport(width, 1400);
  document.documentElement.setAttribute("data-theme", theme);
  host = document.createElement("div");
  // The page's list column (the kit's ListDetail list, 17rem) beside the page; the whole
  // screen on a phone, where the list is one pane.
  host.style.cssText = `width: ${width <= 390 ? `${width}px` : "17rem"};`;
  document.body.append(host);
  root = createRoot(host);
  const groups = directoryGroups(shownDirectory(catalog), {
    kind: "google_ads",
    model,
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
    "every growth group under its heading at %ipx, each name whole beside its mark and its state",
    async (width) => {
      await mount(width, theme, EVERY, ATTENTION);
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
        // A growth line draws its state beside its whole name; the mark is centred on the name's line.
        if (GROWTH.some(([, name]) => name === label.textContent))
          expect(label.closest("a")!.textContent, label.textContent ?? "").toContain(
            "Needs attention",
          );
        const row = label.closest("a")!;
        const mark = row.querySelector("svg")!.getBoundingClientRect();
        const text = label.getBoundingClientRect();
        expect(
          Math.abs(mark.top + mark.height / 2 - (text.top + text.height / 2)),
          label.textContent ?? "",
        ).toBeLessThan(3);
      }
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
