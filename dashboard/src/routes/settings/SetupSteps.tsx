/**
 * A connect form's Setup steps: what to do at the app, in order, with the
 * permissions to grant there drawn as code and each vendor link where the
 * engine placed it (`setup.Guide.Prepare`).
 *
 * THE STEPS SPAN THE FIELDS, so they are said once, in a box of their own,
 * rather than run into the help under whichever field they happen to produce.
 * This module draws the box and the two ways to reach it: the box itself, and
 * a "Setup steps" button that opens it in a side sheet where the pane has no
 * room to stand it beside the form.
 */

import { useId, useState, type ReactNode } from "react";
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
 * with a close button at its trailing edge where `onClose` is given.
 */
export function StepsBox({ steps, linkOf, onClose }: StepsProps & { onClose?: () => void }) {
  return (
    <Callout
      variant="neutral"
      title="Setup steps"
      className="int-steps-box"
      action={
        onClose ? (
          <IconButton
            label="Close Setup steps"
            icon={<XGlyph size="sm" />}
            size="sm"
            variant="ghost"
            onClick={onClose}
          />
        ) : undefined
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
