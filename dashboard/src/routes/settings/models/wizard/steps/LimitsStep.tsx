/**
 * How many CLI processes the model runs at once, how long one may go without
 * progress, and its web tools.
 *
 * NUMBERS ARE NUMBERS (design QA Minor 6): narrow fields for a few digits,
 * labelled for what they count, empty for the engine's own default.
 *
 * THE WEB TOOLS SWITCH SAYS ITS STATE (design QA Minor 28): web tools are
 * allowed by default, as the owner decided, and the helper says so rather
 * than "off by default", which read as the tools being off. While the
 * Dashboard Assistant runs on the model the switch is held on, because the
 * engine refuses the assistant on a model that allows them.
 */

import { FormField, Input, Switch } from "@crewlethq/ui";
import {
  DEFAULT_HARNESS_MAX_CONCURRENT,
  DEFAULT_HARNESS_TIMEOUT_SECONDS,
} from "~/contract/models.ts";
import type { Draft } from "../draft.ts";

/** The web tools switch's helper, which Review repeats in the same words. */
export const WEB_TOOLS_HELP =
  "Web tools are allowed unless you turn this on. Turn it on for jobs that read what people write. Ask Crewlet runs only on models with web tools denied.";

/** The web tools as Review says them, in the switch's words. */
export const webToolsWords = (denied: boolean) => (denied ? "Denied" : "Allowed");

export function LimitsStep({
  draft,
  errors,
  onChange,
}: {
  draft: Draft;
  errors: { concurrent?: string | undefined; timeout?: string | undefined };
  onChange: (patch: Partial<Draft>) => void;
}) {
  const held = draft.uses.assistant && draft.webDenied;
  return (
    <>
      <FormField
        label="Processes at once"
        optional
        helper={`CLI processes on the engine host for this model. Executors are not counted. Empty means ${DEFAULT_HARNESS_MAX_CONCURRENT}.`}
        error={errors.concurrent}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
            error={field.invalid}
            type="number"
            inputMode="numeric"
            min={1}
            step={1}
            width="xs"
            value={draft.concurrent}
            placeholder={String(DEFAULT_HARNESS_MAX_CONCURRENT)}
            onChange={(e) => onChange({ concurrent: e.target.value })}
          />
        )}
      </FormField>
      <FormField
        label="Timeout, in seconds"
        optional
        helper={`How long one stretch of work may go without progress on the host. Empty means ${DEFAULT_HARNESS_TIMEOUT_SECONDS}.`}
        error={errors.timeout}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid || undefined}
            error={field.invalid}
            type="number"
            inputMode="numeric"
            min={1}
            step={1}
            width="xs"
            value={draft.timeout}
            placeholder={String(DEFAULT_HARNESS_TIMEOUT_SECONDS)}
            onChange={(e) => onChange({ timeout: e.target.value })}
          />
        )}
      </FormField>
      <Switch
        label="Deny the CLI's web tools"
        description={
          held
            ? `${WEB_TOOLS_HELP} Ask Crewlet runs on this model, so they stay denied.`
            : WEB_TOOLS_HELP
        }
        checked={draft.webDenied}
        disabled={held}
        onCheckedChange={(webDenied) => onChange({ webDenied })}
      />
    </>
  );
}
