/**
 * The pieces every part of the MCP tools pages shares, where a piece decides
 * something of its own.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { TestClient } from "~/test/client.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { AutoAcceptSwitch } from "./parts.tsx";

afterEach(() => cleanup());

function mount(checked: boolean, onChange: (on: boolean) => unknown) {
  render(
    <TestClient>
      <LayerHost>
        <AutoAcceptSwitch server="Linear" checked={checked} waiting={null} onChange={onChange} />
      </LayerHost>
    </TestClient>,
  );
}

// TURNING AUTO-ACCEPT ON ASKS FIRST wherever the switch stands (the add and edit
// flow has no count, so it says any waiting change), and Cancel changes
// nothing; turning it off does not ask. Mutation: call onChange on the switch's
// first press, and this is red.
test("turning auto-accept on asks first, and off does not", async () => {
  const onChange = vi.fn();
  mount(false, onChange);
  fireEvent.click(screen.getByRole("switch", { name: "Accept tool changes automatically" }));
  const asked = screen.getByRole("dialog");
  expect(asked.textContent).toContain("Any change waiting now is accepted too.");
  fireEvent.click(within(asked).getByRole("button", { name: "Cancel" }));
  expect(onChange).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("switch", { name: "Accept tool changes automatically" }));
  fireEvent.click(screen.getByRole("button", { name: "Accept automatically" }));
  await afterMicrotasks();
  expect(onChange).toHaveBeenLastCalledWith(true);
  cleanup();
  onChange.mockReset();
  mount(true, onChange);
  fireEvent.click(screen.getByRole("switch", { name: "Accept tool changes automatically" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(onChange).toHaveBeenLastCalledWith(false);
});
