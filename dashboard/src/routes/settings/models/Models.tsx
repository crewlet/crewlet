/**
 * Settings › Models & Fleets: every model the company runs, what each is doing, and
 * every operator task on them from the page, with no hand edit of the company
 * document.
 *
 * One page (the models, then Ask Crewlet and the Advisor, then every seat's
 * jobs), one page per model, and the flows a task needs, each opened by the
 * address (`?flow=`), so a link, an alarm's fix or the browser's Back reaches
 * the same step. Every member can open the page and
 * read each model's condition; the reading names no credential, and every
 * action is an admin's (`llm_entries` answers each reading, and the engine
 * decides every write).
 */

import { useEffect, useMemo, useRef } from "react";
import { EmptyState, Skeleton, useToast } from "@crewlethq/ui";
import { CpuGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { MODELS_POLL_MS, useModelsData, type ModelView } from "./data.ts";
import { QueryState } from "~/components/common.tsx";
import { EntryPage } from "./EntryPage.tsx";
import { Overview } from "./Overview.tsx";
import { flowMisfit, useFlow, type OnFlow } from "./parts.tsx";
import { DeviceDialog } from "./flows/DeviceDialog.tsx";
import { RemoveDialog } from "./flows/RemoveDialog.tsx";
import { RetryDialog } from "./flows/RetryDialog.tsx";
import { TestDialog } from "./flows/TestDialog.tsx";
import { lazyScreen } from "~/app/lazyScreen.ts";
import "~/styles/models.css";

/** The add and edit wizard, a chunk of its own: drawn only while a model is added or edited. */
const ModelWizard = lazyScreen("model-wizard", (module) => module.ModelWizard);
/** The create and Settings flows of a fleet, a chunk of its own, by `?fleet=`. */
const FleetFlow = lazyScreen("fleet-wizard", (module) => module.FleetFlow);

export function Models({ id }: { id?: string }) {
  const data = useModelsData();
  const nav = useNavigator();
  // THE OLD LENS ADDRESS (`?view=jobs`) opens the one page: its grid is on it now.
  const [view] = useParam("view", "");
  useEffect(() => {
    if (view) nav.filter({ view: null });
  }, [view, nav]);
  // A model the wizard has just added, which the engine's reading may not hold yet.
  const [added, setAdded] = useParam("added", "");
  const flow = useFlow(id);
  const [fleet] = useParam("fleet", "");
  const open: OnFlow = (next, key, at) => {
    // A HOST SIGN-IN CHANGES NOTHING in the model, so it never opens the
    // wizard: off the model's page it goes to that page's sign-in card.
    if (next === "edit" && at?.focus === "host-login" && !id && key) {
      nav.to(["settings", "models", key], { focus: "host-login" });
      return;
    }
    if (next === "edit") {
      const target = key ?? id;
      if (target) flow.wizard(target, at ?? { step: "harness" });
      return;
    }
    if (id && key && key !== id) {
      nav.to(["settings", "models", key], { flow: next });
      return;
    }
    flow.open(next, key, at);
  };
  const model = useMemo(
    () => data.models.find((m) => m.key === flow.entry) ?? null,
    [data.models, flow.entry],
  );
  const misfit = useMisfit(data, flow, model);
  const addressed = id ? (data.models.find((m) => m.key === id) ?? null) : null;
  const adding = !!id && !addressed && added === id;
  useAddedMark(adding, !!addressed && added !== "", () => setAdded(""));

  if (data.loading) return <Skeleton variant="text" rows={8} label="Loading models" />;
  // A FAILED READ IS THE SHARED FAILED-READ STATE, with its Try again: it
  // printed the bare code ("query_failed") as the reason.
  if (data.failed && data.models.length === 0) {
    return (
      <QueryState
        error={data.failed.error}
        refusal={data.failed.refusal}
        {...(data.failed.detail ? { detail: data.failed.detail } : {})}
        loading={false}
      />
    );
  }

  // A FLEET'S FLOW IS A PAGE OF ITS OWN too: `new` creates one, a key edits it.
  if (data.admin && !id && fleet) return <FleetFlow data={data} fleetKey={fleet} />;

  // THE WIZARD IS A PAGE OF ITS OWN, in place of the list, so its steps have
  // the room. Adding a model and editing one are the same wizard; an edit
  // opens it filled in, at the step its action is about.
  if (data.admin && (flow.flow === "add" || (flow.flow === "edit" && model))) {
    return (
      <ModelWizard
        key={flow.flow === "edit" ? `edit-${model?.key}` : "add"}
        data={data}
        {...(flow.flow === "edit" && model ? { editing: model } : {})}
        {...(flow.at ? { target: flow.at } : {})}
        onClose={flow.close}
        onDevice={(key) => nav.to(["settings", "models", key], { flow: "device" })}
      />
    );
  }

  return (
    <>
      {id && addressed ? <EntryPage model={addressed} data={data} onFlow={open} /> : null}
      {adding ? <Skeleton variant="text" rows={6} label={`Adding ${id}`} /> : null}
      {id && !addressed && !adding ? (
        <EmptyState
          icon={<CpuGlyph size="xl" />}
          title={`No model called ${id}`}
          description="It may have been renamed or removed."
          action={
            <a className="t-link" href={href(["settings", "models"])}>
              Every model
            </a>
          }
        />
      ) : null}
      {!id ? <Overview data={data} onFlow={open} /> : null}

      {data.admin && model && !misfit && flow.flow === "test" ? (
        <TestDialog model={model} onClose={flow.close} onDone={data.reload} />
      ) : null}
      {data.admin && model && !misfit && flow.flow === "retry" ? (
        <RetryDialog model={model} data={data} onClose={flow.close} onDone={data.reload} />
      ) : null}
      {data.admin && model && !misfit && flow.flow === "remove" ? (
        <RemoveDialog
          model={model}
          data={data}
          onClose={flow.close}
          onDone={data.reload}
          // BACK TO WHERE IT WAS OPENED: the list stays the list. Only the removed model's own page has nowhere to
          // return to, and goes to the list.
          onRemoved={() => (id ? nav.to(["settings", "models"]) : flow.close())}
        />
      ) : null}
      {data.admin && model && !misfit && flow.flow === "device" ? (
        <DeviceDialog model={model} onClose={flow.close} onDone={data.reload} />
      ) : null}
    </>
  );
}

/**
 * Clears the just-added mark once the reading holds the model, or after two
 * polls, past which a model still missing is said to be missing.
 */
function useAddedMark(adding: boolean, held: boolean, clear: () => void) {
  useEffect(() => {
    if (held) clear();
    if (!adding) return;
    const t = setTimeout(clear, 2 * MODELS_POLL_MS);
    return () => clearTimeout(t);
    // `clear` writes the address, the same every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [adding, held]);
}

/** The dialog flows, which each act on one model and are opened by the address. */
const DIALOG_FLOWS: readonly string[] = ["test", "retry", "remove", "device"];

/**
 * Whether the address's dialog flow does not fit its model, checked when the
 * flow ARRIVES: a misfit is dropped from the address and its reason said. A
 * flow that fit when it opened stays open while its own write moves the model.
 */
function useMisfit(
  data: ReturnType<typeof useModelsData>,
  flow: ReturnType<typeof useFlow>,
  model: ModelView | null,
): string | null {
  const toast = useToast();
  const accepted = useRef("");
  const arrival = `${flow.flow}:${flow.entry}`;
  // A model not in the reading yet (just added, the reading catching up) waits for it.
  const misfit =
    data.admin && model && DIALOG_FLOWS.includes(flow.flow) && accepted.current !== arrival
      ? flowMisfit(flow.flow, model)
      : null;
  useEffect(() => {
    if (!DIALOG_FLOWS.includes(flow.flow)) accepted.current = "";
    else if (misfit === null && model) accepted.current = arrival;
    if (misfit === null) return;
    toast.show({ variant: "info", message: misfit });
    flow.close();
    // Once per arrival: the close moves the address, which ends the misfit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [misfit, arrival, model !== null]);
  return misfit;
}
