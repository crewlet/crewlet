/**
 * A BOX THAT IS TAKING DICTATION TAKES NO ASSISTANT CHANGE: a proposal for it
 * is refused with the reason until Accept or Cancel, so the dictated words are
 * never spliced beside a change made under them. Driven through the kit's
 * composer with its scripted microphone and transport.
 */
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, expect, test, vi } from "vitest";
import {
  Announcer,
  Composer,
  fakeMicrophone,
  fakeTransport,
  preloadDictation,
  type DictationTransport,
} from "@crewlethq/ui";
import { DraftBoxes, DraftBoxesContext, boxesDraft, useDraftBox } from "~/app/assistant/boxes.ts";
import { DraftRefusal, useDraftState } from "~/app/assistant/draft.ts";
import { useViewer, type ViewerState } from "~/lib/viewer.ts";
import { DICTATING, useDictationBox } from "./source.ts";

vi.mock("~/lib/viewer.ts", () => ({ useViewer: vi.fn() }));

const transport: DictationTransport = fakeTransport({ pace: 20, flushDelay: 10 });
const microphone = fakeMicrophone();

function Box() {
  const [value, setValue] = useDraftState("Ship it");
  const { dictation, refusal } = useDictationBox();
  useDraftBox({
    path: "message",
    label: "Message",
    hint: "Markdown",
    rank: 0,
    value,
    set: setValue,
    refuses: () => refusal,
  });
  return (
    <Composer
      label="Message"
      value={value}
      onChange={setValue}
      dictation={{ ...dictation, transport, microphone }}
    />
  );
}

/** What the assistant's proposal for the box is answered. */
function propose(boxes: DraftBoxes) {
  const spec = boxesDraft(boxes.read(), { scope: "room", help: "" })!;
  return spec.fields[0]!.write!(spec.value, "Ship it today");
}

beforeAll(async () => {
  await preloadDictation();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

test("a proposal waits while the box dictates, and is taken again after Cancel", async () => {
  vi.mocked(useViewer).mockReturnValue({ admin: false } as ViewerState);
  const boxes = new DraftBoxes();
  render(
    <DraftBoxesContext.Provider value={boxes}>
      <Announcer />
      <Box />
    </DraftBoxesContext.Provider>,
  );
  expect(propose(boxes)).not.toBeInstanceOf(DraftRefusal);

  fireEvent.click(screen.getByRole("button", { name: "Dictate" }));
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Accept dictation" })).toBeTruthy(),
  );
  const refused = propose(boxes);
  expect(refused).toBeInstanceOf(DraftRefusal);
  expect((refused as DraftRefusal).reason).toBe(DICTATING);

  act(() => {
    fireEvent.keyDown(screen.getByRole("textbox", { name: "Message" }), { key: "Escape" });
  });
  await waitFor(() => expect(propose(boxes)).not.toBeInstanceOf(DraftRefusal));
});
