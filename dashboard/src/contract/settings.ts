/**
 * Which screen changes a setting in place. COPIES of `queries.SettingGroups`,
 * held by `internal/api`'s `TestTheDashboardReadsExactlyTheSettingsTheEngineSends`:
 * `company` is the company's clock, `code` Gitlet's commit identity, and
 * `knowledge` the knowledge base's own settings, and `sign_in` the company's
 * sign-in policy.
 */
type SettingGroup = "company" | "code" | "knowledge" | "sign_in";

/**
 * What a setting's value is, and so how it is typed. COPIES of
 * `queries.SettingKinds`: `text` a line of text (a domain), `zone` an IANA
 * time zone, `container` one space key, `containers` a list of them, and
 * `switch` on or off.
 */
type SettingKind = "text" | "zone" | "container" | "containers" | "switch";

/** A setting's value: a string, a list of strings, or a switch's state, by `kind`. */
type SettingValue = string | string[] | boolean;

/**
 * One setting of the company document that is not a number: `queries.SettingRow`
 * in Go, the numeric `LimitRow`'s sibling. `value` is what the engine APPLIES,
 * its default applied, so a screen keeps no copy of a default; `set` is whether
 * the document writes `key`, which these settings remember.
 *
 * NO RULE OF THE VALUE IS COPIED HERE: the engine checks a change before it is
 * saved (`lib/useSettingWrite.ts` runs the dry run), and its refusal carries a
 * person's words (`said`).
 */
export interface SettingRow {
  /** The setting's path in the company document, e.g. `code.identity_domain`. */
  key: string;
  group: SettingGroup;
  kind: SettingKind;
  value: SettingValue;
  /** What the engine applies where the document writes nothing, as it derives it for this company. */
  default: SettingValue;
  /** The company document writes `key`. */
  set: boolean;
  /** `value` switches the setting off: an empty skills container is no tool skills. */
  off: boolean;
  /** The setting has an Off of its own, an empty value distinct from its default. */
  has_off: boolean;
}

/**
 * The `settings` answer, an administrator's: every setting that applies to the
 * company, in the engine's drawing order. A code setting is answered only where
 * Gitlet hosts code, and a knowledge setting only where a knowledge base runs.
 * Empty on a node running no company.
 */
export interface SettingsAnswer {
  settings: SettingRow[];
}
