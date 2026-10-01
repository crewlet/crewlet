/**
 * The builder context refuses to be read outside the Builder, and a view's
 * registration lasts exactly as long as the view.
 */

import { cleanup, render } from "~/test/inCase.ts";
import { afterEach, expect, test, vi } from "vitest";
import {
  BuilderContext,
  useBuilder,
  useBuilderView,
  type BuilderApi,
  type BuilderViewHandle,
} from "./BuilderContext.tsx";

afterEach(cleanup);

// A VIEW OUTSIDE THE BUILDER IS A WIRING DEFECT. Answering it with a blank
// context would draw an empty chart that looks like a company with no seats.
test("reading the context outside the Builder throws", () => {
  function Reader() {
    useBuilder();
    return null;
  }
  // THE THROW IS EXPECTED, and React reports a render that throws on
  // console.error — which fails any case that did not silence it
  // (`src/test/setup.ts`). Silenced for this render only and put back: left
  // in place it swallowed every error the rest of the file printed.
  const quiet = vi.spyOn(console, "error").mockImplementation(() => {});
  try {
    expect(() => render(<Reader />)).toThrow(/outside the org builder/);
  } finally {
    quiet.mockRestore();
  }
});

// A handle left registered after its view unmounted would take focus calls
// meant for the view that replaced it.
test("a view is registered while mounted and unregistered when it goes", () => {
  const unregister = vi.fn();
  const registerView = vi.fn((_handle: BuilderViewHandle) => unregister);
  const api = { registerView } as unknown as BuilderApi;
  const handle: BuilderViewHandle = { focusNode() {}, expandAll() {}, collapseAll() {} };
  function View() {
    useBuilderView(handle);
    return null;
  }
  const view = render(
    <BuilderContext.Provider value={api}>
      <View />
    </BuilderContext.Provider>,
  );
  expect(registerView).toHaveBeenCalledWith(handle);
  expect(unregister).not.toHaveBeenCalled();
  view.unmount();
  expect(unregister).toHaveBeenCalledTimes(1);
});
