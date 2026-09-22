/**
 * A filter's three states, and the one that was unreachable.
 *
 * A control whose options are `prose` / `skills` / everything, or `open` /
 * `closed` / everything, has a default that is one of the NAMED options and a
 * third state that means "do not narrow at all". [useParam] spells "back to the
 * default" as `null` and hands anything else through as a value — so the third
 * state arrives at [Navigator.filter] as the empty string.
 *
 * `filter` deleted on both. Deleting is how the default is spelled, so choosing
 * the third state wrote NOTHING to the address, the next render read the key
 * back as absent, resolved it to the non-empty fallback, and the control
 * snapped to the option beside the one that was pressed. The tracker's All
 * scope and `DataGrid`'s third sort state — click a sorted column twice on a
 * grid with a `defaultSort` and the unsorted state was simply the default again
 * — were each unreachable, and nothing failed, because a control that silently
 * refuses is perfectly well-typed. (The knowledge browse's own All is spelled
 * `all` rather than `""`, because an ANCHOR has to carry it and `buildHash`
 * drops an empty value from a query record; this rule is what makes the two
 * CLICKED ones work.)
 *
 * ASSERTED IN BOTH DIRECTIONS: a `filter` that never deleted would leave a
 * `?unit=` on every screen a reader had ever narrowed and then un-narrowed.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { Router, useParam } from "./router.tsx";

let kind: string | null = null;
let setKind: ((value: string) => void) | null = null;

/** One screen with one three-state filter on it, defaulting to a named value. */
function Probe() {
  const [value, set] = useParam("kind", "prose");
  kind = value;
  setKind = set;
  return null;
}

beforeEach(() => {
  history.replaceState(null, "", "#/knowledge");
});

afterEach(() => {
  cleanup();
  kind = null;
  setKind = null;
  history.replaceState(null, "", "#/");
});

test("a filter's third state is reachable, and reads back as itself", () => {
  render(
    <Router>
      <Probe />
    </Router>,
  );
  expect(kind).toBe("prose");

  // A NAMED OPTION is an ordinary value.
  act(() => setKind!("skills"));
  expect(location.hash).toBe("#/knowledge?kind=skills");
  expect(kind).toBe("skills");

  // THE THIRD STATE is written as an empty value rather than dropped, so the
  // next read answers "" instead of falling back to `prose`.
  act(() => setKind!(""));
  expect(location.hash).toBe("#/knowledge?kind=");
  expect(kind).toBe("");
});

test("and the default still deletes the key rather than spelling it", () => {
  // THE OTHER DIRECTION. `useParam` maps "back to the fallback" to `null`, and
  // `null` is the only delete — so the address a reader arrives on and the
  // address they get by choosing the default are the same place, rather than
  // two spellings of it.
  history.replaceState(null, "", "#/knowledge?kind=skills");
  render(
    <Router>
      <Probe />
    </Router>,
  );
  expect(kind).toBe("skills");
  act(() => setKind!("prose"));
  expect(location.hash).toBe("#/knowledge");
  expect(kind).toBe("prose");
});
