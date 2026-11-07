/**
 * The page's live reading as a store: what a tick of the clock costs a reader
 * of it when nothing a reader draws has changed.
 */

import { act, cleanup, render } from "@testing-library/react";
import { useState, type ReactNode } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { useClient } from "~/lib/store-hooks.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { fixtureCompany } from "./builder/model/testkit.ts";
import { checkedEdit } from "./builder/testState.ts";
import { BuilderHarness, builderSpies, harnessProbe } from "./builder/viewTestkit.tsx";
import { LiveStructure, useLiveValue, useSeatLive } from "./structureLive.tsx";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

/** The running company in the store, as the org push leaves it. */
function Running({ children }: { children: ReactNode }) {
  const { store } = useClient();
  useState(() => store.applyOrg(withDerived(fixtureCompany()) as unknown as OrgProjection));
  return <>{children}</>;
}

/*
 * THE CLOCK TICKS EVERY SECOND AND THE READING IS TAKEN AGAIN WITH IT, so the org's
 * index and a seat's reading have to come out as they went in, or every reader draws
 * every second with nothing on screen changed. Mutation: build the index with the
 * clock, or let the store take every new reading as it comes.
 */
test("a tick of the clock draws no reader of the reading", () => {
  vi.useFakeTimers();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  let draws = 0;
  function Reader() {
    useSeatLive("dev");
    useLiveValue((live) => live?.index ?? null);
    draws += 1;
    return null;
  }
  render(
    <BuilderHarness
      initial={checkedEdit(fixtureCompany())}
      spies={builderSpies()}
      probe={harnessProbe()}
    >
      <Running>
        <LiveStructure>
          <Reader />
        </LiveStructure>
      </Running>
    </BuilderHarness>,
  );
  const settled = draws;
  expect(settled).toBeGreaterThan(0);
  act(() => {
    vi.advanceTimersByTime(5000);
  });
  expect(draws).toBe(settled);
});
