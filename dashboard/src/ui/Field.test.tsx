/**
 * The url field's scheme affix.
 *
 * Every config field of this kind is refused without a scheme, so the affix
 * is not decoration: it is the difference between a value that saves and one
 * the engine rejects after the fact.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { useState } from "react";
import { Field } from "./Field.tsx";

afterEach(cleanup);

// Typed the way a person says it out loud, saved the way the config demands.
test("a url field supplies the scheme the config requires", () => {
  const onChange = vi.fn();
  render(<Field label="Jira site" kind="url" value="" onChange={onChange} />);

  const input = screen.getByLabelText("Jira site") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "acme.atlassian.net" } });
  expect(onChange).toHaveBeenCalledWith("https://acme.atlassian.net");
});

// The box shows the address, the affix shows the scheme, and the value is
// both. Showing the whole url in a box already prefixed with https:// would
// read as https://https://acme.atlassian.net.
test("a stored url is shown without the scheme the affix carries", () => {
  render(
    <Field label="Jira site" kind="url" value="https://acme.atlassian.net" onChange={() => {}} />,
  );
  expect((screen.getByLabelText("Jira site") as HTMLInputElement).value).toBe("acme.atlassian.net");
});

// PASTING IS HOW A SITE ADDRESS ARRIVES, out of an address bar, scheme and
// all. Doubling it onto the affix would produce a value nothing accepts.
test("a pasted https url is not doubled onto the affix", () => {
  const onChange = vi.fn();
  render(<Field label="Jira site" kind="url" value="" onChange={onChange} />);

  fireEvent.change(screen.getByLabelText("Jira site"), {
    target: { value: "HTTPS://acme.atlassian.net" },
  });
  expect(onChange).toHaveBeenCalledWith("https://acme.atlassian.net");
});

// THE AFFIX IS A DEFAULT, NOT A CAGE. A Data Center instance reachable only
// over http is a thing the config accepts, and rewriting it to https would
// point the engine at a port nothing answers on.
test("a value carrying http keeps it, and the affix steps aside", () => {
  const onChange = vi.fn();
  const { rerender } = render(<Field label="Jira site" kind="url" value="" onChange={onChange} />);

  fireEvent.change(screen.getByLabelText("Jira site"), {
    target: { value: "http://jira.internal" },
  });
  expect(onChange).toHaveBeenCalledWith("http://jira.internal");

  rerender(<Field label="Jira site" kind="url" value="http://jira.internal" onChange={onChange} />);
  const input = screen.getByLabelText("Jira site") as HTMLInputElement;
  expect(input.value).toBe("http://jira.internal");
  expect(screen.queryByText("https://")).toBeNull();
});

// Only url fields. A token or a handle with https:// in front of it is
// nonsense, and the affix rewrites what it is attached to.
test("no other kind of field wears a scheme", () => {
  const onChange = vi.fn();
  render(<Field label="API token" kind="text" value="" onChange={onChange} />);

  fireEvent.change(screen.getByLabelText("API token"), { target: { value: "abc" } });
  expect(onChange).toHaveBeenCalledWith("abc");
});

// A REFERENCE IS NOT A CREDENTIAL, so masking it defeats the point of showing
// it. The field held `${JIRA_TOKEN}` and rendered sixteen dots, which is the
// same thing an operator saw before the engine sent the reference at all.
test("a secret holding a reference is readable", () => {
  render(<Field label="API token" kind="secret" value="${JIRA_TOKEN}" onChange={() => {}} />);
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
    render(<Field label="API token" kind="secret" value={value} onChange={() => {}} />);
    expect((screen.getByLabelText("API token") as HTMLInputElement).type).toBe("password");
  }
});

// A SINGLE TOKEN TAKES NO WHITESPACE, including from a paste. A space around
// an address is invisible in the box and fatal at the vendor.
test("a single-token field drops whitespace as it is typed or pasted", () => {
  for (const kind of ["url", "id", "email"] as const) {
    cleanup();
    const onChange = vi.fn();
    render(<Field label="Value" kind={kind} value="" onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Value"), {
      target: { value: "  acme.example.com  " },
    });
    const written = onChange.mock.calls[0]?.[0] as string;
    expect(written.endsWith("acme.example.com")).toBe(true);
    expect(/\s/.test(written)).toBe(false);
  }
});

// FREE TEXT KEEPS ITS SPACES, which is why the rule is per kind: a Datadog
// role is "Datadog Read Only Role", and stripping there would store a name
// the vendor has no record of.
test("free text keeps the spaces it is given", () => {
  const onChange = vi.fn();
  render(<Field label="Role" kind="text" value="" onChange={onChange} />);
  fireEvent.change(screen.getByLabelText("Role"), {
    target: { value: "Datadog Read Only Role" },
  });
  expect(onChange).toHaveBeenCalledWith("Datadog Read Only Role");
});

// THE BROWSER MUST NOT GET AN OPINION ABOUT A REFERENCE.
//
// Any field here may hold a `${VAR}` naming a sealed entry rather than a
// value. With type="email" the browser refused the form for a value with no
// "@" in it, so the dialog could not be saved at all over a reference the
// engine resolves correctly. The shape is checked where the value is used.
test("an email field never asserts a shape the browser enforces", () => {
  render(
    <Field
      label="Account email"
      kind="email"
      value="${ATLASSIAN_USER_ACCOUNT_EMAIL}"
      onChange={() => {}}
    />,
  );
  const input = screen.getByLabelText("Account email") as HTMLInputElement;
  expect(input.type).toBe("text");
  expect(input.checkValidity()).toBe(true);
});

/**
 * Completing a `${NAME}` from the company's sealed entries.
 *
 * A field takes either a value or a reference to one, and the reference has
 * to be exact: a name off by a character resolves to nothing, which reads as
 * configured on every surface while the route it feeds refuses every
 * delivery. Nothing on the form knew the names, so getting one right meant
 * opening the Secrets screen in another tab and copying it across.
 */
const held = ["GITHUB_TOKEN", "GH_WEBHOOK_SECRET", "DATADOG_APP_KEY"];

/** A controlled field, because the completion writes through onChange. */
function Editable({ secrets = held }: { secrets?: string[] }) {
  const [value, setValue] = useState("");
  return (
    <Field label="Webhook secret" kind="id" value={value} onChange={setValue} secrets={secrets} />
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

// ARROWS MOVE, ENTER TAKES, and the value becomes a whole reference.
test("a name is chosen with the arrows and Enter", () => {
  render(<Editable />);
  const input = screen.getByLabelText("Webhook secret") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "$GH" } });
  fireEvent.keyUp(input, { key: "H" });

  fireEvent.keyDown(input, { key: "ArrowDown" });
  fireEvent.keyDown(input, { key: "Enter" });

  // THE SECOND NAME, because the first was highlighted and the arrow moved
  // past it, and written whole: the engine resolves a reference only when it
  // is complete.
  expect(input.value).toBe("${GITHUB_TOKEN}");
  expect(screen.queryByRole("listbox")).toBeNull();
});

// TAB TAKES THE HIGHLIGHTED NAME, which is what it means in every other
// completion list, rather than leaving the field with the list open.
test("Tab takes the highlighted name", () => {
  render(<Editable />);
  const input = screen.getByLabelText("Webhook secret") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "$DATA" } });
  fireEvent.keyUp(input, { key: "A" });

  fireEvent.keyDown(input, { key: "Tab" });
  expect(input.value).toBe("${DATADOG_APP_KEY}");
});

// ESCAPE CLOSES THE LIST AND KEEPS WHAT WAS TYPED.
//
// The field lives in a dialog that also closes on Escape, so the key is
// stopped here: one press means "not this name", and losing a half-filled
// form to it would be the worse of the two readings.
test("Escape closes the list without clearing the field", () => {
  render(<Editable />);
  const input = screen.getByLabelText("Webhook secret") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "$GH" } });
  fireEvent.keyUp(input, { key: "H" });
  expect(screen.getByRole("listbox")).toBeTruthy();

  fireEvent.keyDown(input, { key: "Escape" });
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(input.value).toBe("$GH");
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
      <Field
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

// A COMPANY THAT HOLDS NOTHING GETS NO LIST, and neither does a query that
// matches nothing: an empty popup is a control that says a company has
// entries when it has none.
test("no list where there is nothing to offer", () => {
  const { rerender } = render(<Editable secrets={[]} />);
  const input = screen.getByLabelText("Webhook secret") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "$" } });
  fireEvent.keyUp(input, { key: "$" });
  expect(screen.queryByRole("listbox")).toBeNull();

  rerender(<Editable />);
  const live = screen.getByLabelText("Webhook secret") as HTMLInputElement;
  fireEvent.change(live, { target: { value: "$ZZZ" } });
  fireEvent.keyUp(live, { key: "Z" });
  expect(screen.queryByRole("listbox")).toBeNull();
});
