/**
 * A connect form's Setup steps: what to do at the app, in order, with the
 * permissions to grant there drawn as code and each vendor link where the
 * engine placed it (`setup.Guide.Prepare`).
 *
 * THE STEPS SPAN THE FIELDS, so they are said once, in a box of their own,
 * rather than run into the help under whichever field they happen to produce.
 * This module draws the box and the two ways to reach it: in a column beside
 * the form, the box itself or, put away, a "Setup steps" button where it
 * stood ([SetupStepsAside]); and where the pane has no room for the column, a
 * button in the form's title row that opens the box in a side sheet
 * ([SetupStepsSheet]).
 */

import { useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { Button, Callout, IconButton, InlineCode, List, Modal } from "@crewlethq/ui";
import { ListOrderedGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import type { SetupStep } from "~/protocol/index.ts";
import { withLink, type FieldLink } from "./setupWords.tsx";

/** What every part is given: the steps and how a step's link resolves. */
export interface StepsProps {
  steps: SetupStep[];
  /** The vendor link of a field a step names, resolved as the field's own is. */
  linkOf: (field: string) => FieldLink | undefined;
}

/** One step's words, then the permissions it grants, each one a person finds and ticks off. */
function stepWords(step: SetupStep, linkOf: StepsProps["linkOf"]): ReactNode {
  return (
    <span className="int-step">
      <span>{withLink(step.text, step.link ? linkOf(step.link) : undefined)}</span>
      {step.grants && step.grants.length > 0 && (
        <span className="int-step-grants">
          {step.grants.map((grant) => (
            <InlineCode key={grant}>{grant}</InlineCode>
          ))}
        </span>
      )}
    </span>
  );
}

/**
 * The box: a neutral card titled "Setup steps" holding the numbered steps,
 * with a close button in its top corner, beside its title, where `onClose` is
 * given.
 */
export function StepsBox({ steps, linkOf, onClose }: StepsProps & { onClose?: () => void }) {
  return (
    <Callout
      variant="neutral"
      title={
        <span className="int-steps-title">
          <span>Setup steps</span>
          {onClose && (
            <IconButton
              label="Close Setup steps"
              icon={<XGlyph size="sm" />}
              size="sm"
              variant="ghost"
              onClick={onClose}
            />
          )}
        </span>
      }
    >
      <List variant="numbered" density="compact">
        {steps.map((step, i) => (
          <List.Item key={i}>{stepWords(step, linkOf)}</List.Item>
        ))}
      </List>
    </Callout>
  );
}

/**
 * The column beside the form: the box while it is open, with its close button
 * in its corner, and a "Setup steps" button where it stood while it is put
 * away.
 *
 * FOCUS FOLLOWS THE PERSON, never the default. Opened by a press, focus goes
 * into the box; put away from inside it (its close button, or Escape anywhere
 * in it), focus goes back to the button that reopens it. Open by default, it
 * takes nothing from the page: whoever arrived came to fill the form in.
 */
export function SetupStepsAside({
  steps,
  linkOf,
  open,
  onOpenChange,
}: StepsProps & { open: boolean; onOpenChange: (open: boolean) => void }) {
  const id = useId();
  const toggle = useRef<HTMLButtonElement | null>(null);
  const box = useRef<HTMLDivElement | null>(null);
  // WHERE FOCUS IS OWED once the change lands: into the box a person opened,
  // or back to the button after they put it away from inside.
  const owed = useRef<"box" | "toggle" | null>(null);
  useLayoutEffect(() => {
    if (owed.current === "box" && open) box.current?.querySelector<HTMLElement>("button")?.focus();
    if (owed.current === "toggle" && !open) toggle.current?.focus();
    owed.current = null;
  }, [open]);
  const putAway = () => {
    owed.current = box.current?.contains(document.activeElement) ? "toggle" : null;
    onOpenChange(false);
  };
  if (!open) {
    return (
      <Button
        ref={toggle}
        size="small"
        variant="secondary"
        leadingIcon={<ListOrderedGlyph size="sm" />}
        aria-expanded={false}
        aria-controls={id}
        className="int-steps-toggle"
        onClick={() => {
          owed.current = "box";
          onOpenChange(true);
        }}
      >
        Setup steps
      </Button>
    );
  }
  return (
    <div
      ref={box}
      id={id}
      onKeyDown={(e) => {
        if (e.key !== "Escape" || e.defaultPrevented) return;
        e.preventDefault();
        putAway();
      }}
    >
      <StepsBox steps={steps} linkOf={linkOf} onClose={putAway} />
    </div>
  );
}

/**
 * Where the pane has no room for a column beside the form: a "Setup steps"
 * button in the form's title row, opening the box in a side sheet. It opens
 * only when pressed: a sheet that opened by itself would take the page from
 * someone who came to fill the form in. The sheet holds focus, closes on
 * Escape and gives focus back to the button.
 */
export function SetupStepsSheet({ steps, linkOf }: StepsProps) {
  const id = useId();
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button
        size="small"
        variant="secondary"
        leadingIcon={<ListOrderedGlyph size="sm" />}
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        onClick={() => setOpen(true)}
      >
        Setup steps
      </Button>
      <Modal
        open={open}
        onClose={() => setOpen(false)}
        variant="sheet"
        side="end"
        title="Setup steps"
        closeLabel="Close Setup steps"
      >
        <div id={id}>
          <StepsBox steps={steps} linkOf={linkOf} />
        </div>
      </Modal>
    </>
  );
}
