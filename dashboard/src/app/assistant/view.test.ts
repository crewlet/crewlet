/**
 * The outline of the screen the page context carries: what is on screen,
 * named as the person reads it, and never what a field holds.
 */

import { afterEach, describe, expect, test } from "vitest";
import {
  ASSISTANT_PAGE_VIEW_MAX_BYTES,
  ASSISTANT_PAGE_VIEW_MAX_NODES,
} from "~/contract/assistant.ts";
import { pageContextFits, wireBytes } from "~/protocol/assistantWire.ts";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import {
  actionRevision,
  applyPageAction,
  pageSnapshot,
  resetPageContextForTest,
  setFramePage,
} from "./context.ts";
import { buildPageView, PRIVATE_ATTRIBUTE, withView } from "./view.ts";

/** A main pane drawn from `html`, and the outline of it. */
function outline(html: string, around = "") {
  document.body.innerHTML = `${around}<main id="main">${html}</main>`;
  return buildPageView(document.getElementById("main"));
}

afterEach(() => {
  document.body.innerHTML = "";
  resetPageContextForTest();
});

describe("the screen's outline", () => {
  test("names each control as the screen labels it, and a field's state, never its value", () => {
    const view = outline(`
      <h1>Google Cloud</h1>
      <form aria-label="Connect Google Cloud">
        <label for="project">Project ID</label>
        <input id="project" required value="acme-prod-4471" />
        <label for="region">Region</label>
        <select id="region"><option value="">Pick one</option><option value="eu">eu</option></select>
        <label><input type="checkbox" checked /> Read only</label>
        <textarea aria-label="Notes">launch plan for acme</textarea>
        <details><summary>More settings</summary><p>Atlas reads</p></details>
        <button type="submit">Connect</button>
        <button type="button" disabled>Cancel</button>
      </form>`);
    expect(view.nodes).toEqual([
      { role: "heading", name: "Google Cloud", level: 1 },
      { role: "section", name: "Connect Google Cloud" },
      { role: "field", name: "Project ID", field: "text", filled: true, required: true, depth: 1 },
      { role: "field", name: "Region", field: "choice", filled: false, depth: 1 },
      { role: "field", name: "Read only", field: "check", filled: true, depth: 1 },
      { role: "field", name: "Notes", field: "text", filled: true, depth: 1 },
      { role: "button", name: "More settings", depth: 1 },
      { role: "button", name: "Connect", depth: 1 },
      { role: "button", name: "Cancel", disabled: true, depth: 1 },
    ]);
    const sent = JSON.stringify(view);
    expect(sent).not.toContain("acme-prod-4471");
    expect(sent).not.toContain("launch plan");
  });

  test("never reads a secret field's text or anything the page marks private", () => {
    const view = outline(`
      <label for="pw">Service account key</label>
      <input id="pw" type="password" value="hunter2-secret" />
      <label for="tok">API token</label>
      <input id="tok" name="api_token" value="tok-123456" />
      <div ${PRIVATE_ATTRIBUTE}><p role="status">Key fingerprint 9f:aa:01</p><button>Reveal</button></div>`);
    expect(view.nodes).toEqual([
      { role: "field", name: "Service account key", field: "secret", filled: true },
      { role: "field", name: "API token", field: "secret", filled: true },
    ]);
    const sent = JSON.stringify(view);
    for (const secret of ["hunter2", "tok-123456", "9f:aa:01", "Reveal"]) {
      expect(sent).not.toContain(secret);
    }
  });

  test("skips what is hidden", () => {
    const view = outline(`
      <button>Shown</button>
      <button hidden>Hidden attribute</button>
      <div aria-hidden="true"><button>Aria hidden</button></div>
      <div style="display: none"><button>Not displayed</button></div>
      <div style="visibility: hidden"><button>Invisible</button></div>
      <div inert><button>Inert</button></div>`);
    expect(view.nodes).toEqual([{ role: "button", name: "Shown" }]);
  });

  test("puts an open dialog first, and leaves out the assistant's own panel", () => {
    const view = outline(
      `<h1>Integrations</h1><button>Disconnect</button>`,
      `<div role="dialog" aria-label="Disconnect Google Cloud?">
         <p role="alert">Agents lose access at once.</p><button>Disconnect</button>
       </div>
       <aside class="panel"><div role="dialog" aria-label="Ask Crewlet"><button>Send</button></div></aside>`,
    );
    const again = buildPageView(document.getElementById("main"), document.querySelector(".panel"));
    expect(view.nodes[0]).toEqual({ role: "dialog", name: "Disconnect Google Cloud?" });
    expect(again.nodes).toEqual([
      { role: "dialog", name: "Disconnect Google Cloud?" },
      { role: "alert", name: "Agents lose access at once.", depth: 1 },
      { role: "button", name: "Disconnect", depth: 1 },
      { role: "heading", name: "Integrations", level: 1 },
      { role: "button", name: "Disconnect" },
    ]);
  });

  test("marks the current tab and step, a field's error, and a notice", () => {
    const view = outline(`
      <div role="tablist"><button role="tab" aria-selected="true">Setup</button><button role="tab" aria-selected="false">Access</button></div>
      <ol aria-label="Steps"><li>Done, Create a service account</li><li aria-current="step">Grant roles</li><li>Connect</li></ol>
      <div class="crewlet-callout">Grant the role in the Google Cloud console first.</div>
      <label for="p">Project ID</label>
      <input id="p" aria-invalid="true" aria-errormessage="perr" />
      <span id="perr">Enter the project's ID, not its name</span>`);
    expect(view.nodes).toEqual([
      { role: "tab", name: "Setup", current: true },
      { role: "tab", name: "Access" },
      { role: "step", name: "Done, Create a service account" },
      { role: "step", name: "Grant roles", current: true },
      { role: "step", name: "Connect" },
      { role: "notice", name: "Grant the role in the Google Cloud console first." },
      {
        role: "field",
        name: "Project ID",
        field: "text",
        filled: false,
        error: "Enter the project's ID, not its name",
      },
    ]);
  });

  test("is bounded, and says so when it left entries out", () => {
    const many = Array.from(
      { length: ASSISTANT_PAGE_VIEW_MAX_NODES + 20 },
      (_, i) => `<a href="#${i}">Link ${i}</a>`,
    );
    const counted = outline(many.join(""));
    expect(counted.nodes).toHaveLength(ASSISTANT_PAGE_VIEW_MAX_NODES);
    expect(counted.trimmed).toBe(true);

    const long = Array.from(
      { length: 80 },
      (_, i) => `<h2>${"Section heading ".repeat(15)}${i}</h2>`,
    );
    const sized = outline(long.join(""));
    expect(wireBytes(sized)).toBeLessThanOrEqual(ASSISTANT_PAGE_VIEW_MAX_BYTES);
    expect(sized.trimmed).toBe(true);

    expect(outline(`<button>One</button>`).trimmed).toBeUndefined();
  });
});

describe("reading the screen", () => {
  test("every page answers a screen read, and what it sends carries the screen as drawn now", () => {
    setFramePage({
      screen: "integrations",
      title: "Integrations › Google Cloud",
      path: ["company", "integrations", "gcp"],
      params: { kind: "gcp" },
      query: {},
    });
    document.body.innerHTML = `<div id="${SCREEN_SCROLL_ID}"><button type="submit">Connect</button></div>`;
    const context = pageSnapshot()!.context;
    expect(context.accepts).toEqual(["page.read"]);
    const read = applyPageAction({
      correlation_id: "r",
      action: "page.read",
      payload: {},
      source_revision: actionRevision("page.read"),
    });
    expect(read.outcome).toBe("applied");
    // THE SCREEN MOVED UNDER THE REGISTRY: the outline is read when it is sent.
    document.querySelector("button")!.textContent = "Save";
    const sent = withView(pageSnapshot()!.context);
    expect(sent.view?.nodes).toEqual([{ role: "button", name: "Save" }]);
    expect(pageContextFits(sent)).toBe(true);
    expect(actionRevision("page.read")).toBe(context.action_revisions?.["page.read"]);
  });
});
