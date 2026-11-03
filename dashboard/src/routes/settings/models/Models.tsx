/**
 * Settings › Models: every model the company runs, what each is doing, and
 * every operator task on them from the page, with no hand edit of the company
 * document.
 *
 * Two lenses (Models, By job), one page per model, and the flows a task
 * needs, each opened by the address (`?flow=`), so a link, an alarm's fix or
 * the browser's Back reaches the same step. Every member can open the page and
 * read each model's condition; the reading names no credential, and every
 * action is an admin's (`llm_entries` answers each reading, and the engine
 * decides every write).
 */

import { useMemo } from "react";
import { Callout, EmptyState, Skeleton, Tabs } from "@crewlethq/ui";
import { CpuGlyph } from "@crewlethq/icons/glyphs";
import { PageLenses } from "~/app/frame/PageActions.tsx";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useModelsData } from "./data.ts";
import { EntryPage } from "./EntryPage.tsx";
import { Overview } from "./Overview.tsx";
import { useFlow, type OnFlow } from "./parts.tsx";
import { ByJob } from "./ByJob.tsx";
import { DeviceDialog } from "./flows/DeviceDialog.tsx";
import { RemoveDialog } from "./flows/RemoveDialog.tsx";
import { RetryDialog } from "./flows/RetryDialog.tsx";
import { TestDialog } from "./flows/TestDialog.tsx";
import { ModelWizard } from "./wizard/ModelWizard.tsx";
import "~/styles/models.css";

export function Models({ id }: { id?: string }) {
  const data = useModelsData();
  const nav = useNavigator();
  const [view, setView] = useParam("view", "", "section");
  const flow = useFlow(id);
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

  if (data.loading) return <Skeleton variant="text" rows={8} label="Loading models" />;
  if (data.error && data.models.length === 0) {
    return (
      <Callout variant="danger" title="The models could not be read">
        {data.error}
      </Callout>
    );
  }

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

  const addressed = id ? (data.models.find((m) => m.key === id) ?? null) : null;

  return (
    <>
      {!id ? (
        <PageLenses>
          <Tabs
            variant="chip"
            ariaLabel="Lens"
            value={view || "models"}
            onValueChange={(v) => setView(v === "models" ? "" : v)}
            items={[
              { value: "models", label: "Models" },
              { value: "jobs", label: "By job" },
            ]}
          />
        </PageLenses>
      ) : null}

      {id && addressed ? <EntryPage model={addressed} data={data} onFlow={open} /> : null}
      {id && !addressed ? (
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
      {!id && view !== "jobs" ? <Overview data={data} onFlow={open} /> : null}
      {!id && view === "jobs" ? <ByJob data={data} onFlow={open} /> : null}

      {data.admin && model && flow.flow === "test" ? (
        <TestDialog model={model} onClose={flow.close} onDone={data.reload} />
      ) : null}
      {data.admin && model && flow.flow === "retry" ? (
        <RetryDialog model={model} data={data} onClose={flow.close} onDone={data.reload} />
      ) : null}
      {data.admin && model && flow.flow === "remove" ? (
        <RemoveDialog model={model} data={data} onClose={flow.close} onDone={data.reload} />
      ) : null}
      {data.admin && model && flow.flow === "device" ? (
        <DeviceDialog model={model} onClose={flow.close} onDone={data.reload} />
      ) : null}
    </>
  );
}
