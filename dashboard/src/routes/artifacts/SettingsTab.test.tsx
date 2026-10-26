/**
 * The Settings panel's details as the assistant reads and changes them: the
 * title, description and icon as typed, set through the panel's own fields
 * and held to their bounds, with Save left to the editor.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { SettingsTab } from "./SettingsTab.tsx";
import { context, draftField, openFrame, setDraft } from "./draftTesting.ts";

const role = vi.hoisted(() => ({ can: true }));
vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return {
    ...actual,
    useArtifactAccess: () =>
      role.can ? { can: true, as: "ada", acts: [] } : { can: false, reason: "Editors rename it." },
  };
});

afterEach(cleanup);

const DETAIL = {
  id: "a-1",
  kind: "doc",
  title: "Runbook",
  description: "",
  icon: "",
  head_number: 4,
  capabilities: [],
  role: "owner",
  owner: "ada",
  status: "live",
  created_by: { name: "ada", kind: "human" },
  updated_by: { name: "ada", kind: "human" },
  created_at: "2026-10-17T10:00:00Z",
  updated_at: "2026-10-18T10:00:00Z",
} as unknown as ArtifactDetail;

function mount() {
  render(
    <TestClient>
      <Router>
        <LayerHost>
          <ToastProvider>
            <SettingsTab detail={DETAIL} onChanged={() => {}} />
          </ToastProvider>
        </LayerHost>
      </Router>
    </TestClient>,
  );
}

test("the details are the assistant's to read, and a draft.set fills the fields for Save", () => {
  role.can = true;
  openFrame("artifact");
  mount();
  expect(context().draft).toMatchObject({ kind: "client", id: "a-1", revision: "4" });
  expect(draftField("title")).toMatchObject({ value: "Runbook", editable: true });
  expect(
    setDraft({ title: "On-call runbook", description: "What to do when paged." }),
  ).toMatchObject({ outcome: "applied" });
  expect(screen.getByRole("textbox", { name: "Title" })).toHaveProperty("value", "On-call runbook");
  expect(setDraft({ title: " " })).toMatchObject({
    outcome: "invalid",
    reason: "An artifact has a title.",
  });
});

test("a reader who may not rename it holds no draft", () => {
  role.can = false;
  openFrame("artifact");
  mount();
  expect(context().draft).toBeUndefined();
});
