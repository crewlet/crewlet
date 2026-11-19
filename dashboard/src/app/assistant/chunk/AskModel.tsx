/**
 * Ask Crewlet's model, where its page and its panel name it: the model's
 * name for anybody, and the picker for a reader who may assign it
 * (`components/HelperModel.tsx`). Whether the reader may is the engine's
 * answer (`viewer.assistant.can_assign`), as is the model assigned.
 *
 * A COMPANY WITH NO MODEL is told so with the picker in the same place the
 * conversation would be, for a reader who may assign one: choosing it is
 * what turns Ask Crewlet on, so it is the one step offered.
 */

import { EmptyState, Text } from "@crewlethq/ui";
import { WandSparklesGlyph } from "@crewlethq/icons/glyphs";
import { HelperModel, useHelperModel } from "~/components/HelperModel.tsx";
import { rereadViewer, useViewer } from "~/lib/viewer.ts";

/** The model in a conversation's head: its name, or the picker. */
export function AskModel() {
  const viewer = useViewer();
  const assigned = viewer.assistantAssigned;
  if (!viewer.assistantCanAssign) {
    return assigned ? (
      <Text as="span" variant="caption" tone="secondary" title={assigned.label}>
        Runs on {assigned.key}
      </Text>
    ) : null;
  }
  return <AskModelPicker />;
}

function AskModelPicker() {
  const viewer = useViewer();
  const model = useHelperModel("assistant");
  return (
    <HelperModel
      helper="assistant"
      current={viewer.assistantAssigned?.key ?? model.current}
      choices={model.choices}
      canAssign
      onApplied={() => {
        model.reload();
        rereadViewer();
      }}
    />
  );
}

/** No model assigned, said to a reader who may assign one, with the picker. */
export function ChooseAskModel() {
  return (
    <EmptyState
      icon={<WandSparklesGlyph size="lg" />}
      title="Choose the model Ask Crewlet runs on"
      description="Ask Crewlet is off until it has a model. Any of the company's models a helper can run on will do; it runs with no tools of its own, whatever the model's entry allows."
      action={<AskModelPicker />}
    />
  );
}
