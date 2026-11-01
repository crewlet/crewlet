/**
 * The Ask button: how a reader opens the assistant from any page.
 *
 * DRAWN BY THE FRAME, NEVER BY A SCREEN: the page bar places it last (the
 * pill sits above the panel it opens), and a screen that draws its own head
 * places this same component, so there is one Ask in the product.
 *
 * NEVER HIDDEN: where the reader may not ask it is drawn disabled, with the
 * reason after its name and in its tooltip (`availability.ts`), and it is
 * still reachable by Tab. On a phone it keeps its place as an icon button,
 * because it is how a reader reaches help on a screen they do not follow.
 *
 * AND IT SAYS WHEN THE ASSISTANT NEEDS THEM while the panel is closed: a dot
 * on the mark's corner, and the reason in its name and its tooltip, when an
 * answer is on its way or a change waits on their approval, read from
 * the entry's one-word store (`status.ts`), so this button never imports the
 * panel's chunk.
 */

import {
  Button,
  IconButton,
  IndicatorAnchor,
  StatusDot,
  VisuallyHidden,
  useMediaMatch,
} from "@crewlethq/ui";
import { ProductMark } from "@crewlethq/icons/product-mark";
import { PHONE_BREAKPOINT, useKeyHints } from "../layout.ts";
import { ariaKeysOf, keyRow, pressText } from "../keymap.ts";
import { ASSISTANT_TRIGGER_ID, useAssistantControl } from "./control.ts";
import { useAssistantActivity } from "./status.ts";

/** What the dot says, in words, while the panel is closed. */
export const ACTIVITY_WORDS = {
  working: "An answer is on its way",
  waiting: "A change waits on your approval",
} as const;

export function AssistantTrigger() {
  const { open, unavailable, toggle } = useAssistantControl();
  const activity = useAssistantActivity();
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  const keyHints = useKeyHints();
  const press = keyRow("assistant.open").presses[0]!;
  // THE DOT IS FOR A CLOSED PANEL: open, the panel says it itself.
  const shown = !open && activity !== "idle" ? activity : null;
  // THE DOT IS WORN ON THE MARK'S CORNER (the kit's IndicatorAnchor), ringed
  // in the button's own fill, never set after the word, where it sat on the
  // text's baseline against the last letter.
  const mark = (
    <IndicatorAnchor
      indicator={
        shown ? (
          <StatusDot tone={shown === "waiting" ? "warning" : "info"} pulse={shown === "working"} />
        ) : null
      }
    >
      <ProductMark product="assistant" size="sm" />
    </IndicatorAnchor>
  );
  // NO KEYCAP ON THE BUTTON: the shortcut is the button's own property
  // (`aria-keyshortcuts`) and its tooltip's words, for a reader with keys.
  const ask = keyHints ? `Ask Crewlet (${pressText(press)})` : "Ask Crewlet";
  const tooltip = shown ? `${ask}. ${ACTIVITY_WORDS[shown]}.` : ask;
  const common = {
    id: ASSISTANT_TRIGGER_ID,
    disabledReason: unavailable?.short,
    onClick: () => toggle("composer"),
    "aria-keyshortcuts": ariaKeysOf(press),
  };
  if (phone) {
    return (
      <IconButton
        {...common}
        size="md"
        variant="secondary"
        pressed={open}
        icon={mark}
        label={shown ? `Ask Crewlet, ${ACTIVITY_WORDS[shown]}` : "Ask Crewlet"}
      />
    );
  }
  return (
    <Button
      {...common}
      variant="secondary"
      shape="pill"
      size="medium"
      pressed={open}
      leadingIcon={mark}
      title={unavailable ? undefined : tooltip}
    >
      Ask
      {shown ? <VisuallyHidden>{`, ${ACTIVITY_WORDS[shown]}`}</VisuallyHidden> : null}
    </Button>
  );
}
