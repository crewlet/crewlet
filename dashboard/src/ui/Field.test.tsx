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
