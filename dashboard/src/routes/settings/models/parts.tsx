/**
 * The pieces every part of Settings › Models shares: how a flow is opened by
 * the address, the one way into the model wizard, a model's condition and
 * sign-in in words, and its menu of actions.
 */

import { Fragment, type ReactNode } from "react";
import { Menu, RelativeTime, StatusDot, Text } from "@crewlethq/ui";
import { EllipsisGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator, useParam } from "~/app/router.tsx";
import { fmtDate } from "~/lib/format.ts";
import { CATEGORY_ORDER, type JobCategory } from "~/lib/modelUses.ts";
import type { ModelView } from "./data.ts";
import { HEALTH, ROUTE, WINDOW } from "./words.ts";

/** The flows the page opens, by the `flow` parameter. `edit` is the model wizard, opened at a step. */
export type Flow = "add" | "remove" | "test" | "retry" | "device" | "edit";

/** The wizard's steps, in the order a person walks them, by the `step` parameter. */
export const WIZARD_STEPS = [
  "cli",
  "signin",
  "model",
  "runs",
  "assign",
  "limits",
  "test",
  "review",
] as const;
export type WizardStep = (typeof WIZARD_STEPS)[number];

/** A use the wizard opens with added to the model: one seat's category, or the assistant. */
export type WizardAdd = { job: JobCategory; seat: string } | { job: "assistant" };

/**
 * Where the model wizard opens: the step, the field to focus there
 * (`credential` opens a stored secret for replacing; `host-login` is the
 * host's sign-in), and the uses to add to the model (a seat's category picked
 * on By job, every seat a limit holds, or the assistant).
 */
export interface WizardAt {
  step: WizardStep;
  focus?: "credential" | "host-login" | undefined;
  add?: readonly WizardAdd[] | undefined;
}

/** Renewing a token and replacing a key are both the wizard's Method step, its secret open. */
export const NEW_CREDENTIAL: WizardAt = { step: "signin", focus: "credential" };

const WIZARD_PARAMS = { step: null, focus: null, add: null } as const;

const isStep = (v: string): v is WizardStep => (WIZARD_STEPS as readonly string[]).includes(v);
const FOCUSES = ["credential", "host-login"] as const;
const isCategory = (v: string): v is JobCategory =>
  (CATEGORY_ORDER as readonly string[]).includes(v);
/** A seat handle as the engine writes one (`roles[].handle`), so an address cannot carry anything else. */
const HANDLE = /^[a-z0-9][a-z0-9-]*$/;

/**
 * The uses to add, as one parameter: `assistant`, or `category:handle`, comma
 * separated. A handle holds neither character, so the list reads back whole.
 */
function addParam(add: readonly WizardAdd[]): string {
  return add.map((a) => ("seat" in a ? `${a.job}:${a.seat}` : a.job)).join(",");
}

function addOf(param: string): WizardAdd[] {
  const out: WizardAdd[] = [];
  for (const part of param.split(",")) {
    if (part === "assistant") {
      out.push({ job: "assistant" });
      continue;
    }
    const pair = part.split(":");
    const [job = "", seat = ""] = pair;
    if (pair.length === 2 && isCategory(job) && HANDLE.test(seat)) out.push({ job, seat });
  }
  return out;
}

/**
 * The address a wizard opens at, read back: a parameter the page does not
 * know is dropped rather than trusted, so a hand-edited link opens the
 * wizard's first step instead of a step that does not exist.
 */
export function wizardAtOf(params: {
  step: string;
  focus: string;
  add: string;
}): WizardAt | undefined {
  if (!isStep(params.step)) return undefined;
  const add = params.add ? addOf(params.add) : [];
  return {
    step: params.step,
    ...((FOCUSES as readonly string[]).includes(params.focus)
      ? { focus: params.focus as WizardAt["focus"] }
      : {}),
    ...(add.length > 0 ? { add } : {}),
  };
}

/** A wizard address as the parameters that carry it. */
export function wizardQuery(at: WizardAt): Record<string, string> {
  return {
    step: at.step,
    ...(at.focus ? { focus: at.focus } : {}),
    ...(at.add && at.add.length > 0 ? { add: addParam(at.add) } : {}),
  };
}

/**
 * The open flow, the model it is about, and the ways to open and close one.
 *
 * `wizard` IS THE ONE WAY INTO THE MODEL WIZARD, for every action that changes
 * a model: an alarm's fix, a row's menu, the model page, a By job cell. It
 * opens the wizard at the step and field the action is about, in place, or on
 * that model's page where the action is about another model; closing it
 * returns to where it was opened. An alarm's deep link arrives on the same
 * address and so opens neutral: no value is chosen for the reader. The one
 * thing an address adds is a use (`add`), and only one the reader asked for:
 * the seat's job they picked a model for, or the seats a limit holds that
 * they chose to move.
 *
 * `open` takes an address too, so adding a model can start with uses already
 * added (moving seats off a limit onto a model not yet added).
 */
export function useFlow(entryFromPage?: string) {
  const [flow, setFlow] = useParam("flow", "", "section");
  const [entryParam] = useParam("entry", "");
  const [step] = useParam("step", "");
  const [focus] = useParam("focus", "");
  const [add] = useParam("add", "");
  const nav = useNavigator();
  return {
    flow: flow as Flow | "",
    entry: entryFromPage ?? entryParam,
    at: wizardAtOf({ step, focus, add }),
    open: (next: Flow, entry?: string, at?: WizardAt) => {
      if ((entry && !entryFromPage) || at)
        nav.filter({
          ...(entry && !entryFromPage ? { entry } : {}),
          ...(at ? { ...WIZARD_PARAMS, ...wizardQuery(at) } : {}),
        });
      setFlow(next);
    },
    wizard: (key: string, at: WizardAt) => {
      if (entryFromPage && key !== entryFromPage) {
        nav.to(["settings", "models", key], { flow: "edit", ...wizardQuery(at) });
        return;
      }
      nav.filter({
        ...WIZARD_PARAMS,
        ...(entryFromPage ? {} : { entry: key }),
        ...wizardQuery(at),
      });
      setFlow("edit");
    },
    close: () => {
      nav.filter({ flow: null, entry: null, ...WIZARD_PARAMS });
    },
  };
}

/** Opens a flow, or the wizard at a step: what every action on the page calls. */
export type OnFlow = (flow: Flow, key?: string, at?: WizardAt) => void;

/** A model's condition in a word and, where it has one, when it changes. */
export function HealthLine({ model, compact = false }: { model: ModelView; compact?: boolean }) {
  const entry = model.entry;
  const words = HEALTH[entry.health];
  let detail: ReactNode = null;
  if (entry.health === "sitting_out" && entry.sitting_out) {
    detail = (
      <>
        {compact ? "back " : `${WINDOW[entry.sitting_out.window]}, back `}
        <RelativeTime value={entry.sitting_out.until} />
      </>
    );
  } else if (entry.health === "expiring" && model.admin?.sign_in.expires_at) {
    detail = `on ${fmtDate(model.admin.sign_in.expires_at)}`;
  } else if (entry.health === "rejected" && model.admin?.last_failure) {
    detail = <RelativeTime value={model.admin.last_failure.at} />;
  } else if (entry.health === "ready" && entry.last_success && !compact) {
    detail = (
      <>
        answered <RelativeTime value={entry.last_success} />
      </>
    );
  }
  return (
    <span className="models-health">
      <StatusDot tone={words.tone}>{words.label}</StatusDot>
      {detail ? (
        <Text as="span" variant="caption" tone="secondary" className="models-health__detail">
          {detail}
        </Text>
      ) : null}
    </span>
  );
}

/**
 * An identifier that may wrap only after its underscores, so a narrow column
 * shows CLAUDE_CODE_ over OAUTH_TOKEN, whole words of the name, rather than
 * cutting it to an ellipsis nothing reveals.
 */
function breakable(name: string): ReactNode {
  const parts = name.split(/(?<=_)/);
  return parts.map((part, i) => (
    <Fragment key={i}>
      {part}
      {i < parts.length - 1 ? <wbr /> : null}
    </Fragment>
  ));
}

/** How a model signs in: the route in words and, for an admin, the secret it reads; never a value. */
export function SignInLine({ model }: { model: ModelView }) {
  const route = model.entry.sign_in.route;
  const variable = model.admin?.sign_in.variable ?? null;
  return (
    <span className="col">
      <span>{ROUTE[route]}</span>
      {variable ? (
        <Text as="span" variant="caption" tone="tertiary" className="mono">
          {breakable(variable)}
        </Text>
      ) : null}
    </span>
  );
}

/**
 * The credential gesture a model's sign-in calls for: the wizard's Method step
 * with the secret open (a key or a token) or the host's sign-in shown, or a
 * device sign-in, which signs in rather than changes the model. None for a
 * member, who changes nothing here.
 */
export function credentialAction(
  model: ModelView,
): { flow: Flow; label: string; at?: WizardAt } | null {
  const signIn = model.admin?.sign_in;
  if (!signIn) return null;
  if (model.agent === "codex" && signIn.mode !== "api-key")
    return { flow: "device", label: "Sign in with a device code" };
  if (signIn.route === "credentials")
    return {
      flow: "edit",
      label: "Sign in on the host",
      at: { step: "signin", focus: "host-login" },
    };
  if (signIn.route === "api-key" || signIn.mode === "api-key")
    return { flow: "edit", label: "Replace API key", at: NEW_CREDENTIAL };
  if (signIn.route === "token") return { flow: "edit", label: "Renew token", at: NEW_CREDENTIAL };
  return null;
}

/** A model's actions, as one menu: on a row of the list and in the model's own head. Admin only. */
export function EntryMenu({
  model,
  onFlow,
  label = `Actions for ${model.key}`,
}: {
  model: ModelView;
  onFlow: OnFlow;
  label?: string;
}) {
  const credential = credentialAction(model);
  return (
    <Menu
      label={label}
      icon={<EllipsisGlyph size="sm" />}
      triggerVariant="ghost"
      align="end"
      items={[
        { key: "test", label: "Test now", onSelect: () => onFlow("test", model.key) },
        ...(credential
          ? [
              {
                key: "credential",
                label: credential.label,
                onSelect: () => onFlow(credential.flow, model.key, credential.at),
              },
            ]
          : []),
        { key: "edit", label: "Edit", onSelect: () => onFlow("edit", model.key, { step: "cli" }) },
        ...(model.executor
          ? [
              {
                key: "runs",
                label: "Change where it runs",
                onSelect: () => onFlow("edit", model.key, { step: "runs" }),
              },
            ]
          : []),
        {
          key: "limits",
          label: "Limits",
          onSelect: () => onFlow("edit", model.key, { step: "limits" }),
        },
        { kind: "separator" as const, key: "sep" },
        {
          key: "remove",
          label: "Remove",
          danger: true,
          onSelect: () => onFlow("remove", model.key),
        },
      ]}
    />
  );
}
