// A GROWTH DECISION, ITS CAPS AND A PRODUCT'S CONNECTIONS, LAID OUT at 1440, 768 and 390px in light
// and dark: nothing runs out sideways, every name and published text is drawn
// whole and wrapped (a person allows what they read), and the caps form's
// fields stay inside its rows. Long names are the vendors' own limits: a
// 255 character campaign name, a 100 character account name.
import { afterEach, describe, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import type { ReactNode } from "react";
import { LayerHost } from "@crewlethq/ui";
import "~/test/layout.tsx";
import { Router } from "~/app/router.tsx";
import type { GrowthCard, GrowthChangeRow } from "~/contract/growth.ts";
import { AnalyticsAttribution, GOOGLE_ANALYTICS_HOME } from "./AnalyticsAttribution.tsx";
import { GrowthConnections } from "./GrowthConnections.tsx";
import { GrowthDecisionCard } from "./GrowthDecisionCard.tsx";
import { RecentChanges } from "./RecentChanges.tsx";

// The recent changes' read and the chart's names, as an engine would answer them.
const changes = vi.hoisted(() => ({ rows: [] as unknown[] }));
vi.mock("~/lib/useQuery.ts", () => ({
  useQuery: () => ({
    data: { rows: changes.rows, next: "41", complete: false },
    loading: false,
    error: null,
  }),
}));
vi.mock("~/lib/store-hooks.ts", async (original) => ({
  ...(await original<typeof import("~/lib/store-hooks.ts")>()),
  useOrg: () => ({ roles: [], units: [] }),
}));
vi.mock("~/lib/seats.ts", async (original) => ({
  ...(await original<typeof import("~/lib/seats.ts")>()),
  useOrgIndex: () => ({
    byHandle: new Map([
      ["growth", { name: "Growth Lead for the whole northern hemisphere's stores" }],
    ]),
  }),
}));

const CAMPAIGN = "Spring sale for every region and every product line ".repeat(5).slice(0, 255);
const ACCOUNT =
  "Nimbus Advertising for the whole northern hemisphere and its every subsidiary company".padEnd(
    100,
    "x",
  );
const LONG_ID = "customers/1234567890/campaignBudgets/" + "9".repeat(128 - 37);
const REPLY = "Thank you for the review. ".repeat(14).slice(0, 349) + ".";

function spend(currency: string, micros: (units: number) => string): GrowthCard {
  return {
    act: "ad_spend",
    product: "google_ads",
    tool: "google_ads_raise_budget",
    account: {
      product: "google_ads",
      account: "123-456-7890",
      account_name: ACCOUNT,
      currency,
      time_zone: "America/Argentina/ComodRivadavia",
      state: "serving",
    },
    acts: [
      {
        target: CAMPAIGN,
        summary: "Raise the daily budget",
        danger: false,
        text_limit: 0,
        target_rating: 0,
        changes: [
          {
            field: "amount",
            label: "Daily budget",
            before: micros(40),
            after: micros(60),
            money: true,
          },
        ],
      },
    ],
    money: {
      currency,
      delta_micros: micros(20),
      daily_micros: micros(140),
      daily_after_micros: micros(160),
      per_change_cap_micros: micros(50),
      daily_cap_micros: micros(200),
    },
    findings: [
      "Destination not working: the landing page answered 404 for every one of the 12 tested regions.",
    ],
    reason: "It spent its whole budget by 14:00 on each of the last 5 days.",
    lapses_at: new Date(Date.now() + 23 * 3_600_000).toISOString(),
    moved: false,
    other_open_asks: 1,
  };
}

function batch(): GrowthCard {
  return {
    ...spend("USD", (n) => String(n * 1_000_000)),
    act: "public_post",
    product: "google_play",
    money: null,
    findings: [],
    acts: Array.from({ length: 10 }, (_, index) => ({
      target: `review-${index}`,
      summary: "Reply to a review",
      danger: false,
      changes: [],
      text: REPLY,
      text_limit: 350,
      target_excerpt: "The app logs me out every time I switch to another app and back again.",
      target_rating: (index % 5) + 1,
      target_author: `A reader whose display name runs long enough to wrap ${index}`,
    })),
  };
}

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  document.documentElement.removeAttribute("data-theme");
});

async function mount(width: number, theme: "light" | "dark", node: ReactNode) {
  await page.viewport(width, 900);
  document.documentElement.setAttribute("data-theme", theme);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <Router>
        <LayerHost>{node}</LayerHost>
      </Router>,
    ),
  );
  await document.fonts.ready;
}

/** Every element that runs out of its own box sideways, or out of the page. */
function overflowing(within: HTMLElement, width: number): string[] {
  const out: string[] = [];
  for (const el of within.querySelectorAll<HTMLElement>("*")) {
    const box = el.getBoundingClientRect();
    // A visually hidden line is one pixel by design, and read rather than seen.
    if (box.width <= 1 || el.closest(".crewlet-visually-hidden")) continue;
    const what = `${el.className || el.tagName} "${(el.textContent ?? "").slice(0, 40)}"`;
    if (box.right > width + 0.5) out.push(`${what} ends at ${box.right}`);
    const style = getComputedStyle(el);
    if (style.overflowX !== "visible" && el.scrollWidth > el.clientWidth + 1) {
      out.push(`${what} scrolls sideways (${el.scrollWidth} > ${el.clientWidth})`);
    }
  }
  return out;
}

/** The one logo file the stylesheet shows. */
function shownLogo(): HTMLImageElement {
  const shown = [...host!.querySelectorAll<HTMLImageElement>("img")].filter(
    (img) => getComputedStyle(img).display !== "none",
  );
  expect(shown).toHaveLength(1);
  return shown[0]!;
}

const card = (data: GrowthCard) => (
  <GrowthDecisionCard
    card={data}
    question="Raise the daily budget of the campaign?"
    state="pending"
    note=""
    onNote={() => {}}
    onAllow={() => {}}
    onDeny={() => {}}
  />
);

describe.each(["light", "dark"] as const)("on a %s document", (theme) => {
  test.each([1440, 768, 390])(
    "a spend decision in KWD stays inside %ipx, its names whole",
    async (width) => {
      await mount(width, theme, card(spend("KWD", (n) => String(n * 1_000_000 + 125_000))));
      expect(overflowing(host!, width)).toEqual([]);
      expect(host!.textContent).toContain(CAMPAIGN);
      expect(host!.textContent).toContain(ACCOUNT);
      expect(host!.textContent).toContain("KWD");
      // The amounts never break across lines.
      for (const money of host!.querySelectorAll<HTMLElement>(".crewlet-money")) {
        expect(money.getClientRects().length, money.textContent ?? "").toBe(1);
      }
    },
  );

  test.each([1440, 390])(
    "ten replies of 350 characters each are drawn whole at %ipx",
    async (width) => {
      await mount(width, theme, card(batch()));
      expect(overflowing(host!, width)).toEqual([]);
      const texts = [...host!.querySelectorAll<HTMLElement>(".growth-decision__text")];
      expect(texts).toHaveLength(10);
      for (const text of texts) {
        expect(text.textContent).toBe(REPLY);
        // Drawn whole: nothing clipped inside its own box.
        expect(text.scrollHeight).toBeLessThanOrEqual(text.clientHeight + 1);
      }
    },
  );

  test.each([1440, 390])(
    "a connection's accounts stay inside %ipx, a URL-long ID wrapped and a short one whole",
    async (width) => {
      const site = "sc-domain:" + "regional-storefront.".repeat(6) + "example.com";
      await mount(
        width,
        theme,
        <GrowthConnections
          kind="search_console"
          product="Search Console"
          roles={[]}
          connections={[
            {
              id: "main",
              name: ACCOUNT,
              identity: "crewlet-reader-for-the-whole-company@identity.example.com",
              state: "connected",
              roles_held: [],
              accounts: [
                {
                  product: "search_console",
                  account: site,
                  account_name: ACCOUNT,
                  state: "serving",
                },
                {
                  product: "search_console",
                  account: "123-456-7890",
                  account_name: "Web",
                  state: "serving",
                },
              ],
              open: false,
            },
          ]}
        />,
      );
      expect(overflowing(host!, width)).toEqual([]);
      expect(host!.textContent).toContain(site);
      const ids = [...host!.querySelectorAll<HTMLElement>(".growth-account__id")];
      expect(ids.map((id) => id.textContent)).toEqual([site, "123-456-7890"]);
      // A short ID moves to its own line rather than break.
      expect(ids[1]!.getClientRects().length).toBe(1);
    },
  );

  test.each([1440, 390])(
    "twenty recent changes with long targets stay inside %ipx, every name whole",
    async (width) => {
      const row = (n: number): GrowthChangeRow => ({
        seat: "growth",
        turn: "t",
        act: `h-${n}`,
        held: n % 2 === 0,
        product: "google_ads",
        connection: "main",
        account: LONG_ID.slice(0, 64),
        // A vendor's ID has no space to wrap at: the longest the log keeps, beside a long name.
        target: n % 2 === 0 ? LONG_ID : CAMPAIGN,
        tool: "google_ads_budget",
        class: "spend",
        step: n % 3 === 0 ? "failed" : n % 3 === 1 ? "allowed" : "applied",
        answered_by: "person",
        answerer: "Avery Admin of the European operations",
        via: "",
        operation: "ab",
        before: "cd",
        changes: [
          {
            field: "amount",
            label: "Daily budget",
            money: true,
            text: false,
            before: "40125000",
            after: "1234567890125000",
          },
          {
            field: "reply",
            label: "Reply",
            money: false,
            text: true,
            after_sha256: "cd",
            after_bytes: 4096,
          },
        ],
        money: null,
        failure: n % 3 === 0 ? "unknown_outcome" : "",
        at: new Date(Date.now() - n * 3_600_000).toISOString(),
      });
      changes.rows = Array.from({ length: 20 }, (_, n) => row(n));
      await mount(
        width,
        theme,
        <RecentChanges product="google_ads" accounts={new Map([[LONG_ID.slice(0, 64), "KWD"]])} />,
      );
      expect(overflowing(host!, width)).toEqual([]);
      const targets = [...host!.querySelectorAll<HTMLElement>(".growth-change__what")];
      expect(targets).toHaveLength(20);
      targets.forEach((target, n) =>
        expect(target.textContent).toBe(n % 2 === 0 ? LONG_ID : CAMPAIGN),
      );
      for (const money of host!.querySelectorAll<HTMLElement>(".crewlet-money")) {
        expect(money.getClientRects().length, money.textContent ?? "").toBe(1);
      }
    },
  );

  test.each([1440, 390])(
    "the Google Analytics logo shows the file for the ground, at the policy's 154 by 50 floor, inside %ipx",
    async (width) => {
      await mount(width, theme, <AnalyticsAttribution />);
      expect(overflowing(host!, width)).toEqual([]);
      const logo = shownLogo();
      await logo.decode();
      // The white-text file on a dark ground, the colour one on a light ground.
      expect(logo.currentSrc).toContain(theme === "dark" ? "_wht" : "_clr");
      const box = logo.getBoundingClientRect();
      expect(box.height).toBe(50);
      expect(box.width).toBeGreaterThanOrEqual(154);
      // Unaltered: drawn at the file's own aspect, 1519 by 402.
      expect(
        Math.abs(box.width / box.height - logo.naturalWidth / logo.naturalHeight),
      ).toBeLessThan(0.02);
      expect(logo.closest("a")!.href).toBe(GOOGLE_ANALYTICS_HOME);
    },
  );

  test("the stars of a review sit on the author's line", async () => {
    await mount(390, theme, card(batch()));
    const target = host!.querySelector<HTMLElement>(".growth-decision__target")!;
    const stars = target.querySelector<HTMLElement>(".crewlet-rating")!.getBoundingClientRect();
    const author = target
      .querySelector<HTMLElement>(".growth-decision__author")!
      .getClientRects()[0]!;
    expect(Math.abs(stars.top + stars.height / 2 - (author.top + author.height / 2))).toBeLessThan(
      4,
    );
  });
});

test("with no theme set, the Google Analytics logo follows the system as the tokens do", async () => {
  await mount(1440, "light", <AnalyticsAttribution />);
  document.documentElement.removeAttribute("data-theme");
  const light = window.matchMedia("(prefers-color-scheme: light)").matches;
  const logo = shownLogo();
  await logo.decode();
  expect(logo.currentSrc).toContain(light ? "_clr" : "_wht");
});
