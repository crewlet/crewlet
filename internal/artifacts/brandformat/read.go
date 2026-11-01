package brandformat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/crewlet/crewlet/internal/jsonform"
	"github.com/crewlet/crewlet/internal/words"
)

// ---- reading a kit -------------------------------------------------------- //

// Kit is a version of a brand kit as an adoption serves it: the index, and
// every file it names in the order the adopted brand serves them.
type Kit struct {
	Brand Brand
	Files []KitFile
}

// KitFile is one file a kit serves: the slot it fills, its path in the
// version, its media type essence, and for a logo its width over its height
// (an SVG's viewBox, a raster's pixels), which the generated stylesheet hands
// the kit's `crewlet-logo` class. A face's file has no ratio.
//
// Stored is the file's stored form, as [Sanitize] writes it, from the one
// read and the one check the kit was read with: an adoption copies these
// bytes rather than reading and sanitising every file a second time.
type KitFile struct {
	Slot    Slot
	Path    string
	Essence string
	Ratio   float64
	Stored  []byte
}

// The essences a logo and a face's file may be.
var (
	logoEssences = []string{"image/svg+xml", "image/png", "image/webp"}
	fontEssence  = "font/woff2"
)

// Read reads a version of a brand kit: `brand.json` held to the closed format,
// every file it names held to its asset rule. A kit outside the format is a
// *[Refused] naming every finding, in file order.
func Read(src Source) (Brand, error) {
	k, err := ReadKit(src)
	return k.Brand, err
}

// ReadKit is [Read], answering the files the brand serves beside its index.
func ReadKit(src Source) (Kit, error) {
	return (&reader{}).kit(src)
}

// kit reads a version through r.
func (r *reader) kit(src Source) (Kit, error) {
	essence, ok := src.Essence(EntryPath)
	switch {
	case !ok:
		r.bad(0, RuleIndex, "the version holds no %s: a brand kit's index is %s at its root",
			EntryPath, EntryPath)
		return Kit{}, r.refused()
	case essence != "application/json":
		r.bad(0, RuleIndex, "%s is %s, and the index is JSON", EntryPath, essence)
		return Kit{}, r.refused()
	}
	raw, err := src.Read(EntryPath)
	if err != nil {
		return Kit{}, fmt.Errorf("brandformat: read %s: %w", EntryPath, err)
	}
	b, ok := r.decode(raw)
	if !ok {
		return Kit{}, r.refused()
	}
	r.fields(b)
	files, err := r.files(src, b)
	if err != nil {
		return Kit{}, err
	}
	if len(r.found) > 0 {
		return Kit{}, r.refused()
	}
	return Kit{Brand: b, Files: files}, nil
}

// CheckIndex is [Read] without reading a logo's or a face's bytes: `brand.json`
// held to the closed format, and every path it names present, of the type its
// field takes, with no file of the version left unnamed. It is what a publish
// whose logos were refused by their asset rule still answers beside them, so
// one refusal names the kit's every finding rather than its assets first and
// its index on the next attempt.
func CheckIndex(src Source) ([]Finding, error) {
	if _, err := (&reader{indexOnly: true}).kit(src); err != nil {
		var refused *Refused
		if errors.As(err, &refused) {
			return refused.Findings, nil
		}
		return nil, err
	}
	return nil, nil
}

// Decode reads the canonical `brand.json` an adoption snapshots: the same
// strict decode and the same field rules as [Read], with no file to check,
// since the files were checked, and copied, when the version was adopted.
func Decode(canonical []byte) (Brand, error) {
	r := &reader{}
	b, ok := r.decode(canonical)
	if ok {
		r.fields(b)
	}
	if len(r.found) > 0 {
		return Brand{}, r.refused()
	}
	return b, nil
}

// Canonical is the bytes an adoption snapshots for a kit: the index written
// back compactly, every key in the format's order, nothing escaped that JSON
// does not require, so the same brand is always the same bytes and
// Decode(Canonical(b)) is b.
func Canonical(b Brand) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(b); err != nil {
		return nil, fmt.Errorf("brandformat: encode the canonical index: %w", err)
	}
	body := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	if len(body) > MaxIndexBytes {
		return nil, fmt.Errorf("brandformat: the canonical index is %d bytes and may be at most %d",
			len(body), MaxIndexBytes)
	}
	return body, nil
}

// reader collects one kit's findings, and the line each field of the index
// was written on.
type reader struct {
	found []Finding
	dec   *jsonform.Decoded

	// indexOnly checks the files the index names by their presence and
	// type alone, never reading their bytes ([CheckIndex]).
	indexOnly bool
}

// bad reports a finding about the index itself.
func (r *reader) bad(line int, rule Rule, format string, args ...any) {
	r.found = append(r.found, Finding{Path: EntryPath, Line: line, Rule: rule, Message: fmt.Sprintf(format, args...)})
}

// field reports a finding about one field of the index, at the line the
// field was written on.
//
// A field the strict decode dropped for a structural fault is left to that
// fault: a rule about it would be about a value the author never wrote.
func (r *reader) field(name string, rule Rule, format string, args ...any) {
	line := 0
	if r.dec != nil {
		if r.dec.Dropped(name) {
			return
		}
		line = r.dec.Line(name)
	}
	r.bad(line, rule, "%s: %s", name, fmt.Sprintf(format, args...))
}

func (r *reader) refused() error { return &Refused{Findings: r.found} }

// ---- the strict decode ---------------------------------------------------- //

// IndexForm is every key `brand.json` may carry, at every depth, and the type
// of each: derived from [Brand] itself ([jsonform.Of]), so the strict decode
// admits exactly what the typed decode reads, and the reference an agent
// reads before it writes a kit ([Reference]) is the same tree.
var IndexForm = jsonform.Of[Brand]()

// decode reads the index strictly: at most [MaxIndexBytes] of UTF-8 holding
// one JSON object, no key twice in any object (encoding/json keeps the last of
// two, which another reader of the same bytes may not), no key the format does
// not carry, no value of the wrong type, nothing after it. EVERY structural
// fault is reported in the one pass, and a value faulted is left out of the
// typed decode, so the field rules still run over every field written well:
// a kit with six faults is answered with six findings, never one at a time.
func (r *reader) decode(raw []byte) (Brand, bool) {
	switch {
	case len(raw) > MaxIndexBytes:
		r.bad(0, RuleBound, "the index is %d bytes and may be at most %d", len(raw), MaxIndexBytes)
		return Brand{}, false
	case !utf8.Valid(raw):
		r.bad(0, RuleIndex, "the index is not UTF-8 text")
		return Brand{}, false
	}
	var b Brand
	r.dec = jsonform.Decode(raw, IndexForm, jsonform.Subject{File: EntryPath, Format: "the brand format"}, &b)
	for _, f := range r.dec.Faults {
		r.bad(f.Line, RuleIndex, "%s", f.Message)
	}
	return b, !r.dec.Fatal
}

// ---- the field rules ------------------------------------------------------ //

var (
	// linkPattern is what reads as a link in plain text anywhere: a scheme
	// and `//`, a `javascript:` or `data:` address, a bare `www.` host, a
	// scheme-relative `//host`, a mail address, a Markdown link's `](`, and
	// a host with a path (`acme.io/promo`). A link is the cheapest beacon an
	// injected instruction asks a model for, and a brand's text has no use
	// for one.
	linkPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://|\bjavascript:|\bdata:\S|\bwww\.\S|` +
			`(?:^|[\s(\[{"'])//\S|[^\s@]@[a-z0-9-]+(?:\.[a-z0-9-]+)+|\]\(|` +
			`\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,24}/`)
	})

	// hostPattern is a bare host (`acme.io`, `evil.example`), which a model
	// reads as an address too. Refused in every field but the name, and
	// never the brand's own name ([ownHost]): a company's own name can be a
	// domain ("Booking.com"), and is what the brand shows as its name.
	hostPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b[a-z0-9](?:[a-z0-9-]*[a-z0-9])?` +
			`(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*\.[a-z]{2,24}\b`)
	})
)

// fields holds every field of a decoded index to its rule.
func (r *reader) fields(b Brand) {
	if b.V != Version {
		r.field("v", RuleIndex, "is %d, and this build reads brand format %d: write \"v\": %d", b.V, Version, Version)
	}
	switch {
	case b.Name == "":
		r.field("name", RuleField, "is required: the company's name as the brand shows it")
	default:
		r.text("name", b.Name, MaxNameBytes, false)
	}
	r.text("tagline", b.Tagline, MaxTaglineBytes, false)
	r.noHost("tagline", b.Tagline, b.Name)

	// A DRAFT NEEDS ONLY ITS NAME: the logo and the colour an adopted brand
	// holds are the adoption's to require ([MissingToAdopt]), so a kit is
	// saved step by step as it is set up. Every field that is present is held
	// to its rule below either way.
	for _, c := range []struct{ name, value string }{
		{"colors.primary", b.Colors.Primary}, {"colors.secondary", b.Colors.Secondary},
		{"colors.text", b.Colors.Text}, {"colors.light_ground", b.Colors.LightGround},
		{"colors.dark_ground", b.Colors.DarkGround}, {"colors.primary_on_dark", b.Colors.PrimaryOnDark},
	} {
		if c.value != "" && !ColorPattern.MatchString(c.value) {
			r.field(c.name, RuleField, "%q is not a colour: write # and six lowercase hex digits, such as #2450e6", c.value)
		}
	}
	r.face("type.heading", b.Type.Heading)
	r.face("type.body", b.Type.Body)

	r.text("voice.summary", b.Voice.Summary, MaxVoiceSummaryBytes, true)
	r.noHost("voice.summary", b.Voice.Summary, b.Name)
	r.lines2("voice.write_like", b.Voice.WriteLike, b.Name)
	r.lines2("voice.avoid", b.Voice.Avoid, b.Name)
}

// noHost refuses a bare host in a field other than the name ([hostPattern]),
// where [plainText] has not already refused the field. The brand's own name
// is never refused ([ownHost]).
func (r *reader) noHost(name, s, brand string) {
	if s == "" || plainText(s, true) != "" {
		return
	}
	for _, host := range hostPattern().FindAllString(s, -1) {
		if ownHost(host, brand) {
			continue
		}
		r.field(name, RuleField, "holds %q, which reads as a web address: a brand's text names "+
			"no link, so write it in words (the brand's own name, %q, may be written as it is)",
			host, brand)
		return
	}
}

// ownHost reports a host is the brand's own name: the name itself, as a
// company named for its domain writes it ("Booking.com"), or the name and one
// top-level domain ("GoPhrase.me" for the brand GoPhrase), case aside. Only a
// host of two labels: a subdomain (`promo.gophrase.me`) is an address, and so
// is any other company's domain.
//
// THE BRAND'S `name`, NEVER THE COMPANY RECORD'S: a kit is read again at
// adoption and at every read of its version, and a rule that consulted a name
// held elsewhere would let a rename turn a kit already published into one its
// own reader refuses. The brand's name is the company's name as the brand
// shows it, and it travels with the bytes it judges.
func ownHost(host, brand string) bool {
	if brand == "" {
		return false
	}
	if strings.EqualFold(host, brand) {
		return true
	}
	label, _, ok := strings.Cut(host, ".")
	if !ok || strings.Contains(host[len(label)+1:], ".") {
		return false
	}
	return compact(label) == compact(brand)
}

// compact is a name as a domain label spells it: lowercase letters and
// digits, nothing else ("Go Phrase" and "go-phrase" are both "gophrase").
func compact(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// text holds one plain-text field: at most max bytes, NFC, trimmed, no control
// or format character (a newline only where newlines are allowed), no `<` or
// `>`, and no link.
func (r *reader) text(name, s string, max int, newlines bool) {
	if s == "" {
		return
	}
	if len(s) > max {
		r.field(name, RuleBound, "is %d bytes and may be at most %d", len(s), max)
		return
	}
	if msg := plainText(s, newlines); msg != "" {
		r.field(name, RuleField, "%s", msg)
	}
}

// plainText is why s is not plain text, or "" when it is.
func plainText(s string, newlines bool) string {
	switch {
	case !norm.NFC.IsNormalString(s):
		return "is not in Unicode normal form C: save it as composed characters"
	case strings.TrimSpace(s) != s:
		return "begins or ends with white space"
	case strings.ContainsAny(s, "<>"):
		return "holds < or >: it is plain text, never markup"
	case linkPattern().MatchString(s):
		return "holds a link or an address: a brand's text names no link"
	}
	for _, c := range s {
		switch {
		case c == '\n' && newlines:
		case unicode.Is(unicode.Cc, c), unicode.Is(unicode.Cf, c), c == '\u2028', c == '\u2029':
			return fmt.Sprintf("holds the invisible or control character %U: write plain text", c)
		}
	}
	return ""
}

// lines2 holds one list of voice lines: at most [MaxVoiceExamples], each
// non-empty plain text of at most [MaxVoiceExampleBytes] on one line.
func (r *reader) lines2(name string, list []string, brand string) {
	if len(list) > MaxVoiceExamples {
		r.field(name, RuleBound, "holds %d lines and may hold at most %d", len(list), MaxVoiceExamples)
	}
	for i, s := range list {
		item := name + "[" + strconv.Itoa(i) + "]"
		if strings.TrimSpace(s) == "" {
			r.field(item, RuleField, "is empty: drop it, or write the line")
			continue
		}
		r.text(item, s, MaxVoiceExampleBytes, false)
		r.noHost(item, s, brand)
	}
}

// face holds one typeface to EXACTLY ONE source, or to being unset.
func (r *reader) face(name string, f Face) {
	// The keys a face names are said as `brand.json` writes them, unquoted
	// like the rest of the refusal's sentence ([words.Said]).
	var named []words.Said
	for _, s := range []struct {
		key words.Said
		set bool
	}{{"kit", f.Kit != ""}, {"stack", f.Stack != ""}, {"family", f.Family != ""}} {
		if s.set {
			named = append(named, s.key)
		}
	}
	sources := len(named)
	switch {
	case sources == 0 && len(f.Files) == 0 && !f.Licensed:
		// UNSET: the kit's own face is drawn wherever the brand is.
		return
	case sources == 0:
		r.field(name, RuleField, "carries files or a licence and names no face: name the uploaded "+
			"family they belong to, or remove them")
		return
	case sources > 1:
		r.field(name, RuleField, "names %s: a face has exactly one source, so keep one of kit "+
			"(a kit face), stack (a system stack) and family (an uploaded family) and remove the "+
			"others", words.And(named))
		return
	case f.Kit != "" && !slices.Contains(KitFaces, f.Kit):
		r.field(name+".kit", RuleField, "%q is not a kit face: name one of %s", f.Kit, words.Or(KitFaces))
	case f.Stack != "" && !slices.Contains(SystemStacks, f.Stack):
		r.field(name+".stack", RuleField, "%q is not a system stack: name one of %s", f.Stack,
			words.Or(SystemStacks))
	}
	if f.Family == "" {
		if len(f.Files) > 0 || f.Licensed {
			r.field(name, RuleField, "a kit face or a system stack carries no files and no licence: "+
				"the kit and the system already hold it")
		}
		return
	}
	if !ValidFamily(f.Family) {
		r.field(name+".family", RuleField, "%q: a family name is an ASCII letter, then letters, digits, "+
			"single spaces and hyphens, at most %d bytes", f.Family, MaxFamilyNameBytes)
	}
	switch {
	case len(f.Files) == 0:
		r.field(name+".files", RuleField, "an uploaded family names its WOFF2 files")
	case len(f.Files) > MaxFontFilesPerFamily:
		r.field(name+".files", RuleBound, "names %d files and a family may have at most %d",
			len(f.Files), MaxFontFilesPerFamily)
	}
	seen := map[string]bool{}
	for i, file := range f.Files {
		item := name + ".files[" + strconv.Itoa(i) + "]"
		if file.Path == "" {
			r.field(item+".path", RuleField, "is required: the WOFF2 file of the version")
		}
		if !ValidWeight(file.Weight) {
			r.field(item+".weight", RuleField, "is %d: a weight is a whole hundred from %d to %d",
				file.Weight, MinFontWeight, MaxFontWeight)
		}
		if !slices.Contains(FontStyles, file.Style) {
			r.field(item+".style", RuleField, "%q: a style is normal or italic", file.Style)
		}
		key := strconv.Itoa(file.Weight) + " " + file.Style
		if seen[key] {
			r.field(item, RuleField, "repeats weight %d %s: each file of a family draws its own weight "+
				"and style", file.Weight, file.Style)
		}
		seen[key] = true
	}
	if !f.Licensed {
		r.field(name+".licensed", RuleLicence, "an uploaded family is published only when you attest the "+
			"company may embed it in what it publishes: set licensed to true")
	}
}

// ---- the files ------------------------------------------------------------ //

// files holds every file the index names to its asset rule, and answers them
// in the order the adopted brand serves them.
func (r *reader) files(src Source, b Brand) ([]KitFile, error) {
	var out []KitFile
	for _, l := range []struct {
		slot  Slot
		field string
		path  string
	}{
		{SlotLogoPrimary, "logos.primary", b.Logos.Primary},
		{SlotLogoOnDark, "logos.on_dark", b.Logos.OnDark},
		{SlotLogoMark, "logos.mark", b.Logos.Mark},
	} {
		if l.path == "" {
			continue
		}
		file, ok, err := r.file(src, l.field, l.path, logoEssences)
		if err != nil {
			return nil, err
		}
		if ok {
			file.Slot = l.slot
			out = append(out, file)
		}
	}
	for _, face := range []struct {
		slot  Slot
		field string
		face  Face
	}{
		{SlotFontHeading, "type.heading", b.Type.Heading},
		{SlotFontBody, "type.body", b.Type.Body},
	} {
		if face.face.Family == "" {
			continue
		}
		for i, f := range face.face.Files {
			if f.Path == "" {
				continue
			}
			file, ok, err := r.file(src, face.field+".files["+strconv.Itoa(i)+"].path", f.Path, []string{fontEssence})
			if err != nil {
				return nil, err
			}
			if ok {
				file.Slot = FontFileSlot(face.slot, i)
				out = append(out, file)
			}
		}
	}
	// A KIT IS CLOSED: its index and the files the index names, nothing else.
	// A file nothing names is one no brand surface shows and no adoption
	// copies, so it is refused rather than kept as a stowaway the version
	// carries from publish to publish.
	named := map[string]bool{EntryPath: true}
	for _, p := range []string{b.Logos.Primary, b.Logos.OnDark, b.Logos.Mark} {
		named[p] = true
	}
	for _, face := range []Face{b.Type.Heading, b.Type.Body} {
		for _, f := range face.Files {
			named[f.Path] = true
		}
	}
	paths := slices.Clone(src.Paths())
	slices.Sort(paths)
	//
	// A LOGO OR A FACE THE STRICT DECODE DROPPED names no path, so while the
	// logos or the faces hold a structural fault, a file of the type they
	// take is not called a stowaway: the fault already says what to change,
	// and the next read judges those files against the index as written.
	// Any other file is one no field could have named, and is refused still.
	unsure := map[string]bool{}
	if r.dec != nil && r.dec.Touched("logos") {
		for _, e := range logoEssences {
			unsure[e] = true
		}
	}
	if r.dec != nil && r.dec.Touched("type") {
		unsure[fontEssence] = true
	}
	for _, p := range paths {
		if essence, _ := src.Essence(p); unsure[essence] {
			continue
		}
		if !named[p] {
			r.found = append(r.found, Finding{Path: p, Rule: RuleReference, Message: "brand.json names " +
				"no logo or face at this path, and a kit holds its index and the files it names " +
				"alone: remove it, or name it in brand.json"})
		}
	}
	return out, nil
}

// Sizer is a [Source] that knows each file's size before reading it, as a
// stored version's manifest does: a file past its bound ([MaxBytes]) is then
// refused from its size, without a byte of it read.
type Sizer interface {
	Size(path string) (int64, bool)
}

// file holds one file the index names: the version holds it, as one of the
// essences the field takes, and it is inside its asset rule.
func (r *reader) file(src Source, field, path string, essences []string) (KitFile, bool, error) {
	essence, ok := src.Essence(path)
	if !ok {
		r.field(field, RuleReference, "the version holds no file at %s", path)
		return KitFile{}, false, nil
	}
	if !slices.Contains(essences, essence) {
		r.field(field, RuleAsset, "%s is %s, and the field takes %s", path, essence, strings.Join(essences, ", "))
		return KitFile{}, false, nil
	}
	if r.indexOnly {
		return KitFile{}, false, nil
	}
	if sized, ok := src.(Sizer); ok {
		if size, known := sized.Size(path); known {
			if msg := TooLarge(essence, size); msg != "" {
				r.found = append(r.found, Finding{Path: path, Rule: RuleAsset, Message: msg})
				return KitFile{}, false, nil
			}
		}
	}
	data, err := src.Read(path)
	if err != nil {
		return KitFile{}, false, fmt.Errorf("brandformat: read %s: %w", path, err)
	}
	a, findings := inspect(path, essence, data)
	if len(findings) > 0 {
		r.found = append(r.found, findings...)
		return KitFile{}, false, nil
	}
	return KitFile{Path: path, Essence: essence, Ratio: a.ratio(), Stored: a.out}, true, nil
}
