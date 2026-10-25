// Package brandformat is the format a `brand` artifact holds: a brand kit, the
// company's name, logos, colour roles, two typefaces and voice, checked at
// publish and handed to every consumer (the dashboard's Brand page and native
// renderers, the generated brand stylesheet, the slide theme, the agents'
// `get_company_brand`) as one canonical tree.
//
// THE FILES. A kit is a version holding `brand.json` ([EntryPath]), the closed
// index below ([Brand]), and the logo and font files it names, each a file of
// the same version named by its path from the version's root. Nothing in
// `brand.json` is a URL: a logo or a face is a path, a colour is `#rrggbb`, a
// face from the kit or the system is a closed name.
//
// A CLOSED FORMAT, REFUSED RATHER THAN DROPPED. Every key `brand.json` may
// carry is a field of [Brand]; the index is decoded strictly (no key the format
// does not carry, no key twice in any object, since encoding/json keeps the
// last of two where another reader keeps the first, nothing after the object),
// and an unknown key, a value outside its grammar or bound, a path the version
// does not hold, and an asset outside its rules are each refused at publish
// ([Read]) with a [Finding] naming the file, the line where there is one, the
// rule and the fix, every finding of the kit in one pass.
//
// THE STORED FORM. A logo or a face is stored as [Sanitize] writes it: an SVG
// re-serialised by internal/artifacts/svgsafe, a PNG without the ancillary
// chunks that carry metadata, a WebP without its EXIF and XMP, a WOFF2 as it
// came once its header is inside the bounds. Nothing else is ever stripped. The
// publish pipeline stores that form, and an adoption sanitizes every file it
// copies again, so what the public brand route serves is always this build's
// form; [Read] therefore holds a file to its rule, not to byte equality with
// the form, and a later build's form never blocks adopting an earlier version.
//
// THE FIELD RULES, which [Brand]'s field docs state one by one and the
// validator holds:
//
//   - `v` is [Version].
//   - `name` is 1 to [MaxNameBytes] bytes; `tagline` 0 to [MaxTaglineBytes].
//     Plain text: NFC, trimmed, no control or format character, no `<` or
//     `>`, no link (a scheme and `//`, `javascript:`, a `data:` address, a
//     `www.` host).
//   - `name` is the one field a publish requires: a kit is saved step by step
//     while it is set up. `logos.primary` and `colors.primary` are required
//     to ADOPT the brand ([AdoptionFields], [MissingToAdopt]); a draft may
//     leave them out, and every field it holds is held to its rule.
//   - `logos.primary`, `logos.on_dark` and `logos.mark` are each the path of an SVG ([MaxSVGBytes], sanitised by
//     internal/artifacts/svgsafe, a `viewBox` required), a PNG or a WebP
//     ([MaxImageBytes], at most [MaxImageSide] pixels a side and
//     [MaxImagePixels] in all, APNG refused).
//   - `colors.primary`, `secondary`, `text`, `light_ground`, `dark_ground`
//     and `primary_on_dark` are colour roles, an absent optional role taking
//     the kit's Crewlet fallback. Each is `#rrggbb`, lowercase hex
//     ([ColorPattern]). Contrast is judged on the effective values (fallbacks
//     filled): `text` on `light_ground` below [MinTextContrast] refuses an
//     ADOPTION (never a publish: a draft may be unfinished); every other pair
//     ([ContrastPairs]) warns. An absent role is measured as the kit paints
//     it ([Fallbacks]).
//   - `type.heading` and `type.body` each name EXACTLY ONE source: a kit face
//     (`kit`: one of [KitFaces]), a curated system stack (`stack`: one of
//     [SystemStacks]), or an uploaded family (`family`, 1 to
//     [MaxFamilyNameBytes] bytes, an ASCII letter then letters, digits, single
//     spaces and hyphens, with 1 to [MaxFontFilesPerFamily] WOFF2 `files`,
//     each at most [MaxFontFileBytes], a weight a whole hundred from 100 to
//     900 and a style `normal` or `italic`, no weight and style twice, and
//     `licensed` true: the publisher attests the company may embed it). A face
//     that names none of the three and carries no file and no licence is
//     unset, and the kit's own face (Geist) is drawn in its place.
//   - `voice.summary` is 0 to [MaxVoiceSummaryBytes] bytes; `voice.write_like`
//     and `voice.avoid` are each up to [MaxVoiceExamples] lines of at most
//     [MaxVoiceExampleBytes] bytes. No URL anywhere in the voice. The voice
//     reaches a model only through `get_company_brand`, fenced under the
//     `company` provenance trust.
//
// THE SLOTS. A file the adopted brand serves fills one [Slot] ([ReadKit]
// answers them in the order the brand serves them); the `from_brand` file
// source an agent copies into a version names a slot, never a path, so a copy
// reads only the adopted snapshot.
//
// WHAT AN ADOPTION MAKES OF A KIT is made only from an [Adoptable], a brand that
// holds every field an adoption requires: the canonical index it snapshots
// ([Canonical], read back with [Decode]), the one stylesheet generated from it
// ([CSS]: `--brand-*` inputs on `:root` and `@font-face` rules, every value
// re-checked as it is written), the extensions the public route serves its
// files under ([Ext], [ServedExts], [ContentType]), and the slide theme a deck
// begun in the brand copies ([SlideTheme]).
//
// PURE. It reads no store and holds no clock: the publish pipeline hands it
// the files' bytes through an [artifacts.FileSource], and the adoption and the
// read routes call it on the same bytes.
//
// ITS PRIVATE TABLES ARE BUILT AT FIRST USE: the link and host patterns, the
// slide family grammar and the PNG and WebP chunk tables are each a
// [sync.OnceValue], built the first time a kit is read. The engine is one
// binary, and that binary is also every push's proc-receive hook, which runs
// every linked package's initialisation before its first statement — for a
// process that reads no kit. cmd/crewlet's TestTheHookPaysLittleForTheEnginesInit
// holds every package to that. The exported patterns and [FaceFamilies] stay
// variables: they are this format's published grammar, which other packages
// and the dashboard's copies are held to.
package brandformat
