/**
 * The url field's scheme affix.
 *
 * Every config field of this kind is refused without a scheme, so the affix
 * is not decoration: it is the difference between a value that saves and one
 * the engine rejects after the fact.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
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
