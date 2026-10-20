import { afterEach, expect, test, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { CreateShell } from "./CreateShell.tsx";

afterEach(cleanup);

function mount(node: React.ReactNode) {
  location.hash = "#/artifacts/new/doc";
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>{node}</ToastProvider>
      </LayerHost>
    </Router>,
  );
}

// THE BAR SAYS WHAT IS BEING MADE, as the artifact's own bar will once it is:
// the kind's glyph and "New doc", the way back to the gallery, Cancel, and the
// kind's one action at its end.
test("the bar names the kind, leads back to the gallery and holds the kind's action", () => {
  const start = vi.fn();
  mount(
    <CreateShell kind="doc" action={{ label: "Start the doc", icon: null, onSelect: start }}>
      <p>Body</p>
    </CreateShell>,
  );
  expect(screen.getByRole("heading", { level: 1, name: "New doc" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Back to artifacts" }).getAttribute("href")).toBe(
    "#/artifacts",
  );
  expect(screen.getByText("Body").closest(".artifact-create-body")).not.toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Start the doc" }));
  expect(start).toHaveBeenCalledOnce();
});

test("Cancel returns to the gallery, or runs the kind's own cancel", () => {
  mount(<CreateShell kind="sheet">{null}</CreateShell>);
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  expect(location.hash).toBe("#/artifacts");
  cleanup();
  const ask = vi.fn();
  mount(
    <CreateShell kind="slides" onCancel={ask}>
      {null}
    </CreateShell>,
  );
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  expect(ask).toHaveBeenCalledOnce();
  expect(location.hash).toBe("#/artifacts/new/doc");
});

// THE COMMAND KEY WITH ENTER IS THE PAGE'S ACTION, from inside a field too, and
// a held action is not pressed by it: one row of the keymap, bound here, so no
// kind binds its own.
test("the command key with Enter presses the action, and never a held one", () => {
  const start = vi.fn();
  mount(
    <CreateShell kind="doc" action={{ label: "Start the doc", icon: null, onSelect: start }}>
      <input aria-label="Title" />
    </CreateShell>,
  );
  const field = screen.getByLabelText("Title");
  field.focus();
  fireEvent.keyDown(field, { key: "Enter", ctrlKey: true, metaKey: true });
  expect(start).toHaveBeenCalledOnce();
  cleanup();
  const held = vi.fn();
  mount(
    <CreateShell
      kind="doc"
      action={{
        label: "Start the doc",
        icon: null,
        onSelect: held,
        disabledReason: "Give it a title.",
      }}
    >
      {null}
    </CreateShell>,
  );
  fireEvent.keyDown(window, { key: "Enter", ctrlKey: true, metaKey: true });
  expect(held).not.toHaveBeenCalled();
});

test("on a narrow bar the action takes its short word and keeps its full one as its name", () => {
  mount(
    <CreateShell
      kind="design"
      action={{ label: "Create and ask SRE Lead", short: "Create", icon: null, onSelect: vi.fn() }}
    >
      {null}
    </CreateShell>,
  );
  const button = screen.getByRole("button", { name: "Create and ask SRE Lead" });
  expect(button.getAttribute("aria-keyshortcuts")).toMatch(/\+Enter$/);
  expect(button.textContent).toContain("Create");
});
