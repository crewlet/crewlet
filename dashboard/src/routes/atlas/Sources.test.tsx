/**
 * Company › Atlas › Sources: each source's standing as the engine answered it,
 * and "Refresh now" held to the engine's floor before a press can be refused.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { grownCompany, sourceStanding, sourcesAnswer } from "~/test/atlas.ts";
import { mountAtlas } from "~/test/atlasMount.tsx";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
import { Atlas } from "./Atlas.tsx";
import { SourcesPage, readLine, refreshBlocked, span } from "./Sources.tsx";

/** A holder of Atlas who is no administrator. */
const ADA = {
  login: "U0ADA",
  grants: ["atlas"],
  handle: "ada",
  owner: "ada",
  name: "Ada",
  kind: "human",
  acts: ["atlas_refresh"],
};

const NOW = Date.parse("2026-11-11T09:00:00Z");

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

/** The sentence a held control is described by. */
function reasonOf(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

function mountSources(answer = sourcesAnswer()) {
  return mountAtlas("#/company/atlas/sources", <SourcesPage />, grownCompany(), {
    answers: { viewer: ADA, atlas_sources: answer },
  });
}

/** The card of the source called `name`. */
async function card(name: string): Promise<HTMLElement> {
  const heading = await screen.findByRole("heading", { name });
  const region = heading.closest("section, article");
  if (!(region instanceof HTMLElement)) throw new Error(`${name} is in no card`);
  return region;
}

describe("a source's standing", () => {
  test("says when it is read next, and for a failed read when it was last read whole", () => {
    expect(readLine(sourceStanding("cloudflare"), NOW)).toBe("Read 10m ago, next read in 50m.");
    expect(
      readLine(
        sourceStanding("github", {
          state: "failed",
          last_attempt_at: "2026-11-11T08:58:00Z",
          next_read_at: undefined,
        }),
        NOW,
      ),
    ).toBe("Tried 2m ago, last read whole 10m ago, due now.");
    expect(
      readLine(
        sourceStanding("ssh", {
          state: "never",
          last_attempt_at: undefined,
          last_complete_at: undefined,
        }),
        NOW,
      ),
    ).toMatch(/^It is read within a minute/);
  });

  test("says a cadence in words", () => {
    expect(span(3600)).toBe("hour");
    expect(span(7200)).toBe("2 hours");
    expect(span(300)).toBe("5 minutes");
    expect(span(90)).toBe("1m 30s");
  });

  test("holds Refresh now inside the engine's floor and while an ask waits", () => {
    expect(refreshBlocked(sourceStanding("cloudflare"), NOW)).toBeUndefined();
    expect(
      refreshBlocked(sourceStanding("github", { refreshable_at: "2026-11-11T09:03:00Z" }), NOW),
    ).toBe("Read moments ago. It can be read again in 3m");
    // THE FLOOR HAS PASSED: an instant behind the clock holds nothing.
    expect(
      refreshBlocked(sourceStanding("github", { refreshable_at: "2026-11-11T08:59:00Z" }), NOW),
    ).toBeUndefined();
    expect(
      refreshBlocked(
        sourceStanding("datadog", { refresh_requested_at: "2026-11-11T08:59:30Z" }),
        NOW,
      ),
    ).toBe("A read is already asked for");
  });
});

describe("Sources", () => {
  test("draws every source with its state, its counts and what it was refused", async () => {
    mountSources();
    const cloudflare = await card("Cloudflare");
    expect(within(cloudflare).getByText("Read")).toBeTruthy();
    expect(within(cloudflare).getByText("48")).toBeTruthy();
    // WITHHELD VALUES ARE COUNTED, and nothing of them is drawn.
    expect(within(cloudflare).getByText("Values withheld")).toBeTruthy();
    expect(within(cloudflare).getByText("2")).toBeTruthy();
    expect(within(cloudflare).getByText("1 entry retired")).toBeTruthy();

    const datadog = await card("Datadog");
    expect(within(datadog).getByText("Partly read")).toBeTruthy();
    expect(within(datadog).getByText(/monitors_read/)).toBeTruthy();
    expect(within(datadog).getByText(/2 entries were not in the last read/)).toBeTruthy();

    const github = await card("GitHub");
    expect(within(github).getByText("Last read failed")).toBeTruthy();
    // NO FAULT TEXT FOR A HOLDER WHO IS NO ADMINISTRATOR: the engine sent none.
    expect(within(github).getByText(/An administrator is shown why/)).toBeTruthy();
  });

  test("lists each server with its own last whole read and what was not read", async () => {
    mountSources();
    const servers = await card("Servers");
    const table = within(servers).getByRole("table");
    const rows = within(table).getAllByRole("row");
    expect(rows).toHaveLength(3);
    expect(rows[1]!.textContent).toContain("web-1");
    expect(rows[1]!.textContent).toContain("Read");
    expect(rows[2]!.textContent).toContain("web-2");
    expect(rows[2]!.textContent).toContain("2 parts not read");
    expect(within(servers).getByText(/could not reach the server/)).toBeTruthy();
    expect(within(servers).getByText("1 more part not read.")).toBeTruthy();
  });

  test("Refresh now asks for one source, and is held where the engine would refuse", async () => {
    // THE SCREEN READS THE REAL CLOCK, so the held source's floor is ahead of it.
    const held = new Date(Date.now() + 180_000).toISOString();
    const { asked } = mountSources(
      sourcesAnswer({
        sources: [
          sourceStanding("cloudflare"),
          sourceStanding("github", { state: "failed", refreshable_at: held }),
        ],
      }),
    );
    const cloudflare = await card("Cloudflare");
    const refresh = within(cloudflare).getByRole("button", { name: "Refresh now" });
    await waitFor(() => expect(refresh.getAttribute("aria-disabled")).not.toBe("true"));
    const github = await card("GitHub");
    expect(
      within(github).getByRole("button", { name: "Refresh now" }).getAttribute("aria-disabled"),
    ).toBe("true");
    fireEvent.click(within(github).getByRole("button", { name: "Refresh now" }));
    expect(posted).toEqual([]);
    const before = asked.filter((one) => one.what === "atlas_sources").length;
    fireEvent.click(refresh);
    await waitFor(() =>
      expect(posted).toEqual([{ tool: "atlas_refresh", args: { integration: "cloudflare" } }]),
    );
    // THE STANDINGS ARE ASKED AGAIN once the ask is answered.
    await waitFor(() =>
      expect(asked.filter((one) => one.what === "atlas_sources").length).toBeGreaterThan(before),
    );
  });

  test("holds Refresh now with its reason for a reader the engine would not serve", async () => {
    mountAtlas("#/company/atlas/sources", <SourcesPage />, grownCompany(), {
      answers: { viewer: { ...ADA, acts: [] }, atlas_sources: sourcesAnswer() },
    });
    const cloudflare = await card("Cloudflare");
    const refresh = within(cloudflare).getByRole("button", { name: "Refresh now" });
    await waitFor(() => expect(reasonOf(refresh)).toContain(WRITE_REASONS.not_served));
    expect(refresh.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(refresh);
    expect(posted).toEqual([]);
  });

  test("holds Refresh now for a reader who is not signed in", async () => {
    const viewer = { login: "", grants: [], handle: "", owner: "", name: "", kind: "", acts: [] };
    mountAtlas("#/company/atlas/sources", <SourcesPage />, grownCompany(), {
      answers: { viewer, atlas_sources: sourcesAnswer() },
    });
    const cloudflare = await card("Cloudflare");
    const refresh = within(cloudflare).getByRole("button", { name: "Refresh now" });
    await waitFor(() => expect(reasonOf(refresh)).toContain(WRITE_REASONS.anonymous));
    expect(refresh.getAttribute("aria-disabled")).toBe("true");
  });

  test("says who asked for a read that waits", async () => {
    mountSources(
      sourcesAnswer({
        sources: [
          sourceStanding("cloudflare", {
            refresh_requested_at: "2026-11-11T08:59:30Z",
            refresh_requested_by: "ada",
          }),
        ],
      }),
    );
    const cloudflare = await card("Cloudflare");
    expect(within(cloudflare).getByText(/^ada asked for a read/)).toBeTruthy();
  });

  test("says so when the company connects no source", async () => {
    mountSources(sourcesAnswer({ sources: [] }));
    expect(await screen.findByText("No source is connected")).toBeTruthy();
  });
});

describe("the band", () => {
  test("leads from Where it comes from to the sources", async () => {
    mountAtlas("#/company/atlas", <Atlas />, grownCompany(), { answers: { viewer: ADA } });
    const link = await screen.findByRole("link", { name: "Sources" });
    expect(link.getAttribute("href")).toBe("#/company/atlas/sources");
  });
});
