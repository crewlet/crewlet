/**
 * A person's answers to a finding, and what the page says of each: Snooze,
 * End the snooze, Dismiss with a reason, and Reopen a dismissed one. One hook
 * for the list and a finding's page, so both say the same.
 *
 * A DISMISSAL CAN BE TAKEN BACK at once: its toast offers Undo, which is the
 * engine's reopen.
 *
 * A REOPENED DISMISSAL IS SAID AS IT IS. A dismissed finding is closed for
 * good (a closed episode never opens again): its reopen lifts the hold the
 * dismissal put on the condition, and the condition comes back as a new
 * finding at the Advisor's next check, if it still holds. The engine answers
 * the dismissed episode as it was, so the page says the hold was lifted and
 * what follows, never "Reopened" over a finding that still reads Dismissed.
 */

import { useCallback } from "react";
import { useToast } from "@crewlethq/ui";
import { useAssistantControl } from "~/app/assistant/control.ts";
import { useNavigator, useRoute } from "~/app/router.tsx";
import { fmtAt } from "~/lib/format.ts";
import { decide } from "./api.ts";
import { questionAbout } from "./rows.tsx";
import type { AdvisorDecision, AdvisorKeptFinding } from "~/protocol/advisorFindings.ts";

/** Why a person dismisses a finding. */
export type DismissReason = Extract<AdvisorDecision, { decision: "dismiss" }>["reason"];

/** What a press answers with: the toast's words, from the finding the engine answered. */
interface Said {
  title: string;
  message: string;
  undo?: () => void;
}

/** The answers, each sent and said; `reload` reads the page again after each lands. */
export function useDecisions(reload: () => void) {
  const toast = useToast();
  const answer = useCallback(
    (f: AdvisorKeptFinding, decision: AdvisorDecision, say: (now: AdvisorKeptFinding) => Said) => {
      void decide(f.id, decision).then(
        (now) => {
          const said = say(now ?? f);
          toast.show({
            variant: "success",
            title: said.title,
            message: said.message,
            ...(said.undo ? { action: { label: "Undo", onClick: said.undo } } : {}),
          });
          reload();
        },
        () =>
          toast.show({
            variant: "danger",
            title: "Nothing was changed",
            message: "The engine did not take that. Try again in a moment.",
          }),
      );
    },
    [toast, reload],
  );
  const reopen = useCallback(
    (f: AdvisorKeptFinding) =>
      answer(f, { decision: "reopen" }, (now) =>
        now.state === "open"
          ? { title: "Back on the list", message: f.title }
          : {
              title: "Dismissal lifted",
              message: "It comes back on the list at the Advisor's next check, if it still holds.",
            },
      ),
    [answer],
  );
  return {
    snooze: (f: AdvisorKeptFinding) =>
      answer(f, { decision: "snooze" }, (now) => ({
        title: now.snoozed_until
          ? `Snoozed until ${fmtAt(now.snoozed_until, Date.now())}`
          : "Snoozed for 7 days",
        message: f.title,
      })),
    // A SNOOZE ENDED EARLY: the engine's reopen of an open finding.
    wake: (f: AdvisorKeptFinding) =>
      answer(f, { decision: "reopen" }, () => ({ title: "Back on the list", message: f.title })),
    dismiss: (f: AdvisorKeptFinding, reason: DismissReason) =>
      answer(f, { decision: "dismiss", reason }, () => ({
        title: "Dismissed",
        message: f.title,
        undo: () => reopen(f),
      })),
    reopen,
  };
}

/**
 * Ask Crewlet about a finding, or null where the reader may not ask: the
 * finding's own page, so the page context names it and its facts stand beside
 * a new conversation, with the question waiting in the box for the person to
 * send or change: the action's own, else [questionAbout]. Never asked for
 * them: the drawer sent a question they had not read, in their name.
 */
export function useAskAbout(): ((f: AdvisorKeptFinding, question?: string) => void) | null {
  const assistant = useAssistantControl();
  const nav = useNavigator();
  const route = useRoute();
  if (assistant.unavailable) return null;
  return (f, question) => {
    const page = ["advisor", "findings", f.id];
    if (route.path.join("/") !== page.join("/")) nav.to(page);
    assistant.askAbout(question ?? questionAbout(f), { send: false });
  };
}
