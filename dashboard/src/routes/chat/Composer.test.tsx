/**
 * The composer, and the one thing it decides on the engine's behalf.
 *
 * A message carries the handles it named as a FIELD — the engine does not
 * re-read the prose — so what this box resolves is what wakes somebody. A name
 * it invents is a wake to nobody; a name it misses is a colleague who never
 * hears.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { Composer, MAX_BODY_BYTES, mentionsIn } from "./Composer.tsx";

afterEach(cleanup);

describe("what a body names", () => {
  const roster = ["ada", "bo", "ceo"];

  test("only a handle the company actually has becomes a mention", () => {
    // `@` is a character people type. A mention that reaches the record is a
    // WAKE, so a name that is not a seat has to stay prose rather than becoming
    // a person who never answers.
    expect(mentionsIn("@ada can you look? cc @nobody", roster)).toEqual({
      mentions: ["ada"],
      collective: false,
    });
  });

  test("@channel is not a handle — it is the room, and it is capped", () => {
    // It wakes at most 32 agents and the record says when it truncated, which
    // is a different gesture from naming somebody.
    expect(mentionsIn("@channel standup in five", roster)).toEqual({
      mentions: [],
      collective: true,
    });
  });

  test("the same name twice is one mention", () => {
    expect(mentionsIn("@bo @bo @BO", roster).mentions).toEqual(["bo"]);
  });
});

describe("saying it", () => {
  function mount(send = vi.fn()) {
    render(<Composer what="#general" handles={["ada"]} send={send} />);
    return { send, box: screen.getByLabelText("Message #general") };
  }

  test("Enter sends and Shift-Enter does not", () => {
    // This is the one interaction on this screen that happens thousands of
    // times. A composer where Enter inserts a line feed needs a button press
    // per message.
    const { send, box } = mount();
    fireEvent.change(box, { target: { value: "morning @ada" } });
    fireEvent.keyDown(box, { key: "Enter", shiftKey: true });
    expect(send).not.toHaveBeenCalled();

    fireEvent.keyDown(box, { key: "Enter" });
    expect(send).toHaveBeenCalledWith({
      body: "morning @ada",
      mentions: ["ada"],
      collective: false,
    });
  });

  test("a body over the cap is refused with the number rather than cut to fit", () => {
    // Nothing is silently shortened: cutting somebody's message to fit is
    // losing what they wrote, and the engine refuses it by name anyway.
    const { send, box } = mount();
    fireEvent.change(box, { target: { value: "x".repeat(MAX_BODY_BYTES + 1) } });
    expect(screen.getByText(/Nothing is cut to fit/)).toBeTruthy();
    fireEvent.keyDown(box, { key: "Enter" });
    expect(send).not.toHaveBeenCalled();
  });

  test("an empty message is not a message", () => {
    const { send, box } = mount();
    fireEvent.change(box, { target: { value: "   " } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(send).not.toHaveBeenCalled();
  });

  test("typing is reported, and withdrawn when the composer goes", () => {
    // The flag drives a fleet-wide presence probe and expires on its own after
    // six seconds. A composer that unmounted without withdrawing leaves
    // somebody typing on everybody else's screen for the rest of that window.
    const onTyping = vi.fn();
    const view = render(
      <Composer what="#general" handles={[]} send={vi.fn()} onTyping={onTyping} />,
    );
    fireEvent.change(screen.getByLabelText("Message #general"), { target: { value: "h" } });
    expect(onTyping).toHaveBeenCalledWith(true);
    view.unmount();
    expect(onTyping).toHaveBeenLastCalledWith(false);
  });

  test("a room that takes no messages says so in the box itself", () => {
    render(
      <Composer
        what="#general"
        handles={[]}
        send={vi.fn()}
        disabled
        disabledReason="This room is archived."
      />,
    );
    const box = screen.getByLabelText("Message #general") as HTMLTextAreaElement;
    expect(box.disabled).toBe(true);
    expect(box.placeholder).toBe("This room is archived.");
  });
});
