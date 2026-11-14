/**
 * A model's test as a screen runs it ({@link checkModel}): started over REST,
 * and its outcome asked as the `llm_check` question on the socket, which any
 * node answers from the test's record — so a test the node behind a balancer
 * started is read wherever the next question lands.
 */

import { useCallback } from "react";
import { useClient } from "~/lib/store-hooks.ts";
import type { CheckAnswer } from "~/protocol/models.ts";
import { checkModel, type ModelAnswer } from "./api.ts";

/** The test of a model, as {@link checkModel} with this screen's socket asking its outcome. */
export type CheckModel = (
  key: string,
  options: { smoke: boolean; entity?: Record<string, unknown> },
  signal?: AbortSignal,
) => Promise<ModelAnswer<CheckAnswer>>;

export function useCheckModel(): CheckModel {
  const { socket } = useClient();
  return useCallback(
    (key, options, signal) =>
      checkModel(key, options, (asked) => socket.query("llm_check", { key: asked }), signal),
    [socket],
  );
}
