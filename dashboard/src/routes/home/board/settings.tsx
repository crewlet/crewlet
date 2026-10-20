/**
 * A widget's settings, in a sheet beside the page, in one of two moments.
 *
 * EDITING ONE ON THE PAGE: every change applies to the board as it is made,
 * so the widget on the page is the preview. Done keeps the changes; Cancel
 * takes back everything changed since the sheet opened.
 *
 * ADDING ONE FROM THE TRAY that has a choice to make (which page, which saved
 * view): nothing is on the page yet. The sheet previews the widget as it will
 * draw, Add to Home places it once its choices are made, and the step back
 * beside the title returns to the tray with nothing added.
 *
 * COMMITTED FROM THE HEAD, as every sheet is (uilet's Modal): the way back
 * leads it, the commit ends it, and neither is a scroll away in a foot.
 */

import { useState } from "react";
import {
  Button,
  FormField,
  HuePicker,
  IconButton,
  Input,
  MarkdownEditor,
  Modal,
  SegmentedControl,
  Select,
  Skeleton,
} from "@crewlethq/ui";
import { PlusGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import { renderMarkdown } from "~/lib/markdown.ts";
import type { WidgetParam } from "./kit.tsx";
import { widgetDef } from "./registry.tsx";
import { LeafView } from "./widgets.tsx";
import {
  LIMITS,
  linkHref,
  type ContribWidget,
  type LinkRow,
  type Tone,
  type Widget,
} from "./model.ts";

/** Whether a widget has anything to set. */
export const configurable = (w: Widget) =>
  w.kind !== "widget" || (widgetDef(w.widget)?.params?.length ?? 0) > 0;

/** What a widget is called, for its controls and the sheet's title. */
export function widgetName(w: Widget): string {
  switch (w.kind) {
    case "widget": {
      const def = widgetDef(w.widget);
      return def ? (def.nameOf?.(w.params) ?? def.label) : "Unavailable widget";
    }
    case "group":
      return w.title || "Untitled group";
    case "heading":
      return w.text ? `Heading “${w.text.slice(0, 30)}”` : "Heading";
    case "divider":
      return w.label ? `Divider “${w.label}”` : "Divider";
    case "note":
      return "Note";
    case "links":
      return w.title || "Links";
  }
}

/** The first choice a contributed widget still needs, in words, or nothing. */
function missingChoice(w: Widget): string | undefined {
  if (w.kind !== "widget") return undefined;
  const param = widgetDef(w.widget)?.params?.find((p) => !w.params[p.key]);
  return param ? `Choose a ${param.label.toLowerCase()} first.` : undefined;
}

export type WidgetSettingsProps =
  | {
      mode: "edit";
      widget: Widget;
      onChange: (next: Widget) => void;
      /** Keep the changes. */
      onDone: () => void;
      /** Take back every change made since the sheet opened. */
      onCancel: () => void;
    }
  | {
      mode: "add";
      widget: Widget;
      onChange: (next: Widget) => void;
      /** Place it on the page. */
      onAdd: () => void;
      /** Back to the tray, adding nothing. */
      onBack: () => void;
      /** Close the sheet, adding nothing. */
      onClose: () => void;
    };

export function WidgetSettings(props: WidgetSettingsProps) {
  const { widget, onChange } = props;
  const name = widgetName(widget);
  if (props.mode === "add") {
    return (
      <Modal
        open
        variant="sheet"
        backdrop="clear"
        stackBody
        title={`Add ${name}`}
        subtitle="Choose what it shows, then add it."
        onClose={props.onClose}
        onBack={props.onBack}
        backLabel="Back to widgets"
        headerActions={
          <Button
            variant="primary"
            size="small"
            disabledReason={missingChoice(widget)}
            onClick={props.onAdd}
          >
            Add to Home
          </Button>
        }
      >
        <Fields widget={widget} onChange={onChange} />
        {widget.kind === "widget" && !missingChoice(widget) ? (
          <section className="home-settings-preview" aria-label={`Preview of ${name}`}>
            <div className="home-tray-head">
              <h3 className="home-tray-title">Preview</h3>
              <p className="home-tray-hint">Links open in a new tab.</p>
            </div>
            <LeafView widget={widget} />
          </section>
        ) : null}
      </Modal>
    );
  }
  return (
    <Modal
      open
      variant="sheet"
      backdrop="clear"
      stackBody
      title={`Edit ${name}`}
      subtitle="Changes apply as you make them."
      onClose={props.onDone}
      showCloseButton={false}
      headerActions={
        <>
          <Button variant="secondary" size="small" onClick={props.onCancel}>
            Cancel
          </Button>
          <Button variant="primary" size="small" onClick={props.onDone}>
            Done
          </Button>
        </>
      }
    >
      <Fields widget={widget} onChange={onChange} />
    </Modal>
  );
}

function ToneField({ value, onChange }: { value: Tone; onChange: (t: Tone) => void }) {
  return (
    <FormField label="Color" as="fieldset">
      <HuePicker label="Color" none="Plain" value={value} onValueChange={onChange} />
    </FormField>
  );
}

function Fields({ widget, onChange }: { widget: Widget; onChange: (next: Widget) => void }) {
  switch (widget.kind) {
    case "group":
      return (
        <>
          <FormField label="Name" helper="Such as Delivery, Spend or Needs attention.">
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                value={widget.title}
                maxLength={LIMITS.title}
                onChange={(e) => onChange({ ...widget, title: e.target.value })}
              />
            )}
          </FormField>
          <ToneField value={widget.tone} onChange={(tone) => onChange({ ...widget, tone })} />
        </>
      );
    case "note":
      return (
        <>
          <MarkdownEditor
            label="Text"
            value={widget.text}
            placeholder="A goal, a reminder, how to read the page."
            onChange={(text) => onChange({ ...widget, text: text.slice(0, LIMITS.note) })}
            renderPreview={(text) => (
              <div className="prose md">{renderMarkdown(text, { headings: "text" })}</div>
            )}
          />
          <ToneField value={widget.tone} onChange={(tone) => onChange({ ...widget, tone })} />
          <Shape
            size={widget.size}
            align={widget.align}
            onChange={(next) => onChange({ ...widget, ...next })}
            sizes={{ md: "Regular", lg: "Large" }}
          />
        </>
      );
    case "heading":
      return (
        <>
          <FormField label="Text">
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                value={widget.text}
                maxLength={LIMITS.heading}
                onChange={(e) => onChange({ ...widget, text: e.target.value })}
              />
            )}
          </FormField>
          <Shape
            size={widget.size}
            align={widget.align}
            onChange={(next) => onChange({ ...widget, ...next })}
            sizes={{ md: "Section", lg: "Page" }}
          />
        </>
      );
    case "divider":
      return (
        <>
          <FormField
            label="Label"
            optional
            helper="Shown on the rule, such as History or Reference."
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                value={widget.label}
                maxLength={LIMITS.title}
                onChange={(e) => onChange({ ...widget, label: e.target.value })}
              />
            )}
          </FormField>
          <FormField label="Label position" as="fieldset">
            <SegmentedControl
              label="Label position"
              semantics="radio"
              options={[
                { value: "start", label: "Start" },
                { value: "center", label: "Center" },
              ]}
              value={widget.align}
              onValueChange={(v) => onChange({ ...widget, align: v as "start" | "center" })}
            />
          </FormField>
        </>
      );
    case "links":
      return <LinksFields widget={widget} onChange={onChange} />;
    case "widget":
      return (
        <>
          {(widgetDef(widget.widget)?.params ?? []).map((param) => (
            <ParamField key={param.key} param={param} widget={widget} onChange={onChange} />
          ))}
        </>
      );
  }
}

/** One of a contributed widget's choices, offered from what its section reads now. */
function ParamField({
  param,
  widget,
  onChange,
}: {
  param: WidgetParam;
  widget: ContribWidget;
  onChange: (next: Widget) => void;
}) {
  const choices = param.useChoices();
  const value = widget.params[param.key] ?? "";
  return (
    <FormField label={param.label} helper={param.helper}>
      {(field) =>
        choices.loading ? (
          <Skeleton variant="text" rows={1} rowHeight={36} />
        ) : choices.options.length === 0 ? (
          <p id={field.id} className="home-card-note">
            {choices.empty ?? "Nothing to choose from yet."}
          </p>
        ) : (
          <>
            <Select
              id={field.id}
              aria-describedby={field.describedBy}
              value={value === "" ? undefined : value}
              placeholder={`Choose a ${param.label.toLowerCase()}`}
              options={choices.options.map((o) => ({ value: o.value, label: o.label }))}
              onChange={(v) =>
                onChange({ ...widget, params: { ...widget.params, [param.key]: String(v) } })
              }
            />
            {choices.partial && <p className="home-card-note">{choices.partial}</p>}
          </>
        )
      }
    </FormField>
  );
}

function Shape({
  size,
  align,
  sizes,
  onChange,
}: {
  size: "md" | "lg";
  align: "start" | "center";
  sizes: Record<"md" | "lg", string>;
  onChange: (next: { size: "md" | "lg"; align: "start" | "center" }) => void;
}) {
  return (
    <div className="home-settings-shape">
      <FormField label="Size" as="fieldset">
        <SegmentedControl
          label="Size"
          semantics="radio"
          options={[
            { value: "md", label: sizes.md },
            { value: "lg", label: sizes.lg },
          ]}
          value={size}
          onValueChange={(v) => onChange({ size: v as "md" | "lg", align })}
        />
      </FormField>
      <FormField label="Alignment" as="fieldset">
        <SegmentedControl
          label="Alignment"
          semantics="radio"
          options={[
            { value: "start", label: "Left" },
            { value: "center", label: "Center" },
          ]}
          value={align}
          onValueChange={(v) => onChange({ size, align: v as "start" | "center" })}
        />
      </FormField>
    </div>
  );
}

/** A title and up to twelve links; an address that is not a dashboard or web address is refused where it is typed. */
function LinksFields({
  widget,
  onChange,
}: {
  widget: Extract<Widget, { kind: "links" }>;
  onChange: (w: Widget) => void;
}) {
  // What is being typed, so an address can be half-written without being dropped.
  const [rows, setRows] = useState<LinkRow[]>(widget.links);
  const commit = (next: LinkRow[]) => {
    setRows(next);
    onChange({
      ...widget,
      links: next.flatMap((r) => {
        const checked = linkHref(r.href);
        return "href" in checked
          ? [{ label: r.label.slice(0, LIMITS.linkLabel), href: checked.href }]
          : [];
      }),
    });
  };
  return (
    <>
      <FormField label="Title">
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            value={widget.title}
            maxLength={LIMITS.title}
            onChange={(e) => onChange({ ...widget, title: e.target.value })}
          />
        )}
      </FormField>
      <div className="home-settings-links" role="group" aria-label="Links">
        {rows.map((row, i) => {
          const checked = row.href === "" ? null : linkHref(row.href);
          const error = checked && "error" in checked ? checked.error : undefined;
          return (
            <div key={i} className="home-settings-link-row">
              <FormField label={`Link ${i + 1} name`}>
                {(field) => (
                  <Input
                    id={field.id}
                    aria-describedby={field.describedBy}
                    value={row.label}
                    maxLength={LIMITS.linkLabel}
                    placeholder="Runbook"
                    onChange={(e) =>
                      commit(rows.map((r, j) => (j === i ? { ...r, label: e.target.value } : r)))
                    }
                  />
                )}
              </FormField>
              <FormField label={`Link ${i + 1} address`} error={error}>
                {(field) => (
                  <Input
                    id={field.id}
                    aria-describedby={field.describedBy}
                    error={Boolean(error)}
                    value={row.href}
                    maxLength={LIMITS.linkHref}
                    placeholder="#/inbox or https://"
                    onChange={(e) =>
                      commit(rows.map((r, j) => (j === i ? { ...r, href: e.target.value } : r)))
                    }
                  />
                )}
              </FormField>
              <IconButton
                variant="ghost-danger"
                label={`Remove link ${i + 1}`}
                icon={<TrashGlyph size="sm" />}
                onClick={() => commit(rows.filter((_, j) => j !== i))}
              />
            </div>
          );
        })}
      </div>
      <Button
        variant="secondary"
        leadingIcon={<PlusGlyph size="sm" />}
        disabledReason={
          rows.length >= LIMITS.links ? `A list holds up to ${LIMITS.links} links.` : undefined
        }
        onClick={() => setRows([...rows, { label: "", href: "" }])}
      >
        Add link
      </Button>
    </>
  );
}
