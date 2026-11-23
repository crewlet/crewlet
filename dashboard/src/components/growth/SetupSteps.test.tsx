/**
 * A person sees exactly what to add at the vendor, and where the engine
 * stands on it; and a consent sign-in says who gave it, or why it was refused.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { AddIdentity, ConsentStep } from "./SetupSteps.tsx";

afterEach(cleanup);

test("an identity step shows the email to copy, the role, where, and whether it is added", () => {
  render(
    <AddIdentity
      product="Google Analytics"
      why="reads reports and nothing else"
      step={{
        email: "crewlet-gr-nimbus-ab12@identity.example.iam.gserviceaccount.com",
        role: { id: "predefinedRoles/viewer", name: "Viewer" },
        where: { words: "Admin, Property access management", url: "https://analytics.google.com/" },
        level: "company",
        state: "missing",
      }}
    />,
  );
  expect(
    screen.getByText("crewlet-gr-nimbus-ab12@identity.example.iam.gserviceaccount.com"),
  ).toBeTruthy();
  expect(screen.getByText("Viewer")).toBeTruthy();
  expect(
    screen.getByRole("link", { name: /Admin, Property access management/ }).getAttribute("href"),
  ).toBe("https://analytics.google.com/");
  expect(screen.getByText("Not added yet")).toBeTruthy();
  expect(screen.getByText(/company's read identity/)).toBeTruthy();
});

test("a seat's identity is named by the seat, and is marked added once the engine sees it", () => {
  render(
    <AddIdentity
      product="Google Ads"
      step={{
        email: "crewlet-growth-ab12cd@identity.example.iam.gserviceaccount.com",
        role: { id: "STANDARD", name: "Standard" },
        where: { words: "Admin, Access and security", url: "" },
        level: "seat",
        seat: "Growth Lead",
        state: "present",
      }}
    />,
  );
  expect(screen.getByText("Added")).toBeTruthy();
  expect(screen.getByText("Growth Lead's own identity at Google Ads.")).toBeTruthy();
  expect(screen.queryByRole("link")).toBeNull();
});

test("a consent step starts the sign-in, and says who signed in or why it was refused", () => {
  const onSignIn = vi.fn();
  const { rerender } = render(
    <ConsentStep
      product="AdSense"
      scope="read access to AdSense"
      state={{ kind: "missing" }}
      onSignIn={onSignIn}
    />,
  );
  fireEvent.click(screen.getByRole("button", { name: "Sign in with Google" }));
  expect(onSignIn).toHaveBeenCalled();
  rerender(
    <ConsentStep
      product="AdSense"
      scope="read access to AdSense"
      state={{ kind: "signed_in", by: "Avery Admin", at: "21 Nov 2026" }}
      onSignIn={onSignIn}
    />,
  );
  expect(screen.getByText(/Signed in by Avery Admin on 21 Nov 2026/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Sign in again" })).toBeTruthy();
  rerender(
    <ConsentStep
      product="AdSense"
      scope="read access to AdSense"
      state={{ kind: "rejected", words: "invalid_grant." }}
      onSignIn={onSignIn}
    />,
  );
  expect(screen.getByText("Refused")).toBeTruthy();
  expect(screen.getByText(/revoked or expired: invalid_grant\./)).toBeTruthy();
});

test("a reader who may not start the sign-in is told why", () => {
  render(
    <ConsentStep
      product="AdMob"
      scope="read access to AdMob"
      state={{ kind: "missing" }}
      onSignIn={() => {}}
      disabledReason="Only an admin signs in for the company."
    />,
  );
  expect(
    screen.getByRole("button", { name: "Sign in with Google" }).getAttribute("aria-disabled"),
  ).toBe("true");
});

test("an identity the engine has not looked for yet says so", () => {
  render(
    <AddIdentity
      product="Google Analytics"
      step={{
        email: "a@b.example",
        role: { id: "viewer", name: "Viewer" },
        where: { url: "", words: "Admin" },
        level: "company",
        state: "unknown",
      }}
    />,
  );
  expect(screen.getByText("Not checked yet")).toBeTruthy();
});
