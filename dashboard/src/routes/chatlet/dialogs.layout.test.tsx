/**
 * ADD PEOPLE'S SUGGESTIONS GROW TO THE ROOM THE DIALOG HAS. The list is the
 * dialog's one growing part, in a body its form fills (the kit's `fill`,
 * `Dialogs.tsx`), so on a wide window it shows every suggestion the dialog has
 * room for, and on a phone's sheet it runs down to the foot. A cap of six
 * control heights showed five of eight rows and a sliver of the sixth on a
 * dialog with room to spare, and left about 400px empty under it on a phone.
 *
 * Measured with the stylesheets where a built page puts them
 * (`~/test/layout.tsx`): Chatlet's sheets are a lazy chunk's.
 */
import "~/test/layout.tsx";
import "~/styles/chatlet-frame.css";
import "~/styles/chatlet-room.css";
import "~/styles/chatlet-panes.css";
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { Button, LayerHost, Modal, PersonRow, ToastProvider } from "@crewlethq/ui";

import { eventually } from "~/test/settle.ts";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

/** The dialog as `AddPeopleDialog` draws it: a field, the suggestions, a foot. */
function AddPeople({ rows }: { rows: number }) {
  return (
    <Modal
      open
      onClose={() => {}}
      size="md"
      phoneSheet
      fill
      title="Add people to #launch"
      subtitle="Add people and agents from the company."
      footer={<Button variant="primary">Add</Button>}
    >
      <div className="chat-form chat-form--fills">
        <input aria-label="Find people and agents" style={{ height: 40 }} />
        <div className="chat-suggest">
          <h3 className="chat-suggest__label">Suggestions</h3>
          <ul className="chat-suggest__list">
            {Array.from({ length: rows }, (_, i) => (
              <li key={i}>
                <PersonRow
                  avatar={<span style={{ display: "inline-block", width: 32, height: 32 }} />}
                  name={`Agent ${i + 1}`}
                  detail="Working"
                  mark="toggle"
                  selected={false}
                  onSelect={() => {}}
                />
              </li>
            ))}
          </ul>
        </div>
      </div>
    </Modal>
  );
}

async function mount(rows: number) {
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <ToastProvider>
        <LayerHost>
          <AddPeople rows={rows} />
        </LayerHost>
      </ToastProvider>,
    ),
  );
  return eventually(() => {
    const list = document.querySelector<HTMLElement>(".chat-suggest__list");
    expect(list?.children.length).toBe(rows);
    return list!;
  });
}

test("on a wide window every suggestion the dialog has room for is shown, none cut", async () => {
  await page.viewport(1440, 900);
  const list = await mount(8);
  await eventually(() => {
    // EIGHT ROWS FIT a dialog that may stand 844px tall: the list scrolls nothing.
    expect(list.scrollHeight - list.clientHeight).toBeLessThanOrEqual(1);
  });
});

test("on a phone's sheet the list runs down to the foot", async () => {
  await page.viewport(390, 844);
  const list = await mount(14);
  await eventually(() => {
    const foot = document.querySelector<HTMLElement>(".crewlet-modal__footer")!;
    const gap = foot.getBoundingClientRect().top - list.getBoundingClientRect().bottom;
    // The body's own padding between them, and no empty band.
    expect(gap).toBeLessThanOrEqual(24);
    expect(list.scrollHeight).toBeGreaterThan(list.clientHeight);
  });
});
