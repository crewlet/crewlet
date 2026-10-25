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
 * when an answer is on its way or a change waits on their approval, read from
 * the entry's one-word store (`status.ts`), so this button never imports the
 * panel's chunk.
 */

import {
  Button,
  IconButton,
  Inline,
  Kbd,
  StatusDot,
  VisuallyHidden,
  useMediaMatch,
} from "@crewlethq/ui";
import { ProductMark } from "@crewlethq/icons/product-mark";
import { PHONE_BREAKPOINT, useKeyHints } from "../layout.ts";
import { capsOf, keyRow } from "../keymap.ts";
import { ASSISTANT_TRIGGER_ID, useAssistantControl } from "./control.ts";
import { useAssistantActivity } from "./status.ts";

/** What the dot says, in words, while the panel is closed. */
const ACTIVITY_WORDS = {
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
  const dot = shown ? (
    <>
      <StatusDot tone={shown === "waiting" ? "warning" : "info"} pulse={shown === "working"} />
      <VisuallyHidden>{`, ${ACTIVITY_WORDS[shown]}`}</VisuallyHidden>
    </>
  ) : null;
  const mark = <ProductMark product="assistant" size="sm" />;
  const common = {
    id: ASSISTANT_TRIGGER_ID,
    disabledReason: unavailable?.short,
    onClick: () => toggle("composer"),
  };
  if (phone) {
    return (
      <Inline gap={1} align="center">
        <IconButton
          {...common}
          size="md"
          variant="secondary"
          pressed={open}
          icon={mark}
          label={shown ? `Ask the assistant, ${ACTIVITY_WORDS[shown]}` : "Ask the assistant"}
        />
        {shown ? <StatusDot tone={shown === "waiting" ? "warning" : "info"} /> : null}
      </Inline>
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
      title={unavailable ? undefined : "Ask the assistant"}
      trailingIcon={keyHints ? <Kbd keys={capsOf(press)} subtle /> : undefined}
    >
      Ask
      {dot}
    </Button>
  );
}
