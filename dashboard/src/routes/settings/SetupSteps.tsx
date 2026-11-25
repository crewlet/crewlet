/**
 * A connect form's Setup steps (`setup.Guide.Prepare`): the box, its column
 * beside the form, and the title-row sheet where the pane has no room.
 */

import { useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Disclosure,
  IconButton,
  Link,
  List,
  Tag,
  VendorUI,
  VisuallyHidden,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { CircleCheckGlyph, ListOrderedGlyph, XGlyph } from "@crewlethq/icons/glyphs";
import type { SetupStep } from "~/protocol/index.ts";
import { VendorWords } from "~/ui/VendorWords.tsx";
import { href } from "~/app/router.tsx";
import { entryOfSurface } from "./integrations/catalogue.tsx";

/** The steps, and the address a step's vendor link opens with the form's values filled in. */
export interface StepsProps {
  steps: SetupStep[];
  resolveUrl: (url: string) => string;
}

/** The integration a step connects first, by name and page. */
function needed(kind: string): { name: string; page: string } {
  const entry = entryOfSurface(kind);
  return { name: entry?.name ?? kind, page: href(["company", "integrations", entry?.key ?? kind]) };
}

/** Whether the integration a step needs is connected, and a link to it while it is not. */
function NeedsState({ kind, connected }: { kind: string; connected: boolean }) {
  const { name, page } = needed(kind);
  return (
    <span className="int-step-needs">
      <Tag size="sm" variant={connected ? "success" : "warning"}>
        {connected ? `${name} connected` : `${name} not connected`}
      </Tag>
      {!connected && <Link href={page}>Open {name}</Link>}
    </span>
  );
}

/**
 * Above the form, each integration a step connects first that is not
 * connected yet, so fields that cannot work until then do not look ready.
 */
export function NeedsFirst({ steps }: { steps: SetupStep[] }) {
  return steps
    .filter((step) => step.needs && step.connected === false)
    .map((step) => {
      const { name, page } = needed(step.needs!);
      return (
        <Callout
          key={step.needs}
          variant="warning"
          title={`Connect ${name} first`}
          action={<Link href={page}>Open {name}</Link>}
        >
          <span>{name} is not connected yet.</span>
        </Callout>
      );
    });
}

/**
 * One step: its title and where it is taken, its words, the permissions it
 * grants, the state of what it connects first, what shows it worked, and
 * what people hit there, folded.
 */
function stepWords(step: SetupStep, resolveUrl: StepsProps["resolveUrl"]): ReactNode {
  return (
    <div className="int-step">
      {step.title && (
        <span className="int-step-head">
          <span className="int-step-title">{step.title}</span>
          {step.where && (
            <Tag size="xs" variant="neutral">
              <VisuallyHidden>In </VisuallyHidden>
              {step.where}
            </Tag>
          )}
        </span>
      )}
      <span>
        <VendorWords text={step.text} resolveUrl={resolveUrl} />
      </span>
      {step.grants && step.grants.length > 0 && (
        <span className="int-step-grants">
          {step.grants.map((grant) => (
            <VendorUI key={grant} path={grant} />
          ))}
        </span>
      )}
      {step.needs && <NeedsState kind={step.needs} connected={step.connected === true} />}
      {step.result && step.result.length > 0 && (
        <span className="int-step-result">
          <span className="int-step-result-mark">
            <CircleCheckGlyph size="sm" aria-hidden />
          </span>
          <span>
            <VisuallyHidden>When it worked: </VisuallyHidden>
            <VendorWords text={step.result} resolveUrl={resolveUrl} />
          </span>
        </span>
      )}
      {step.trouble && step.trouble.length > 0 && (
        <Disclosure
          title="If it goes wrong"
          size="compact"
          variant="aside"
          flush
          headingLevel="none"
        >
          <ul className="int-step-trouble">
            {step.trouble.map((trouble, i) => (
              <li key={i}>
                <span className="int-step-sign">
                  <VendorWords text={trouble.sign} resolveUrl={resolveUrl} />
                </span>
                <span>
                  <VendorWords text={trouble.fix} resolveUrl={resolveUrl} />
                </span>
              </li>
            ))}
          </ul>
        </Disclosure>
      )}
    </div>
  );
}

/** The numbered steps: in the box the numbers hang in its icon column, in the sheet in their own gutter. */
function StepsList({
  steps,
  resolveUrl,
  markers,
}: StepsProps & { markers: "hanging" | "indented" }) {
  return (
    <List variant="numbered" density="compact" markers={markers}>
      {steps.map((step, i) => (
        <List.Item key={i}>{stepWords(step, resolveUrl)}</List.Item>
      ))}
    </List>
  );
}

/**
 * The neutral "Setup steps" box, with a corner close button when `onClose` is
 * given. The numbers hang in the box's icon column, so the steps start where
 * its title does.
 */
export function StepsBox({ steps, resolveUrl, onClose }: StepsProps & { onClose?: () => void }) {
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
      <StepsList steps={steps} resolveUrl={resolveUrl} markers="hanging" />
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
  resolveUrl,
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
        <StepsBox steps={steps} resolveUrl={resolveUrl} onClose={putAway} />
      </div>
    </>
  );
}

/** The narrow pane's title-row button, opening the box in a side sheet only when pressed. */
export function SetupStepsSheet({ steps, resolveUrl }: StepsProps) {
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
        className="int-steps-toggle"
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
        {/* THE SHEET'S TITLE NAMES THEM, so the box's own would say it twice. */}
        <div id={id}>
          <StepsList steps={steps} resolveUrl={resolveUrl} markers="indented" />
        </div>
      </Modal>
    </>
  );
}
