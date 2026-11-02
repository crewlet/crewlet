/**
 * What the config field adds to uilet's ValueField, which is only what the
 * engine means.
 *
 * Every rule about a value on its way through (the url affix, whitespace,
 * masking, the completion list's keys, a choice's hint) is the design
 * system's and is held by its own suite. What is checked here is the wiring
 * this adapter owns: the engine's reference grammar, the company's sealed
 * entries reaching the completion, a refusal drawn as its problems, and a
 * dense form's help line.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { useState } from "react";
import { ConfigField, HelperOnFocus } from "./ConfigField.tsx";

afterEach(cleanup);

// A REFERENCE IS NOT A CREDENTIAL, so masking it defeats the point of showing
// it. The field held `${JIRA_TOKEN}` and rendered sixteen dots, which is the
// same thing an operator saw before the engine sent the reference at all.
test("a secret holding a reference is readable", () => {
  render(<ConfigField label="API token" kind="secret" value="${JIRA_TOKEN}" onChange={() => {}} />);
  const input = screen.getByLabelText("API token") as HTMLInputElement;
  expect(input.type).toBe("text");
  expect(input.value).toBe("${JIRA_TOKEN}");
});

// AND EVERYTHING ELSE IN THAT FIELD IS STILL A CREDENTIAL. The type is what
// keeps it out of an autofill store and out of a screenshot.
test("a secret holding anything else stays masked", () => {
  const cases = ["ATATT-real-credential", "", "${HOST}/x", "${}", "prefix ${NAME}"];
  for (const value of cases) {
    cleanup();
    render(<ConfigField label="API token" kind="secret" value={value} onChange={() => {}} />);
    expect((screen.getByLabelText("API token") as HTMLInputElement).type).toBe("password");
  }
});

/** Completing a `${NAME}` from the company's sealed entries. */
const held = ["GITHUB_TOKEN", "GH_WEBHOOK_SECRET", "DATADOG_APP_KEY"];

/** A controlled field, because the completion writes through onChange. */
function Editable({ secrets = held }: { secrets?: string[] }) {
  const [value, setValue] = useState("");
  return (
    <ConfigField
      label="Webhook secret"
      kind="id"
      value={value}
      onChange={setValue}
      secrets={secrets}
    />
  );
}

// TYPING `$` OPENS THE LIST, and typing more narrows it.
test("typing a dollar offers the company's sealed entries", () => {
  render(<Editable />);
  const input = screen.getByLabelText("Webhook secret") as HTMLInputElement;

  // NOTHING BEFORE THE `$`. A list over an empty box would be a popup
  // nobody asked for on every field of every form.
  expect(screen.queryByRole("listbox")).toBeNull();

  fireEvent.change(input, { target: { value: "$" } });
  fireEvent.keyUp(input, { key: "$" });
  expect(screen.getAllByRole("option").length).toBe(held.length);

  // NARROWED, and ordered closest first: GH_WEBHOOK_SECRET starts with the
  // query and GITHUB_TOKEN merely contains its letters.
  fireEvent.change(input, { target: { value: "$GH" } });
  fireEvent.keyUp(input, { key: "H" });
  const shown = screen.getAllByRole("option").map((o) => o.textContent);
  expect(shown).toEqual(["GH_WEBHOOK_SECRET", "GITHUB_TOKEN"]);
});

// THE LIST IS ASKED FOR WHEN IT IS ABOUT TO BE SHOWN.
//
// There is no push for the secret store, so a list read when a form opened
// goes stale the moment somebody adds an entry in another tab, which is
// exactly what a person does on finding the name they wanted is not there.
// Asked on the way in, the list is current at the one moment that matters.
test("the caller is asked to refresh when the list opens", () => {
  const asked = vi.fn();
  function Asking() {
    const [value, setValue] = useState("");
    return (
      <ConfigField
        label="Webhook secret"
        kind="id"
        value={value}
        onChange={setValue}
        secrets={held}
        onSecretsNeeded={asked}
      />
    );
  }
  render(<Asking />);
  const input = screen.getByLabelText("Webhook secret") as HTMLInputElement;

  // NOT BEFORE. A field nobody is completing in costs nothing.
  fireEvent.change(input, { target: { value: "acme" } });
  fireEvent.keyUp(input, { key: "e" });
  expect(asked).not.toHaveBeenCalled();

  fireEvent.change(input, { target: { value: "acme $" } });
  fireEvent.keyUp(input, { key: "$" });
  expect(asked).toHaveBeenCalledTimes(1);

  // ON THE WAY IN ONLY: typing further into an open list is not a second
  // opening, and asking per keystroke would spend a request on each.
  fireEvent.change(input, { target: { value: "acme $GH" } });
  fireEvent.keyUp(input, { key: "H" });
  expect(asked).toHaveBeenCalledTimes(1);
});

// A REFUSAL IS A LIST OF PROBLEMS, NOT A SENTENCE.
//
// The engine refuses with several problems joined by newlines, each opening
// with the config path it names. HTML collapses those newlines, so a field
// that put the string in its error line showed one run-on paragraph in which
// the second problem's path read as the end of the first one's sentence — and
// the path itself read as prose, on the one form where a reader has to be able
// to find it in their document. Wired through [withProblems], which is what
// gives the row a node rather than a string.
test("a refusal is drawn as its problems, each path picked out", () => {
  render(
    <ConfigField
      label="Site"
      kind="url"
      value=""
      onChange={() => {}}
      error={
        "integrations.confluence: required value missing: give url or cloud_id\n" +
        "integrations.confluence.webhook_secret: required value missing"
      }
    />,
  );
  const line = document.getElementById(
    (screen.getByLabelText("Site").getAttribute("aria-describedby") ?? "").split(" ").pop() ?? "",
  );
  expect(line).not.toBeNull();
  // TWO PROBLEMS ARE TWO LINES, so neither runs into the other.
  expect(line!.querySelectorAll("li")).toHaveLength(2);
  // AND EACH PATH WEARS THE FACE A CONFIG PATH WEARS, rather than reading as
  // the prose around it.
  expect(screen.getByText("integrations.confluence").tagName).toBe("CODE");
  expect(screen.getByText("integrations.confluence.webhook_secret").tagName).toBe("CODE");
});

// A DENSE FORM DRAWS ITS HELP ONLY UNDER THE FIELD THE READER IS IN, and says
// so once, through the context, rather than on each of its fields.
test("a dense form's help line follows focus", () => {
  const { container } = render(
    <HelperOnFocus.Provider value>
      <ConfigField label="Role" value="" onChange={() => {}} help="What this seat does." />
    </HelperOnFocus.Provider>,
  );
  expect(container.querySelector(".crewlet-form-field--helper-on-focus")).not.toBeNull();
});
