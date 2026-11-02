/**
 * What may be ASKED of Crewlet's services, as against what is DRAWN: until
 * the org projection arrives nothing is asked (a read sent then for a service
 * the company does not run is refused), while everything is drawn (nothing
 * is hidden on a guess).
 */

import { act, render } from "@testing-library/react";
import { expect, test } from "vitest";
import { Store } from "~/protocol/index.ts";
import { TestClient } from "~/test/client.tsx";
import { withDerived } from "~/test/org.ts";
import { useServicesAsk, useServicesRun, type ServicesRun } from "./services.ts";

function Probe({ out }: { out: { ask?: ServicesRun; run?: ServicesRun } }) {
  out.ask = useServicesAsk();
  out.run = useServicesRun();
  return null;
}

// A COLD LOAD ON A COMPANY RUNNING JIRA asked the tracker for its inbox, its
// queue and its projects before it knew. Mutation: answer asking as drawing
// and the first expectation holds every service asked.
test("nothing is asked until the projection arrives, and then only what the company runs", () => {
  const store = new Store();
  const out: { ask?: ServicesRun; run?: ServicesRun } = {};
  render(
    <TestClient store={store}>
      <Probe out={out} />
    </TestClient>,
  );
  expect(out.ask).toEqual({ tracker: false, knowledge: false, chat: false, code: false });
  expect(out.run).toEqual({ tracker: true, knowledge: true, chat: true, code: true });
  act(() =>
    store.applyOrg(
      withDerived({
        name: "Acme",
        roles: [],
        services: { tracker: "jira", knowledge: "doclet", chat: "chatlet", code: "native" },
      }),
    ),
  );
  expect(out.ask).toEqual({ tracker: false, knowledge: true, chat: true, code: true });
  expect(out.run).toEqual({ tracker: false, knowledge: true, chat: true, code: true });
});

// A NODE RUNNING NO COMPANY names no services: every one is asked, as every
// one is drawn, since none is known not to run.
test("a projection naming no services asks every one", () => {
  const store = new Store();
  store.applyOrg(withDerived({ name: "", roles: [] }));
  const out: { ask?: ServicesRun } = {};
  render(
    <TestClient store={store}>
      <Probe out={out} />
    </TestClient>,
  );
  expect(out.ask).toEqual({ tracker: true, knowledge: true, chat: true, code: true });
});
