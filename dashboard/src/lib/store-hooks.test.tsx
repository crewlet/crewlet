/**
 * The connection posture as the frame, every query and every control read it.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { ClientContext, useConnection, type Client } from "./store-hooks.ts";

afterEach(cleanup);

/*
 * THE HEALTH PUSH ARRIVES EVERY FIVE SECONDS AND LEAVES THE POSTURE AS IT WAS, so
 * it draws nobody who reads the posture, and a change of the posture draws them.
 * Mutation: read it through `useSlice`, whose every push is a new object.
 */
test("a health push that leaves the connection as it was draws no reader of it", () => {
  const store = new Store();
  const client = { store, socket: null } as unknown as Client;
  store.applyHealth(healthFrame());
  let draws = 0;
  let connected: boolean | undefined;
  function Reader() {
    connected = useConnection().connected;
    draws += 1;
    return null;
  }
  render(
    <ClientContext.Provider value={client}>
      <Reader />
    </ClientContext.Provider>,
  );
  const settled = draws;
  expect(connected).toBe(true);
  act(() => store.applyHealth(healthFrame()));
  expect(draws).toBe(settled);
  act(() => store.setConnected(false));
  expect(draws).toBe(settled + 1);
  expect(connected).toBe(false);
});
