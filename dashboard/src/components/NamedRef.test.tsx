/**
 * What an engine line names, read by the one parser (`lib/inlineTokens.ts`,
 * rules 6a and 8), resolved in one ask per tick (`lib/namedRefs.ts`) and drawn
 * by the one renderer (`NamedRef`, `EngineLine`), on every surface that shows
 * such a line.
 */

import { act, cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";

import { EngineLine } from "./InlineToken.tsx";
import { EventGrid } from "./EventGrid.tsx";
import { EventRow } from "./common.tsx";
import { NamedRef } from "./NamedRef.tsx";
import { TurnWhatCell } from "~/app/frame/cells.tsx";
import { Router } from "~/app/router.tsx";
import { inlineTokens, locate } from "~/lib/inlineTokens.ts";
import { ClientContext, type Client } from "~/lib/store-hooks.ts";
import { Store, type FeedRow, type ReferencesAnswer } from "~/protocol/index.ts";
import {
  drawnRef,
  expectNamedLinks,
  namedClient,
  NAMED_LINE,
  NAMED_REFS,
  WithClient,
} from "~/test/namedRefs.tsx";

afterEach(cleanup);

/** Draws `node` in a client, and waits for the engine's answer to land. */
async function drawn(node: React.ReactElement) {
  const { client, asks } = namedClient();
  const out = render(
    <Router>
      <WithClient client={client}>{node}</WithClient>
    </Router>,
  );
  await waitFor(() => expect(asks.length).toBeGreaterThan(0));
  await act(async () => {});
  return { ...out, asks };
}

describe("the parser", () => {
  test("a line's refs are found by their words, left to right, each after the last", () => {
    const refs = [
      { kind: "work_item", id: "a", text: "ENG-1" },
      { kind: "work_item", id: "b", text: "ENG-1" },
      { kind: "page", id: "p", text: "absent" },
    ];
    const found = locate("ENG-1 then ENG-1", refs);
    expect(found.map((t) => [t.at, t.end])).toEqual([
      [0, 5],
      [11, 16],
    ]);
  });

  test("what a line declares wins what the prose rules would read there", () => {
    const tokens = inlineTokens("handed ENG-287 over", undefined, undefined, undefined, [
      { kind: "work_item", id: "0198-task", text: "ENG-287" },
    ]);
    expect(tokens).toHaveLength(1);
    expect(tokens[0]).toMatchObject({ kind: "named", guessed: false, at: 7, end: 14 });
  });

  test("a Gitlet change in prose is a guess, by the engine's grammar of a repository", () => {
    const kinds = (text: string) =>
      inlineTokens(text).map((t) => (t.kind === "named" ? `${t.ref.kind}:${t.ref.id}` : t.kind));
    expect(kinds("opened web!42 and api.v2!7")).toEqual(["code_change:web!42", "code_change:api.v2!7"]);
    // A two-segment path is a code host's merge request, never a Gitlet change.
    expect(kinds("see group/project!7")).toEqual(["reference"]);
    // No repository is named `Web` or `-x`, and no change is numbered 0.
    expect(kinds("Web!42 -x!3 web!0 wow!")).toEqual([]);
  });
});

describe("the renderer", () => {
  test("what the reader may open links to its page with its title; what is gone is its marked words", async () => {
    const { container, asks } = await drawn(<EngineLine text={NAMED_LINE} refs={NAMED_REFS} />);
    await waitFor(() => expect(drawnRef(container, "ENG-287").tagName).toBe("A"));
    expectNamedLinks(container);
    // ONE ASK for every reference the line names.
    expect(asks).toEqual([NAMED_REFS.map((r) => `${r.kind}:${r.id}`)]);
  });

  test("a reference the engine has not answered yet is its marked words, never a link", () => {
    const pending: Client = {
      store: new Store(),
      socket: { query: () => new Promise(() => {}) } as unknown as Client["socket"],
    };
    const { container } = render(
      <WithClient client={pending}>
        <EngineLine text={NAMED_LINE} refs={NAMED_REFS} />
      </WithClient>,
    );
    expect(container.querySelector("a")).toBeNull();
    expect(drawnRef(container, "ENG-287").querySelector(".crewlet-inline-ref__mark")).not.toBeNull();
  });

  test("inside a row that is itself a link, a reference is never one", async () => {
    const { container } = await drawn(<EngineLine text={NAMED_LINE} refs={NAMED_REFS} links="text" />);
    expect(container.querySelector("a")).toBeNull();
    expect(drawnRef(container, "Runbook").getAttribute("title")).toBe("Deploy runbook");
  });

  test("a kind this build does not know is its words, and is asked about nowhere", () => {
    const { client, asks } = namedClient();
    const { container } = render(
      <WithClient client={client}>
        <NamedRef named={{ kind: "galaxy", id: "m31", text: "Andromeda" }} />
      </WithClient>,
    );
    expect(asks).toEqual([]);
    expect(container.textContent).toBe("Andromeda");
    expect(container.querySelector(".crewlet-inline-ref")).toBeNull();
  });

  test("a guess the engine opens is a link, and one it does not is plain words", async () => {
    const { container } = await drawn(<EngineLine text="see api!7 and foo!9" refs={undefined} />);
    await waitFor(() => expect(drawnRef(container, "api!7").tagName).toBe("A"));
    expect(drawnRef(container, "api!7").getAttribute("href")).toBe("#/gitlet/repos/api/changes/7");
    expect(container.textContent).toContain("foo!9");
    expect(() => drawnRef(container, "foo!9")).toThrow();
  });
});

describe("the batch", () => {
  test("every reference drawn in one tick is one ask, chunked at the engine's bound", async () => {
    const refs = Array.from({ length: 150 }, (_, i) => ({ kind: "turn", id: `t-${i}`, text: `t-${i}` }));
    const { asks } = await drawn(
      <>
        {refs.map((r) => (
          <NamedRef key={r.id} named={r} />
        ))}
      </>,
    );
    expect(asks.map((a) => a.length)).toEqual([100, 50]);
  });

  test("a reference the engine could not settle is asked again when it is drawn again", async () => {
    const asks: string[][] = [];
    const answers: ReferencesAnswer[] = [
      { references: [], unresolved: ["page:page-1"] },
      { references: [{ kind: "page", id: "page-1", title: "Runbook" }], unresolved: [] },
    ];
    const client: Client = {
      store: new Store(),
      socket: {
        query: (_: string, params: { refs: string[] }) => {
          asks.push(params.refs);
          return Promise.resolve(answers[asks.length - 1]);
        },
      } as unknown as Client["socket"],
    };
    const page = { kind: "page", id: "page-1", text: "Runbook" };
    const first = render(
      <WithClient client={client}>
        <NamedRef named={page} />
      </WithClient>,
    );
    await waitFor(() => expect(asks).toHaveLength(1));
    await act(async () => {});
    expect(first.container.querySelector("a")).toBeNull();
    first.unmount();
    const again = render(
      <WithClient client={client}>
        <NamedRef named={page} />
      </WithClient>,
    );
    await waitFor(() => expect(again.container.querySelector("a")?.getAttribute("href")).toBe("#/doclet/pages/page-1"));
    expect(asks).toHaveLength(2);
  });
});

describe("every surface that shows an engine line", () => {
  const event = (over: Partial<FeedRow> = {}): FeedRow =>
    ({
      id: "e-1",
      type: "work_pickup",
      timestamp: "2026-11-19T10:00:00Z",
      source: "SRE",
      actor: "SRE",
      summary: `SRE ${NAMED_LINE}`,
      refs: NAMED_REFS,
      category: "agent",
      ...over,
    }) as FeedRow;

  test("a card's event row (Live, Home)", async () => {
    const { container } = await drawn(<EventRow event={event()} />);
    await waitFor(() => expect(drawnRef(container, "ENG-287").tagName).toBe("A"));
    expectNamedLinks(container);
    // THE ROW STILL OPENS ITS EVENT, and no link is inside another.
    expect(container.querySelector("a.feed-row__open")?.getAttribute("href")).toBe("#/live/events/e-1");
    expect(container.querySelector("a a")).toBeNull();
  });

  test("the event log", async () => {
    const { container } = await drawn(<EventGrid events={[event()]} label="Events" />);
    await waitFor(() => expect(drawnRef(container, "ENG-287").tagName).toBe("A"));
    expectNamedLinks(container);
  });

  test("a turn's account in a list (prose): the fallback reads a change, and its item is a task", async () => {
    const { container } = await drawn(
      <TurnWhatCell
        summary="Shipped ENG-12 through api!7"
        item={{ backend: "native", id: "0198-task", key: "ENG-287", project: "ENG" }}
      />,
    );
    await waitFor(() => expect(drawnRef(container, "api!7").tagName).toBe("A"));
    // THE TURN'S OWN ITEM is a task reference by its id.
    expect(drawnRef(container, "ENG-287").getAttribute("href")).toBe("#/worklet/0198-task");
  });
});

// Outside a client nothing is asked, and a line is its marked words.
test("outside a client a line's references are its marked words", () => {
  const { container } = render(
    <ClientContext.Provider value={null}>
      <EngineLine text={NAMED_LINE} refs={NAMED_REFS} />
    </ClientContext.Provider>,
  );
  expect(container.textContent).toContain("ENG-287");
  expect(container.querySelector("a")).toBeNull();
});
