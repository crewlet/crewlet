/**
 * Company › Brand › Set up (`#/company/brand/setup`): the company's brand set
 * up in six steps, never in a dialog.
 *
 * # Every Continue is a draft
 *
 * Continue saves the step into a draft version of the company's kit (a
 * publish, never an adoption); the first Continue makes the kit, shared with
 * the company to view. A setup left and opened again resumes at the first step
 * that is not done ([resumeStep]) with everything it saved. Each save is keyed,
 * and a save whose answer nobody knows is sent again with the same keys.
 *
 * # How it ends
 *
 * An administrator, a person, finishes with **Publish as the company brand**
 * (the engine asks for a recent sign-in when the proof is stale); a person the
 * kit is shared with to edit finishes with **Save for review**, a draft an
 * administrator adopts from the Brand page. Both ask first that every
 * low-contrast pair is acknowledged; the one pair that blocks an adoption
 * holds the Colours step itself. A finish tried too early says what holds it
 * where it is fixed, on Review itself, and moves focus there.
 *
 * # One page of the kit's parts
 *
 * The steps are the Brand page's card editors (`BrandEditors.tsx`), so a
 * value is chosen and refused the same way in both, and the preview beside
 * them is the Brand page's, drawing the draft as it stands.
 */

import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import {
  Button,
  Card,
  ConfirmModal,
  EditingStatus,
  EmptyState,
  SetupWizard,
  Skeleton,
  useAnnouncer,
  useToast,
  type StepperStatus,
} from "@crewlethq/ui";
import { SwatchBookGlyph } from "@crewlethq/icons/glyphs";
import { useLeaveGuard, useNavigator } from "~/app/router.tsx";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { adoptBrand, type BrandSaveFile } from "~/lib/brand/api.ts";
import { serializeBrandDocument, type BrandLogoSlot } from "~/lib/brand/brand.ts";
import { draftSource, saveFiles, type BrandEdit } from "~/lib/brand/draft.ts";
import {
  SETUP_STEPS,
  nextStep,
  previousStep,
  resumeStep,
  stepAttention,
  stepDone,
  stepSpec,
  type SetupStepId,
} from "~/lib/brand/setup.ts";
import { adoptionProblem, partProblem } from "~/lib/brand/validate.ts";
import { useBrandAssets } from "~/lib/brand/useBrand.ts";
import {
  BRAND_ASSISTANT_HELP,
  applyBrandDraft,
  brandDraftFields,
  type BrandDraft,
} from "~/lib/brand/assistantDraft.ts";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import { useBrandDraftSave } from "~/lib/brand/useBrandSave.ts";
import { BRAND_PAGE } from "~/components/brand.tsx";
import { BrandPreview } from "../BrandPreview.tsx";
import { ColoursStep, IdentityStep, LogosStep, TypographyStep, VoiceStep } from "./steps.tsx";
import { ReviewStep, setupWarnings, warningKey } from "./Review.tsx";
import { useSetupTarget, type SetupTarget } from "./useSetupTarget.ts";

const TITLE = "Set up your brand";

/** Every part of the brand is the setup's to change, whichever step is open. */
const SETUP_FIELDS = brandDraftFields(new Set(["identity", "logos", "colours", "type", "voice"]));

/** A step's place in the setup. */
const indexOf = (id: SetupStepId) => SETUP_STEPS.findIndex((s) => s.id === id);

export function BrandSetup() {
  const read = useSetupTarget();
  if (read.state === "loading") {
    return (
      <div aria-busy="true">
        <Card>
          <Skeleton variant="text" rows={8} label="Loading the brand setup" />
        </Card>
      </div>
    );
  }
  if (read.state === "refused") {
    return (
      <Card>
        <EmptyState
          icon={<SwatchBookGlyph size="lg" />}
          title="Only an administrator sets up the brand"
          description="An administrator, or a person the brand kit is shared with to edit, sets up the company's brand."
        />
      </Card>
    );
  }
  // OPENED AFRESH for another kit or version: what the setup holds is the
  // draft it opened on.
  return <SetupFlow key={read.target.key} target={read.target} admin={read.admin} />;
}

/** What the setup last saved: the kit, its version, the document's text, the files it brought, and when. */
interface Saved {
  kit: string | null;
  version: number;
  head: number;
  text: string;
  files: ReadonlyMap<string, File>;
  at: Date | null;
}

/** The logo slots an engine refusal names, by the path each logo is saved at. */
function refusedLogos(
  findings: unknown,
  doc: BrandEdit["document"],
): Partial<Record<BrandLogoSlot, string>> {
  const out: Partial<Record<BrandLogoSlot, string>> = {};
  if (!Array.isArray(findings)) return out;
  for (const f of findings as { path?: unknown; message?: unknown; line?: unknown }[]) {
    const slot = (Object.keys(doc.logos) as BrandLogoSlot[]).find((s) => doc.logos[s] === f.path);
    if (!slot || typeof f.message !== "string") continue;
    out[slot] = typeof f.line === "number" ? `Line ${f.line}: ${f.message}` : f.message;
  }
  return out;
}

function SetupFlow({ target, admin }: { target: SetupTarget; admin: boolean }) {
  const nav = useNavigator();
  const toast = useToast();
  const announce = useAnnouncer();
  const draftSave = useBrandDraftSave();
  const adopt = useKeyedGesture();
  const body = useRef<HTMLDivElement>(null);

  const initial: BrandEdit = useMemo(
    () => ({ document: target.document, files: new Map() }),
    [target.document],
  );
  const [edit, setEdit] = useState<BrandEdit>(initial);
  // A FRESH SETUP STARTS AT THE START, its name prefilled from the company's;
  // a draft resumes at its first step not done.
  const [step, setStepNow] = useState<SetupStepId>(() =>
    target.kit ? resumeStep(initial) : "identity",
  );
  // THE FURTHEST STEP VISITED, so walking back to Logos keeps every step up
  // to it a button in the rail.
  const [reached, setReached] = useState<SetupStepId>(step);
  const setStep = (to: SetupStepId) => {
    setStepNow(to);
    setReached((was) => (indexOf(to) > indexOf(was) ? to : was));
  };
  const [saved, setSaved] = useState<Saved>(() => ({
    kit: target.kit,
    version: target.version,
    head: target.head,
    text: serializeBrandDocument(target.document),
    files: new Map(),
    at: null,
  }));
  const [refused, setRefused] = useState<Partial<Record<BrandLogoSlot, string>>>({});
  const [said, setSaid] = useState<string | null>(null);
  const [acknowledged, setAcknowledged] = useState<ReadonlySet<string>>(new Set());
  // A FINISH WAS TRIED while something held it: Review marks what, in place.
  const [tried, setTried] = useState(false);
  const [held, setHeld] = useState(0);
  const [leaving, setLeaving] = useState<{ leave: () => void } | null>(null);
  const [finishing, setFinishing] = useState(false);

  const source = useMemo(() => draftSource(target.source, edit), [target.source, edit]);
  const assets = useBrandAssets(source);

  // WHAT A LEAVE WOULD LOSE: anything changed since the setup opened or last
  // saved. A fresh setup has nothing saved yet, so its first Continue saves
  // even an unchanged step: that is what makes the draft kit.
  const dirty =
    serializeBrandDocument(edit.document) !== saved.text ||
    [...edit.files].some(([path, file]) => saved.files.get(path) !== file);
  const unsaved = dirty || saved.kit === null;

  useLeaveGuard(
    dirty && !finishing
      ? (_to, leave) => {
          setLeaving({ leave });
          return true;
        }
      : null,
  );

  /**
   * Save the draft as it stands, when anything changed: what is saved once it
   * is (the version a finish adopts), or null when it was not.
   */
  const save = useCallback(async (): Promise<Saved | null> => {
    if (!unsaved) return saved;
    setSaid(null);
    let files: BrandSaveFile[];
    try {
      files = await saveFiles(target.source, edit, undefined, {
        version: saved.kit ? saved.version : 0,
        saved: saved.files,
      });
    } catch (err) {
      setSaid(err instanceof Error ? err.message : String(err));
      return null;
    }
    const answer = await draftSave.run({
      kit: saved.kit,
      head: saved.head,
      title: `${edit.document.name.trim()} brand`,
      document: edit.document,
      files,
    });
    if (!answer) return null;
    if (answer.kind === "saved") {
      const now: Saved = {
        kit: answer.kit,
        version: answer.version,
        head: answer.version,
        text: serializeBrandDocument(edit.document),
        files: new Map(edit.files),
        at: new Date(),
      };
      setRefused({});
      setSaved(now);
      return now;
    }
    if (answer.kind === "refused")
      setRefused(refusedLogos(answer.error?.body.findings, edit.document));
    setSaid(answer.text);
    announce(answer.text);
    return null;
  }, [unsaved, target.source, edit, saved, draftSave, announce]);

  /** Hold a step that cannot be saved: focus its first refused field and say why. */
  const hold = (problem: string) => {
    setSaid(problem);
    announce(problem);
    const field =
      body.current?.querySelector<HTMLElement>('[aria-invalid="true"]') ??
      body.current?.querySelector<HTMLElement>("input, button");
    field?.focus();
  };

  const go = async (to: SetupStepId) => {
    const part = stepSpec(step).part;
    const problem = part ? partProblem(part, edit) : undefined;
    const forward = indexOf(to) > indexOf(step);
    if (forward && problem) return hold(problem);
    if (!problem && (await save()) === null) return;
    setSaid(null);
    setStep(to);
  };

  const warnings = setupWarnings(edit.document);
  const unacknowledged = warnings.filter((w) => !acknowledged.has(warningKey(w))).length;
  const ready = adoptionProblem(edit.document);
  const finishHold =
    ready ??
    (unacknowledged > 0
      ? `Confirm the ${unacknowledged === 1 ? "low-contrast pair" : `${unacknowledged} low-contrast pairs`} first.`
      : undefined);

  // FOCUS GOES TO WHAT HOLDS THE FINISH once Review has drawn it: the part
  // missing a required value, or the first pair not yet confirmed.
  useEffect(() => {
    if (held > 0) body.current?.querySelector<HTMLElement>("[data-hold]")?.focus();
  }, [held]);

  const finish = async () => {
    if (finishHold) {
      // THE FIRST TIME, the marks themselves are alerts and say it; after
      // that they are already drawn, so the reason is said again.
      if (tried) announce(finishHold);
      setTried(true);
      setHeld((n) => n + 1);
      return;
    }
    const now = await save();
    if (now === null) return;
    if (!admin) {
      setFinishing(true);
      toast.ok("Saved for review. An administrator publishes it from the Brand page.");
      nav.to(BRAND_PAGE);
      return;
    }
    if (!now.kit) return;
    const answer = await adopt.run(adoptBrand(now.kit, now.version));
    if (answer?.kind === "done") {
      setFinishing(true);
      toast.ok(`The ${edit.document.name.trim()} brand is published`);
      nav.to(BRAND_PAGE);
    } else if (answer) {
      setSaid(answer.text);
      announce(answer.text);
    }
  };

  const saveDraft = async () => {
    if ((await save()) !== null) toast.ok("Draft saved");
  };

  // ENTER CONTINUES from a one-line field, when the field did not take the
  // key itself (a list's field adds a line on Enter).
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== "Enter" || e.defaultPrevented || e.nativeEvent.isComposing) return;
    const field = e.target as HTMLElement;
    if (field.tagName !== "INPUT") return;
    const type = (field as HTMLInputElement).type;
    if (type === "checkbox" || type === "color" || type === "file" || type === "radio") return;
    if (step === "review") return;
    e.preventDefault();
    void go(nextStep(step));
  };

  // THE CURRENT STEP KEEPS ITS OWN WORD: the rail draws it as current, and
  // says too that it is optional or needs attention.
  const steps = SETUP_STEPS.map((s) => {
    const status: StepperStatus = stepAttention(s.id, edit, Object.keys(refused).length > 0)
      ? "attention"
      : s.id === step
        ? s.optional
          ? "optional"
          : "current"
        : stepDone(s.id, edit)
          ? "done"
          : s.optional
            ? "optional"
            : "upcoming";
    return { id: s.id, label: s.label, status };
  });

  const busy = draftSave.busy || adopt.busy;
  // WHAT IS OPEN, for the assistant: the draft as it stands, at its step, and
  // its text and value fields set through the same `setEdit` typing uses;
  // Continue, Save as draft and the finish stay the person's.
  useAssistantDraft<BrandDraft>({
    kind: "client",
    id: saved.kit,
    revision: saved.kit ? saved.version : null,
    step,
    help: BRAND_ASSISTANT_HELP,
    scope: "The brand you are setting up (not saved)",
    value: edit,
    fields: SETUP_FIELDS,
    apply: (next) =>
      applyBrandDraft(edit, next, setEdit, (sentence) => {
        setSaid(sentence);
        announce(sentence);
      }),
    blocked: () => (busy ? "The brand is being saved." : null),
  });
  const spec = stepSpec(step);
  // THE STEP'S PRIMARY ACTION is the wizard's own slot: it keeps one width on
  // every step, and on a phone it takes the footer's second row by name.
  const footer =
    step === "review" ? (
      <>
        <Button variant="ghost" onClick={() => void go(previousStep(step))}>
          Back
        </Button>
        <Button variant="secondary" loading={draftSave.busy} onClick={() => void saveDraft()}>
          Save as draft
        </Button>
      </>
    ) : step !== "identity" || spec.optional ? (
      <>
        {step !== "identity" ? (
          <Button variant="ghost" onClick={() => void go(previousStep(step))}>
            Back
          </Button>
        ) : null}
        {spec.optional ? (
          <Button variant="ghost" onClick={() => setStep(nextStep(step))}>
            Skip for now
          </Button>
        ) : null}
      </>
    ) : undefined;
  const primary =
    step === "review" ? (
      <Button variant="primary" loading={busy} onClick={() => void finish()}>
        {admin ? "Publish as the company brand" : "Save for review"}
      </Button>
    ) : (
      <Button variant="primary" loading={draftSave.busy} onClick={() => void go(nextStep(step))}>
        Continue
      </Button>
    );

  const editorProps = { edit, onEdit: setEdit, assets };
  return (
    <>
      <SetupWizard
        title={TITLE}
        status={
          <EditingStatus
            dirty={dirty}
            saving={draftSave.busy}
            {...(saved.at ? { savedAt: saved.at } : {})}
            labels={{ editing: "Draft" }}
          />
        }
        // EXIT AT THE START OF THE FOOTER, apart from the step's own actions at
        // its end; Review takes the page's width for its two columns.
        footerStart={
          <Button variant="ghost" onClick={() => nav.to(BRAND_PAGE)}>
            Exit
          </Button>
        }
        wide={step === "review"}
        steps={steps}
        current={step}
        reached={reached}
        onSelect={(id: string) => void go(id as SetupStepId)}
        stepsLabel="Brand setup steps"
        heading={spec.label}
        lead={spec.lead}
        {...(step === "review"
          ? {}
          : {
              aside: (
                <BrandPreview
                  document={edit.document}
                  assets={assets}
                  drawn="changes"
                  title="Preview of your draft"
                />
              ),
              asideLabel: "Preview of your draft",
            })}
        footer={footer}
        primaryAction={primary}
      >
        <div ref={body} onKeyDown={onKeyDown} className="brand-setup-body">
          {/* WHY THE STEP IS HELD, over its fields, where it is read first. */}
          {said ? (
            <p className="brand-refusal" role="alert">
              {said}
            </p>
          ) : null}
          {step === "identity" ? (
            <IdentityStep edit={edit} onEdit={setEdit} />
          ) : step === "logos" ? (
            <LogosStep {...editorProps} refused={refused} />
          ) : step === "colours" ? (
            <ColoursStep {...editorProps} />
          ) : step === "type" ? (
            <TypographyStep {...editorProps} />
          ) : step === "voice" ? (
            <VoiceStep {...editorProps} />
          ) : (
            <ReviewStep
              edit={edit}
              assets={assets}
              admin={admin}
              showErrors={tried}
              onEditStep={(to) => setStep(to)}
              acknowledged={acknowledged}
              onAcknowledge={(key, on) =>
                setAcknowledged((was) => {
                  const next = new Set(was);
                  if (on) next.add(key);
                  else next.delete(key);
                  return next;
                })
              }
            />
          )}
        </div>
      </SetupWizard>
      <ConfirmModal
        open={leaving !== null}
        onClose={() => setLeaving(null)}
        onConfirm={() => {
          const go = leaving?.leave;
          setLeaving(null);
          setFinishing(true);
          go?.();
        }}
        title="Leave the setup?"
        message="Your changes to this step are not saved. Every step you continued from is kept as a draft."
        confirmLabel="Leave"
        cancelLabel="Keep editing"
        destructive
      />
    </>
  );
}
