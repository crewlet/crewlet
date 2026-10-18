/**
 * The composer, and the two things it must not decide on the engine's behalf.
 *
 * WHAT A MESSAGE NAMES IS THE ENGINE'S READING OF IT: the tool reads the
 * handles and `@channel` off the body against the company's seats, so the
 * composer sends what was typed and nothing about it — a second reading here
 * would be a second answer to "who did this wake".
 *
 * AND THE SEND KEY IS THE KEYMAP'S: one row (`compose.send`) for every box a
 * person writes to somebody in, so a reply in the Inbox and a message in a
 * room are sent the same way. The assistant's question box is the one box
 * that sends on Enter instead.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { keyRow } from "~/app/keymap.ts";
import { MAX_BODY_BYTES } from "~/contract/chat.ts";
import { Composer } from "./Composer.tsx";

afterEach(cleanup);

function mount(send = vi.fn()) {
  render(<Composer what="#general" send={send} />);
  return { send, box: screen.getByLabelText("Message #general") };
}

/** The keymap's send press, as a reader's keyboard produces it. */
const sendKey = { key: "Enter", ctrlKey: true };

describe("saying it", () => {
  test("the keymap's compose row sends, and Enter alone is a new line", () => {
    // ONE ROW FOR A REPLY AND A MESSAGE. The press is read through the keymap
    // rather than spelled here, so this case also holds that the row still
    // says what this file sends with.
    expect(keyRow("compose.send").presses).toEqual([{ key: "enter", mod: true }]);
    const { send, box } = mount();
    fireEvent.change(box, { target: { value: "morning @ada" } });
    fireEvent.keyDown(box, { key: "Enter" });
    fireEvent.keyDown(box, { key: "Enter", shiftKey: true });
    expect(send).not.toHaveBeenCalled();

    fireEvent.keyDown(box, sendKey);
    expect(send).toHaveBeenCalledTimes(1);
  });

  test("a question to the assistant is the one box that sends on Enter", () => {
    // THE EXCEPTION THIS COMPOSER'S DOC NAMES, held so the doc cannot outlive
    // it: the assistant's box is the design system's, sending a question on
    // Enter and writing a line on Shift+Enter, while a message sends on the
    // command key.
    expect(keyRow("assistant.send").presses).toEqual([{ key: "enter" }]);
    expect(keyRow("compose.send").presses).toEqual([{ key: "enter", mod: true }]);
  });

  test("what was typed is what is sent, mentions and all, for the engine to read", () => {
    // NO FIELD BESIDE THE BODY: the tool resolves `@ada` against the seats
    // and `@channel` as the room, and a copy of that reading here could only
    // disagree with it.
    const { send, box } = mount();
    fireEvent.change(box, { target: { value: "  @channel standup, @ada leads  " } });
    fireEvent.keyDown(box, sendKey);
    expect(send).toHaveBeenCalledWith("@channel standup, @ada leads");
  });

  test("a composition in progress is the input method's Enter, not a send", () => {
    const { send, box } = mount();
    fireEvent.change(box, { target: { value: "日本" } });
    fireEvent.keyDown(box, { ...sendKey, isComposing: true });
    expect(send).not.toHaveBeenCalled();
  });

  test("a body over the cap is refused with the number rather than cut to fit", () => {
    // Nothing is silently shortened: cutting somebody's message to fit is
    // losing what they wrote, and the engine refuses it by name anyway.
    const { send, box } = mount();
    fireEvent.change(box, { target: { value: "x".repeat(MAX_BODY_BYTES + 1) } });
    expect(screen.getByText(/Nothing is cut to fit/)).toBeTruthy();
    fireEvent.keyDown(box, sendKey);
    expect(send).not.toHaveBeenCalled();
  });

  test("a body near the cap counts as every capped field in the dashboard does", () => {
    // ONE RULE FOR A TEXT AGAINST ITS CAP (`textBudget`): the count starts at
    // the last fifth and says it in the same words as the room's topic field
    // beside it. This composer once kept a threshold of its own (nine
    // tenths) and sentences of its own, so a message started counting later,
    // and differently, than every other field.
    const { box } = mount();
    const near = Math.ceil(MAX_BODY_BYTES * 0.8);
    fireEvent.change(box, { target: { value: "x".repeat(near - 1) } });
    expect(screen.queryByText(/ of .* bytes$/)).toBeNull();
    fireEvent.change(box, { target: { value: "x".repeat(near) } });
    const count = screen.getByText(`${near} of ${MAX_BODY_BYTES} bytes`);
    // SAID WITH THE BOX, as a field's help is: a screen reader hears it there.
    expect(count.id).not.toBe("");
    expect(box.getAttribute("aria-describedby") ?? "").toContain(count.id);

    // PAST THE CAP, THE REFUSAL AND THIS FIELD'S OWN REMEDY, as the error.
    fireEvent.change(box, { target: { value: "x".repeat(MAX_BODY_BYTES + 1) } });
    expect(
      screen.getByText(
        `${MAX_BODY_BYTES + 1} bytes, but a message holds at most ${MAX_BODY_BYTES}. ` +
          "Nothing is cut to fit — shorten it, or put the long part somewhere with an address.",
      ),
    ).toBeTruthy();
  });

  test("a body at the cap is sendable whatever whitespace trails it", () => {
    // MEASURED AS WHAT IS SENT, the trimmed body (`textBudget`): the
    // composer counted the spaces it was about to drop, and held a message
    // that fit.
    const { send, box } = mount();
    fireEvent.change(box, { target: { value: `${"x".repeat(MAX_BODY_BYTES)}   ` } });
    expect(screen.queryByText(/Nothing is cut to fit/)).toBeNull();
    fireEvent.keyDown(box, sendKey);
    expect(send).toHaveBeenCalledWith("x".repeat(MAX_BODY_BYTES));
  });

  test("an empty message is not a message", () => {
    const { send, box } = mount();
    fireEvent.change(box, { target: { value: "   " } });
    fireEvent.keyDown(box, sendKey);
    expect(send).not.toHaveBeenCalled();
  });

  test("typing is reported, and withdrawn when the composer goes", () => {
    // The flag drives a fleet-wide presence probe and expires on its own after
    // six seconds. A composer that unmounted without withdrawing leaves
    // somebody typing on everybody else's screen for the rest of that window.
    const onTyping = vi.fn();
    const view = render(<Composer what="#general" send={vi.fn()} onTyping={onTyping} />);
    fireEvent.change(screen.getByLabelText("Message #general"), { target: { value: "h" } });
    expect(onTyping).toHaveBeenCalledWith(true);
    view.unmount();
    expect(onTyping).toHaveBeenLastCalledWith(false);
  });

  test("a room that takes no messages says so in the box itself", () => {
    render(<Composer what="#general" send={vi.fn()} closed="This room is archived." />);
    const box = screen.getByLabelText("Message #general") as HTMLTextAreaElement;
    expect(box.disabled).toBe(true);
    expect(box.placeholder).toBe("This room is archived.");
  });

  test("a browser that cannot send holds Send and leaves the box open", () => {
    // THE BROWSER'S REASON IS THE BUTTON'S, as on every other composer: the
    // box stays typeable — a field that went dead when the socket blinked
    // took the person's focus mid-word — and Send is held, focusable, with
    // the sentence, which is written beside it too.
    const send = vi.fn();
    render(<Composer what="#general" send={send} held="Offline — reconnect to make changes." />);
    const box = screen.getByLabelText("Message #general") as HTMLTextAreaElement;
    expect(box.disabled).toBe(false);
    fireEvent.change(box, { target: { value: "still here" } });
    const button = screen.getByRole("button", { name: /^send$/i });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    expect(button.getAttribute("title")).toBe("Offline — reconnect to make changes.");
    expect(screen.getAllByText("Offline — reconnect to make changes.").length).toBeGreaterThan(0);

    // NEITHER THE KEY NOR THE BUTTON SENDS, and the words are kept.
    fireEvent.keyDown(box, sendKey);
    fireEvent.click(button);
    expect(send).not.toHaveBeenCalled();
    expect(box.value).toBe("still here");
  });

  test("Send says why an empty message is no message", () => {
    const { box } = mount();
    const button = screen.getByRole("button", { name: /^send$/i });
    expect(button.getAttribute("title")).toBe("Write the message first.");
    fireEvent.change(box, { target: { value: "morning" } });
    expect(button.getAttribute("aria-disabled")).not.toBe("true");
  });
});
