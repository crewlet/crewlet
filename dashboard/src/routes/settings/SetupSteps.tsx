/**
 * A connect form's Setup steps (`setup.Guide.Prepare`): the box, its column
 * beside the form, and the title-row sheet where the pane has no room.
 */

import { useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { Button, Callout, IconButton, InlineCode, List, Modal } from "@crewlethq/ui";
import { ListOrderedGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import type { SetupStep } from "~/protocol/index.ts";
import { withLink, type FieldLink } from "./setupWords.tsx";

/** The steps, and how a step's vendor link resolves. */
export interface StepsProps {
  steps: SetupStep[];
  linkOf: (field: string) => FieldLink | undefined;
}

/** One step's words, then the permissions it grants as code. */
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
 * The neutral "Setup steps" box, with a corner close button when `onClose` is
 * given. The numbers hang in the box's icon column, so the steps start where
 * its title does.
 */
export function StepsBox({ steps, linkOf, onClose }: StepsProps & { onClose?: () => void }) {
  return (
    <Callout
      variant="neutral"
      title={
        onClose ? (
          <span className="int-steps-title">
            <span>Setup steps</span>
            <IconButton
              label="Close Setup steps"
              icon={<XGlyph size="sm" />}
              size="sm"
              variant="ghost"
              onClick={onClose}
            />
          </span>
        ) : (
          "Setup steps"
        )
      }
    >
      <List variant="numbered" density="compact" markers="hanging">
        {steps.map((step, i) => (
          <List.Item key={i}>{stepWords(step, linkOf)}</List.Item>
        ))}
      </List>
    </Callout>
  );
}

/**
 * The column beside the form: the box, or a "Setup steps" button while it is
 * put away. The box stays mounted and hidden then, so the button always names
 * it; focus moves only on a person's press, close or Escape.
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
  // Where focus goes once the change lands: into the box, or back to the button.
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
  return (
    <>
      {!open && (
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
      )}
      <div
        ref={box}
        id={id}
        hidden={!open}
        onKeyDown={(e) => {
          if (e.key !== "Escape" || e.defaultPrevented) return;
          e.preventDefault();
          putAway();
        }}
      >
        <StepsBox steps={steps} linkOf={linkOf} onClose={putAway} />
      </div>
    </>
  );
}

/** The narrow pane's title-row button, opening the box in a side sheet only when pressed. */
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
