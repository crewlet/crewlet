/**
 * Who gets a server: the grant the picker writes, and the agents a team
 * lists, which are every agent the engine's grant to that team reaches.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test } from "vitest";
import { Store } from "~/protocol/index.ts";
import { TestClient } from "~/test/client.tsx";
import { withDerived } from "~/test/org.ts";
import {
  accessEntity,
  accessOf,
  EVERY,
  GrantPicker,
  sameAccess,
  type Access,
} from "./GrantPicker.tsx";

/** Engineering with Backend inside it, a product agent, and an agent at the root. */
function company(): Store {
  const store = new Store();
  store.applyOrg(
    withDerived({
      roles: [{ name: "Chief of Staff", handle: "cos" }],
      units: [
        {
          id: "eng",
          name: "Engineering",
          roles: [{ name: "Tech Lead", handle: "tech-lead" }],
          children: [
            {
              id: "backend",
              name: "Backend",
              roles: [{ name: "Backend Engineer", handle: "backend-dev" }],
            },
          ],
        },
        { id: "product", name: "Product", roles: [{ name: "Product Manager", handle: "pm" }] },
      ],
    }),
  );
  return store;
}

let written: Access = EVERY;

function Harness({ start }: { start: Access }) {
  const [value, setValue] = useState(start);
  return (
    <GrantPicker
      label="Who gets Linear"
      value={value}
      onChange={(next) => {
        written = next;
        setValue(next);
      }}
    />
  );
}

function mount(start: Access = EVERY) {
  written = start;
  render(
    <TestClient store={company()}>
      <Harness start={start} />
    </TestClient>,
  );
}

afterEach(() => cleanup());

test("every agent writes no grant, and the count is every agent the chart holds", () => {
  mount();
  expect(screen.getByRole("radio", { name: /Every agent/ })).toBeDefined();
  expect(screen.getByText("4 today, and anyone who joins")).toBeDefined();
  expect(accessEntity(EVERY)).toBeUndefined();
});

// A TEAM'S GRANT REACHES THE TEAMS INSIDE IT, as the engine reads a unit key,
// so ticking Engineering ticks Backend's engineer too. Mutation: list a
// team's own members alone, and the backend engineer is left unticked.
test("a team lists every agent beneath it, and ticking it writes its key", () => {
  mount();
  fireEvent.click(screen.getByRole("radio", { name: /Chosen teams and agents/ }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Engineering" }));
  expect(written).toEqual({ every: false, units: ["eng"], seats: [], except: [] });
  expect(screen.getByText("2 of 4 agents")).toBeDefined();
  const backend = screen.getAllByRole("checkbox", { name: "Backend Engineer" });
  expect(backend.length).toBeGreaterThan(0);
  for (const box of backend) expect((box as HTMLInputElement).checked).toBe(true);
});

test("an agent left out of a chosen team is written as an exception, one outside it as a seat", () => {
  mount({ every: false, units: ["eng"], seats: [], except: [] });
  const eng = screen.getByRole("checkbox", { name: "Engineering" }).closest("li")!;
  fireEvent.click(within(eng as HTMLElement).getAllByRole("checkbox", { name: "Tech Lead" })[0]!);
  expect(written.except).toEqual(["tech-lead"]);
  // THE OUTERMOST TEAMS OPEN AT FIRST: Product's agents are there to tick.
  fireEvent.click(screen.getByRole("checkbox", { name: "Product Manager" }));
  expect(written).toEqual({ every: false, units: ["eng"], seats: ["pm"], except: ["tech-lead"] });
  expect(accessEntity(written)).toEqual({ units: ["eng"], seats: ["pm"], except: ["tech-lead"] });
});

test("an entry's access reads back as the form holds it, and naming nobody is written as nobody", () => {
  expect(accessOf(undefined)).toEqual(EVERY);
  expect(accessOf({ units: ["eng"], except: ["tech-lead"] })).toEqual({
    every: false,
    units: ["eng"],
    seats: [],
    except: ["tech-lead"],
  });
  expect(accessEntity({ every: false, units: [], seats: [], except: [] })).toEqual({});
  expect(
    sameAccess(accessOf({ seats: ["pm"] }), { every: false, units: [], seats: ["pm"], except: [] }),
  ).toBe(true);
});
