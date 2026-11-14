/**
 * An atlas finding of the setup advisor, fixed (ADR-0141): the dialog names
 * each entry with a link to its page, sends the atlas's own tool for what the
 * fix asks, never sends before the reader has added what it asks for, and
 * holds every control for a reader the engine would refuse.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import type { AdvisorFinding } from "~/protocol/advisor.ts";
import { grownCompany, reviewAnswer, waitingItems } from "~/test/atlas.ts";
import { mountAtlas } from "~/test/atlasMount.tsx";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
import { AdvisorAtlasFix, atlasPlace } from "./AdvisorFix.tsx";

/** A holder of Atlas who is no administrator. */
const ADA = {
  login: "U0ADA",
  grants: ["atlas"],
  handle: "ada",
  owner: "ada",
  name: "Ada",
  kind: "human",
  acts: ["atlas_set_owner", "atlas_relate", "atlas_confirm", "atlas_correct", "atlas_refresh"],
};

const CHECKOUT = {
  id: "0195a1c0-0000-8000-8000-00000000c0de",
  kind: "service",
  key: "checkout-api",
  name: "Checkout API",
} as const;

function finding(over: Partial<AdvisorFinding>): AdvisorFinding {
  return {
    class: "atlas.unowned",
    severity: "note",
    actor: "owner",
    source: "atlas",
    area: "atlas",
    subject: { kind: "atlas" },
    title: "7 services have no owner.",
    why: "Nobody is named as answering for a service the company runs.",
    path: "",
    evidence: [{ key: "entries", value: 7 }],
    entries: [CHECKOUT],
    fix: {
      kind: "atlas_act",
      sentence: "Name the seat or the unit that answers for each.",
      options: [],
      act: { tool: "atlas_set_owner", asks: "owner" },
    },
    ...over,
  };
}

let posted: { tool: string; args: Record<string, unknown> }[];

beforeEach(() => {
  posted = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      const body = JSON.parse(init.body as string) as { args: Record<string, unknown> };
      posted.push({ tool, args: body.args });
      return new Response(JSON.stringify({ tool, outcome: "applied", receipt: {} }), {
        status: 200,
        headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  location.hash = "";
});

function mount(f: AdvisorFinding, answers: Record<string, unknown> = {}) {
  return mountAtlas(
    "#/settings/advisor",
    <AdvisorAtlasFix finding={f} onClose={() => {}} />,
    grownCompany(),
    { answers: { viewer: ADA, ...answers } },
  );
}

/** The sentence a held control is described by. */
function reasonOf(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

describe("an atlas finding's fix", () => {
  test("names each entry with a link to its page, and how many more there are", async () => {
    mount(finding({}));
    const dialog = await screen.findByRole("dialog");
    const link = within(dialog).getByRole("link", { name: "Checkout API" });
    expect(link.getAttribute("href")).toBe(`#/company/atlas/${CHECKOUT.id}`);
    expect(
      within(dialog).getByText(/6 more entries are listed here once these are fixed/),
    ).toBeTruthy();
    expect(within(dialog).getByRole("link", { name: "Open the atlas" }).getAttribute("href")).toBe(
      "#/company/atlas",
    );
  });

  test("sends no owner before one is picked", async () => {
    mount(finding({}));
    const press = await screen.findByRole("button", { name: "Set owner" });
    await waitFor(() => expect(reasonOf(press)).toContain("Pick the seat or the unit"));
    expect(press.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(press);
    expect(posted).toEqual([]);
  });

  test("names the owner on record that left the chart", async () => {
    mount(
      finding({
        class: "atlas.owner_gone",
        entries: [{ ...CHECKOUT, owner: { kind: "seat", name: "grace" } }],
      }),
    );
    expect(await screen.findByText(/Owner on record: grace, no longer in the chart/)).toBeTruthy();
  });

  test("asks for a read of the one source a stale finding names", async () => {
    mount(
      finding({
        class: "atlas.source_stale",
        subject: { kind: "integration", name: "Datadog" },
        title: "The atlas has not read Datadog whole in 4 hours.",
        evidence: [{ key: "hours", value: 4 }],
        entries: undefined,
        fix: {
          kind: "atlas_act",
          sentence: "Ask for a read now.",
          options: [],
          act: { tool: "atlas_refresh", args: { integration: "datadog" } },
        },
      }),
    );
    const press = await screen.findByRole("button", { name: "Read Datadog now" });
    await waitFor(() => expect(press.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(press);
    await waitFor(() =>
      expect(posted).toEqual([{ tool: "atlas_refresh", args: { integration: "datadog" } }]),
    );
    expect(screen.getByRole("link", { name: "Open the atlas's sources" })).toBeTruthy();
  });

  test("decides a disagreement with the review's own decisions, on the named entries only", async () => {
    const company = grownCompany();
    const items = waitingItems(company);
    const disputed = items.find((item) => item.kind === "disagreement")!;
    mount(
      finding({
        class: "atlas.disagreement",
        entries: [
          {
            id: disputed.entry.id,
            kind: disputed.entry.kind,
            key: disputed.entry.key,
            name: disputed.entry.name,
          },
        ],
        fix: {
          kind: "atlas_act",
          sentence: "Decide each in the atlas's review.",
          options: [],
          act: { tool: "atlas_confirm", asks: "value" },
        },
      }),
      { atlas_review: reviewAnswer(items) },
    );
    const keep = await screen.findByRole("button", { name: /^Keep / });
    // ONLY THE NAMED ENTRY'S DISAGREEMENT: what waits as a whole is the review's.
    expect(screen.getAllByRole("button", { name: /^Keep / })).toHaveLength(1);
    await waitFor(() => expect(keep.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(keep);
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]!.tool).toBe("atlas_confirm");
    expect(posted[0]!.args.fact).toBe(disputed.fact!.fact);
  });

  test("holds every control for a reader the engine would not serve", async () => {
    mountAtlas(
      "#/settings/advisor",
      <AdvisorAtlasFix finding={finding({})} onClose={() => {}} />,
      grownCompany(),
      { answers: { viewer: { ...ADA, acts: [] } } },
    );
    const press = await screen.findByRole("button", { name: "Set owner" });
    await waitFor(() => expect(reasonOf(press)).toContain(WRITE_REASONS.not_served));
    fireEvent.click(press);
    expect(posted).toEqual([]);
  });

  test("holds every control for a reader who is not signed in", async () => {
    const viewer = { login: "", grants: [], handle: "", owner: "", name: "", kind: "", acts: [] };
    mountAtlas(
      "#/settings/advisor",
      <AdvisorAtlasFix finding={finding({})} onClose={() => {}} />,
      grownCompany(),
      { answers: { viewer } },
    );
    const press = await screen.findByRole("button", { name: "Set owner" });
    await waitFor(() => expect(reasonOf(press)).toContain(WRITE_REASONS.anonymous));
    expect(press.getAttribute("aria-disabled")).toBe("true");
  });
});

test("a finding's link opens the screen of the atlas it is decided on", () => {
  expect(atlasPlace({ class: "atlas.disagreement" }).path).toEqual(["company", "atlas", "review"]);
  expect(atlasPlace({ class: "atlas.source_stale" }).path).toEqual(["company", "atlas", "sources"]);
  expect(atlasPlace({ class: "atlas.unowned" }).path).toEqual(["company", "atlas"]);
});
