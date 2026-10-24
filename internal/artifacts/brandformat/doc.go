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
// carry is a field of [Brand]; an unknown key, a value outside its grammar or
// bound, a path the version does not hold, and an asset outside its rules are
// each refused at publish with a [Finding] naming the file, the line where
// there is one, the rule and the fix. Nothing is silently stripped except the
// metadata an asset rule names (PNG ancillary chunks, WebP EXIF and XMP).
//
// THE FIELD RULES, which [Brand]'s field docs state one by one and the
// validator holds:
//
//   - `v` is [Version].
//   - `name` is 1 to [MaxNameBytes] bytes; `tagline` 0 to [MaxTaglineBytes].
//     Plain text: no control or format character, no URL.
//   - `logos.primary` is required; `logos.on_dark` and `logos.mark` are
//     optional. Each is the path of an SVG ([MaxSVGBytes], sanitised by
//     internal/artifacts/svgsafe, a `viewBox` required), a PNG or a WebP
//     ([MaxImageBytes], at most [MaxImageSide] pixels a side and
//     [MaxImagePixels] in all, APNG refused).
//   - `colors.primary` is required; `secondary`, `text`, `light_ground`,
//     `dark_ground` and `primary_on_dark` are optional, an absent role taking
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
//     spaces and hyphens, with 1 to [MaxFontFilesPerFamily] WOFF2 `files`, each at most
//     [MaxFontFileBytes], and `licensed` true: the publisher attests the
//     company may embed it).
//   - `voice.summary` is 0 to [MaxVoiceSummaryBytes] bytes; `voice.write_like`
//     and `voice.avoid` are each up to [MaxVoiceExamples] lines of at most
//     [MaxVoiceExampleBytes] bytes. No URL anywhere in the voice. The voice
//     reaches a model only through `get_company_brand`, fenced under the
//     `company` provenance trust.
//
// THE SLOTS. A file the adopted brand serves fills one [Slot]; the `from_brand`
// file source an agent copies into a version names a slot, never a path, so a
// copy reads only the adopted snapshot.
//
// PURE. It reads no store and holds no clock: the publish pipeline hands it
// the files' bytes through an [artifacts.FileSource], and the adoption and the
// read routes call it on the same bytes.
package brandformat
