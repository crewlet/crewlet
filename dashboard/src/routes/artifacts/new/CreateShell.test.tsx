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
