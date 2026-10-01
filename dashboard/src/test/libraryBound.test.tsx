/**
 * The testing library is bound to the case in a file that never imports the
 * binding.
 *
 * Most suites render, wait and click through the library alone and never
 * import `inCase.ts`; their waits and events are bound because `setup.ts`
 * loads the binding wherever there is a document. Every other suite that
 * proves the binding imports it, which binds the library whatever `setup.ts`
 * does — so this file is the one place a setup that stopped loading it goes
 * red. IT MUST NOT IMPORT `inCase.ts`, or it proves nothing.
 */

import { fireEvent } from "@testing-library/react";
import { expect, test } from "vitest";

let wake: () => void = () => {};
// Chained while the file loads, outside every case: a dispatch from no
// case's context while a case runs is refused by the binding, and reaches the
// page through the bare library.
const fromNoCase = new Promise<void>((resolve) => {
  wake = resolve;
})
  .then(() => {
    fireEvent.click(document.body);
  })
  .then(
    () => "returned",
    (cause: unknown) => (cause instanceof Error ? cause.message : String(cause)),
  );

test("an event from no case's context is refused, though nothing here imports the binding", async () => {
  wake();
  expect(await fromNoCase).toMatch(
    /^fireEvent: called while a case runs but from none's async context/,
  );
});
