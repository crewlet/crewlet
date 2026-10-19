package lint

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/html"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// NonceAttr is what the linter writes into every script element, straight
// after its name so it is the first nonce a browser reads. [Source] removes
// exactly this text, which no source may contain ([CodePlaceholder]).
const NonceAttr = ` nonce="` + artifacts.NoncePlaceholder + `"`

// Options configure one lint.
type Options struct {
	// Hosts are the CDN hosts a page may load from.
	Hosts Allowlist

	// Resolve copies an allowlisted CDN file into the version and answers
	// the path the version serves it at, relative to the referencing file's
	// directory. Nil turns copying off, and every allowlisted reference is
	// then refused ([CodeVendorOff]).
	Resolve func(Ref) (string, error)

	// HasWasm reports that the version holds a WebAssembly file, which a
	// page compiles.
	HasWasm bool
}

// Ref is one remote reference the linter asks [Options.Resolve] to copy in.
type Ref struct {
	// URL is the file, absolute.
	URL *url.URL

	// Kind is what the reference loads.
	Kind RefKind

	// Integrity is the author's subresource integrity value, empty where
	// they gave none: the copy must match it.
	Integrity string

	// From is the path of the file holding the reference, and Line where.
	From string
	Line int
}

// Page is a linted HTML file.
type Page struct {
	// Text is the file as it is stored: every script element carrying the
	// nonce placeholder and every allowlisted reference pointing at the
	// version's copy.
	Text []byte

	// Policy is what the serving origin builds the page's script policy from.
	Policy artifacts.ScriptPolicy

	// Warnings are what is legal and almost certainly not meant. Whether
	// the page follows its reader's appearance is not among them: the
	// publish pipeline answers that for the whole version ([ThemeOf]).
	Warnings []Finding
}

// maxWarnings bounds how many warnings of one code one file reports: past it
// the author has the pattern, and a list a page long hides the refusal beside
// it.
const maxWarnings = 10

// HTML lints one HTML file of a version: it REFUSES what the serving contract
// forbids, REWRITES every script element to carry the nonce placeholder and
// every allowlisted CDN reference to the version's own copy, derives the
// page's script policy, and WARNS on what is legal and wrong.
//
// # Why every script carries the nonce
//
// The artifact origin answers each document with a fresh nonce and
// 'strict-dynamic', so the page's own scripts run and anything they create
// runs, while a script a viewer's datastore row injects (an <img onerror> in
// a value written into the page) carries no nonce and does not. A script
// element WITH the nonce runs from wherever its source points, which is why
// every remote script source is copied in or refused here: the linter, not
// the policy, keeps a page's scripts its own.
//
// # Why it parses once and verifies once
//
// The file is tokenized by the standard HTML tokenizer, decisions are taken
// on the attributes as it decodes them, and the rewrite replaces only the
// bytes it must, leaving the rest of the page as the author wrote it. The
// result is then tokenized again and every script element in it checked:
// carrying the placeholder as the nonce a browser reads, and pointing at no
// other origin. A page that fails that is refused ([CodeUnparseable]), never
// answered with a script the linter did not account for.
func HTML(path string, src []byte, opts Options) (Page, error) {
	l := &linter{
		fs: &findings{path: path}, path: path, opts: opts, hashes: map[string]bool{},
		policy: artifacts.ScriptPolicy{Nonced: true, WasmEval: opts.HasWasm},
		warned: map[Code]int{},
	}
	if i := bytes.Index(src, []byte(artifacts.NoncePlaceholder)); i >= 0 {
		l.fs.add(lineAt(src, i), CodePlaceholder, "the page carries the engine's nonce "+
			"placeholder, which only the engine writes; remove it")
		return Page{}, l.fs.err()
	}
	out, consumed, err := l.rewrite(src)
	if err != nil {
		l.fs.add(l.line, CodeUnparseable, "the page could not be read as HTML: %v", err)
	} else if consumed != len(src) {
		l.fs.add(l.line, CodeUnparseable, "the page could not be read to its end as HTML")
	}
	if err := l.fs.err(); err != nil {
		return Page{}, err
	}
	if err := verify(out, l.scripts); err != nil {
		l.fs.add(0, CodeUnparseable, "the rewritten page could not be verified: %v", err)
		return Page{}, l.fs.err()
	}
	if !l.viewport {
		l.warn(0, CodeNoViewport, "the page has no <meta name=\"viewport\">, so it renders at "+
			"desktop width on a phone")
	}
	for h := range l.hashes {
		l.policy.HandlerHashes = append(l.policy.HandlerHashes, h)
	}
	slices.Sort(l.policy.HandlerHashes)
	return Page{Text: out, Policy: l.policy, Warnings: l.fs.warnings}, nil
}

// linter is one HTML file's lint in progress.
type linter struct {
	fs     *findings
	path   string
	opts   Options
	line   int
	policy artifacts.ScriptPolicy
	hashes map[string]bool
	warned map[Code]int

	scripts   int
	inScript  bool
	scriptJS  bool
	importMap bool
	inStyle   bool
	viewport  bool
}

// warn records a warning, at most [maxWarnings] of one code.
func (l *linter) warn(line int, code Code, format string, args ...any) {
	if l.warned[code] >= maxWarnings {
		return
	}
	l.warned[code]++
	l.fs.add(line, code, format, args...)
}

// rewrite is the one pass: every token copied as written, but for the ones a
// rule rewrites. It answers how many source bytes the tokens covered, which
// must be all of them.
func (l *linter) rewrite(src []byte) ([]byte, int, error) {
	z := html.NewTokenizer(bytes.NewReader(src))
	var out bytes.Buffer
	out.Grow(len(src) + 64)
	l.line = 1
	consumed := 0
	for {
		tt := z.Next()
		raw := slices.Clone(z.Raw())
		if tt == html.ErrorToken {
			if errors.Is(z.Err(), io.EOF) {
				return out.Bytes(), consumed, nil
			}
			return nil, consumed, z.Err()
		}
		line := l.line
		l.line += bytes.Count(raw, []byte("\n"))
		consumed += len(raw)
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			out.Write(l.startTag(tok, raw, line))
		case html.EndTagToken:
			switch strings.ToLower(z.Token().Data) {
			case "script":
				l.inScript = false
				l.importMap = false
			case "style":
				l.inStyle = false
			}
			out.Write(raw)
		case html.TextToken:
			switch {
			case l.inScript && l.importMap:
				out.Write(l.importMapText(raw, line))
			case l.inScript:
				l.scriptText(raw, line)
				out.Write(raw)
			case l.inStyle:
				out.Write(cssRewrite(l.fs, l.path, raw, l.opts, nil, line-1))
			default:
				out.Write(raw)
			}
		default:
			out.Write(raw)
		}
	}
}

// urlAttrs are the attributes whose value is a URL a browser navigates to,
// loads or submits to: a javascript: URL in any of them is refused.
var urlAttrs = []string{"href", "src", "action", "formaction", "xlink:href", "data", "poster", "background", "cite", "srcset"}

// blockedTags are the elements that load a resource the artifact origin's
// policy confines to the version itself.
var blockedTags = []string{"img", "source", "video", "audio", "track", "iframe", "embed", "object", "image", "use", "input", "frame"}

// startTag lints one start tag and answers its bytes as stored.
func (l *linter) startTag(tok html.Token, raw []byte, line int) []byte {
	name := strings.ToLower(tok.Data)
	attrs := rawAttrs(raw)
	values := map[int]string{}
	get := func(key string) (string, bool) {
		for _, a := range tok.Attr {
			if a.Namespace == "" && a.Key == key || a.Namespace != "" && a.Namespace+":"+a.Key == key {
				return a.Val, true
			}
		}
		return "", false
	}
	first := func(key string) int {
		return slices.IndexFunc(attrs, func(a rawAttr) bool { return a.name == key })
	}
	for _, a := range tok.Attr {
		key := a.Key
		if a.Namespace != "" {
			key = a.Namespace + ":" + a.Key
		}
		if len(key) > 2 && strings.HasPrefix(key, "on") {
			sum := sha256.Sum256([]byte(a.Val))
			l.hashes[base64.StdEncoding.EncodeToString(sum[:])] = true
		}
		if slices.Contains(urlAttrs, key) && isJavaScriptURL(a.Val) {
			l.fs.add(line, CodeJavaScriptURL, "<%s %s> is a javascript: URL, which the "+
				"artifact policy does not run; attach a handler in a script instead", name, key)
		}
		if key == "style" && strings.Contains(strings.ToLower(a.Val), "url(") {
			for _, r := range scanCSS([]byte(a.Val)) {
				if IsRemote(r.url) {
					l.warn(line, CodeRemoteBlocked, "<%s style> loads %s, which the artifact "+
						"policy will not load from another origin; upload it into the artifact",
						name, r.url)
				}
			}
		}
	}
	insert := ""
	switch name {
	case "script":
		l.scripts++
		l.inScript = true
		typ, _ := get("type")
		typ = mediaEssence(typ)
		l.scriptJS = jsType(typ)
		l.importMap = typ == "importmap"
		if transformType(typ) {
			l.policy.UnsafeEval = true
		}
		if _, ok := get("nonce"); ok {
			l.warn(line, CodeAuthorNonce, "<script> carries a nonce of its own, which the "+
				"engine's per-response nonce replaces; remove it")
		}
		kind := RefScript
		if typ == "module" {
			kind = RefModule
		}
		integrity, _ := get("integrity")
		for _, key := range []string{"src", "href", "xlink:href"} {
			v, ok := get(key)
			if !ok || strings.TrimSpace(v) == "" || isJavaScriptURL(v) {
				continue
			}
			if l.importMap {
				// AN IMPORT MAP IS READ INLINE. One fetched from a source
				// would be a list of module addresses nothing here reads,
				// so it is refused rather than copied in as a script.
				if IsRemote(v) {
					l.fs.add(line, CodeRemoteImport, "<script type=\"importmap\" %s=%q> loads "+
						"an import map from another origin; write the map inline", key, v)
				}
				continue
			}
			if !IsRemote(v) {
				if transformPath(v) {
					l.policy.UnsafeEval = true
				}
				continue
			}
			if s := scheme(normalizeRef(v)); s != "" && s != "https" && s != "http" {
				l.fs.add(line, CodeRemoteScript, "<script %s> loads a %s: URL; put the script "+
					"inline or in a file of the artifact instead", key, s)
				continue
			}
			u, err := absolute(v)
			if err != nil {
				l.fs.add(line, CodeRemoteScript, "<script %s=%q> is not a URL a page can load: %v",
					key, v, err)
				continue
			}
			local, ok := vendorRef(l.fs, l.opts, Ref{URL: u, Kind: kind, Integrity: integrity,
				From: l.path, Line: line})
			if !ok {
				continue
			}
			if i := first(key); i >= 0 {
				values[i] = local
			}
			if transformPath(u.Path) {
				l.policy.UnsafeEval = true
			}
		}
		insert = NonceAttr
	case "link":
		href, _ := get("href")
		rel, _ := get("rel")
		rels := strings.Fields(strings.ToLower(rel))
		if href == "" || !IsRemote(href) {
			break
		}
		switch {
		case slices.Contains(rels, "stylesheet"):
			u, err := absolute(href)
			if err != nil {
				l.fs.add(line, CodeRemoteStylesheet, "<link href=%q> is not a URL a page can "+
					"load: %v", href, err)
				break
			}
			integrity, _ := get("integrity")
			local, ok := vendorRef(l.fs, l.opts, Ref{URL: u, Kind: RefStylesheet,
				Integrity: integrity, From: l.path, Line: line})
			if ok {
				if i := first("href"); i >= 0 {
					values[i] = local
				}
			}
		case slices.ContainsFunc(rels, func(r string) bool {
			return slices.Contains([]string{"icon", "apple-touch-icon", "preload",
				"modulepreload", "prefetch", "manifest", "mask-icon"}, r)
		}):
			l.warn(line, CodeRemoteBlocked, "<link rel=%q> loads %s, which the artifact policy "+
				"will not load from another origin; upload it into the artifact", rel, href)
		}
	case "style":
		l.inStyle = true
	case "form":
		if action, ok := get("action"); ok && IsRemote(action) && !isJavaScriptURL(action) {
			l.fs.add(line, CodeFormOffOrigin, "<form action=%q> posts to another origin, which "+
				"an artifact may not; keep the data in the artifact's datastore", action)
		}
	case "button", "input":
		if action, ok := get("formaction"); ok && IsRemote(action) && !isJavaScriptURL(action) {
			l.fs.add(line, CodeFormOffOrigin, "<%s formaction=%q> posts to another origin, which "+
				"an artifact may not", name, action)
		}
		typ, _ := get("type")
		auto, _ := get("autocomplete")
		if strings.EqualFold(strings.TrimSpace(typ), "password") ||
			slices.ContainsFunc(strings.Fields(strings.ToLower(auto)), func(t string) bool {
				return slices.Contains([]string{"current-password", "new-password", "cc-number",
					"cc-csc", "cc-exp"}, t)
			}) {
			l.warn(line, CodePasswordField, "the page asks for a password or a card number; an "+
				"artifact never collects credentials or payment details")
		}
	case "meta":
		equiv, _ := get("http-equiv")
		metaName, _ := get("name")
		if strings.EqualFold(strings.TrimSpace(metaName), "viewport") {
			l.viewport = true
		}
		if strings.EqualFold(strings.TrimSpace(equiv), "refresh") {
			content, _ := get("content")
			if target := refreshTarget(content); target != "" && IsRemote(target) {
				l.fs.add(line, CodeRefreshOffOrigin, "<meta http-equiv=refresh> sends the "+
					"reader to %s, another origin, which an artifact may not", target)
			}
		}
	}
	if slices.Contains(blockedTags, name) {
		for _, key := range []string{"src", "srcset", "poster", "data", "href", "xlink:href"} {
			v, ok := get(key)
			if !ok || isJavaScriptURL(v) {
				continue
			}
			target := v
			if key == "srcset" {
				target, _, _ = strings.Cut(strings.TrimSpace(v), " ")
			}
			if IsRemote(target) && !strings.EqualFold(scheme(normalizeRef(target)), "data") {
				l.warn(line, CodeRemoteBlocked, "<%s %s> loads %s, which the artifact policy "+
					"will not load from another origin; upload it into the artifact", name, key, target)
			}
		}
	}
	if len(values) == 0 && insert == "" {
		return raw
	}
	return rewriteTag(raw, attrs, values, insert)
}

// scriptText reads one inline script's text.
func (l *linter) scriptText(text []byte, line int) {
	if !l.scriptJS {
		return
	}
	lower := bytes.ToLower(text)
	if bytes.Contains(lower, []byte("localstorage")) || bytes.Contains(lower, []byte("sessionstorage")) {
		l.warn(line, CodeBrowserStorage, "the page uses browser storage, which is per viewer, "+
			"empty on an opaque origin and never shared; declare the state capability for "+
			"per-viewer state, or db for state the viewers share")
	}
	if bytes.Contains(text, []byte("WebAssembly")) {
		l.policy.WasmEval = true
	}
	for _, im := range RemoteImports(Imports(text)) {
		l.fs.add(line+im.Line-1, CodeRemoteImport, "the script imports %s, another origin, "+
			"which a page loads at view time; load it with a classic <script src> from an "+
			"allowlisted CDN at an exact version, or upload the module into the artifact",
			im.Specifier)
	}
}

// verify tokenizes a rewritten page again and holds every script element to
// what the rewrite promised: the placeholder as the nonce a browser reads, a
// source on the version's own origin, and an import map naming no module on
// another. want is how many scripts the rewrite counted; the placeholder must
// occur exactly once for each.
func verify(out []byte, want int) error {
	if n := bytes.Count(out, []byte(artifacts.NoncePlaceholder)); n != want {
		return fmt.Errorf("the placeholder occurs %d times for %d scripts", n, want)
	}
	z := html.NewTokenizer(bytes.NewReader(out))
	got := 0
	inMap := false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if !errors.Is(z.Err(), io.EOF) {
				return z.Err()
			}
			if got != want {
				return fmt.Errorf("the page holds %d scripts and the rewrite counted %d", got, want)
			}
			return nil
		case html.EndTagToken:
			inMap = false
		case html.TextToken:
			if !inMap {
				continue
			}
			entries, _, err := importMapEntries(z.Raw())
			if err != nil {
				return fmt.Errorf("an import map could not be read: %w", err)
			}
			for _, e := range entries {
				if IsRemote(e.address) {
					return fmt.Errorf("an import map still maps %q to %q", e.specifier, e.address)
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			if !strings.EqualFold(tok.Data, "script") {
				continue
			}
			got++
			nonce, typ := "", ""
			for _, a := range tok.Attr {
				key := a.Key
				if a.Namespace != "" {
					key = a.Namespace + ":" + a.Key
				}
				switch key {
				case "nonce":
					if nonce == "" {
						nonce = a.Val
					}
				case "type":
					if typ == "" {
						typ = mediaEssence(a.Val)
					}
				case "src", "href", "xlink:href":
					if IsRemote(a.Val) || isJavaScriptURL(a.Val) {
						return fmt.Errorf("a script still loads %q", a.Val)
					}
				}
			}
			if nonce != artifacts.NoncePlaceholder {
				return errors.New("a script does not carry the placeholder as its nonce")
			}
			inMap = typ == "importmap"
		}
	}
}

// isJavaScriptURL reports a javascript: URL, read as a browser reads it.
func isJavaScriptURL(v string) bool { return scheme(normalizeRef(v)) == "javascript" }

// refreshTarget is the URL a meta refresh's content names, empty for none.
func refreshTarget(content string) string {
	_, rest, ok := strings.Cut(content, ";")
	if !ok {
		_, rest, ok = strings.Cut(content, ",")
		if !ok {
			return ""
		}
	}
	rest = strings.TrimSpace(rest)
	if len(rest) >= 3 && strings.EqualFold(rest[:3], "url") {
		rest = strings.TrimSpace(rest[3:])
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "="))
	}
	return strings.Trim(rest, `"'`)
}

// mediaEssence is a type attribute without parameters, lower case.
func mediaEssence(t string) string {
	t, _, _ = strings.Cut(t, ";")
	return strings.ToLower(strings.TrimSpace(t))
}

// jsType reports a script type a browser runs as JavaScript, or an in-browser
// transform compiles to it.
func jsType(t string) bool {
	switch t {
	case "", "module", "text/javascript", "application/javascript", "application/ecmascript",
		"text/ecmascript", "application/x-javascript", "text/jsx":
		return true
	}
	return transformType(t)
}

// transformType reports a script type only an in-browser compiler runs, which
// compiles the script and evaluates the result: the page needs string
// evaluation.
func transformType(t string) bool {
	switch t {
	case "text/babel", "text/jsx", "text/x-babel", "text/typescript", "text/tsx":
		return true
	}
	return false
}

// transformPath reports a script file that compiles and evaluates code in the
// browser (an in-browser JSX compiler, a framework's template compiler, a
// framework that evaluates its attributes), by the package its path names: on
// a CDN or as the version's own copy of one, which keeps the CDN's path under
// the copy's.
func transformPath(p string) bool {
	p = strings.ToLower(p)
	base := p[strings.LastIndex(p, "/")+1:]
	switch {
	case strings.Contains(p, "@babel/standalone"), strings.Contains(p, "babel-standalone"):
		return true
	case strings.Contains(p, "petite-vue"):
		return true
	case strings.Contains(p, "alpinejs") && !strings.Contains(p, "csp"):
		return true
	case (strings.Contains(p, "/vue@") || strings.Contains(p, "/libs/vue/")) &&
		!strings.Contains(base, "runtime"):
		return true
	}
	return false
}

// lineAt is the line byte i of text is on.
func lineAt(text []byte, i int) int { return 1 + bytes.Count(text[:i], []byte("\n")) }

// asRefused is errors.As for the allowlist's refusal.
func asRefused(err error, target **RefusedRef) bool { return errors.As(err, target) }

// Source is a stored page as its author wrote it: the nonce placeholders the
// linter wrote removed, so an edit or a read sees the author's text and a
// second lint writes them again where they were.
func Source(text []byte) []byte { return bytes.ReplaceAll(text, []byte(NonceAttr), nil) }

// Stored is the script policy of a page the linter already wrote, a carried
// file a version names again: its source linted again with nothing copied in
// (every reference it holds is already the version's own), and refused unless
// that lint writes back exactly the stored bytes.
func Stored(path string, text []byte, opts Options) (artifacts.ScriptPolicy, error) {
	opts.Resolve = nil
	page, err := HTML(path, Source(text), opts)
	if err != nil {
		return artifacts.ScriptPolicy{}, err
	}
	if !bytes.Equal(page.Text, text) {
		return artifacts.ScriptPolicy{}, errors.New("lint: the stored page is not in the form " +
			"the linter writes")
	}
	return page.Policy, nil
}

// htmlLocalRefs is every script, stylesheet, style-element and import map
// reference a page makes, as written. An import map's addresses are among
// them because a copied module is named by the map alone: a carried page
// whose map points at a copy needs the copy in the version beside it.
func htmlLocalRefs(text []byte) []string {
	var out []string
	z := html.NewTokenizer(bytes.NewReader(text))
	inStyle, inMap := false, false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			name := strings.ToLower(tok.Data)
			inStyle = name == "style"
			inMap = false
			for _, a := range tok.Attr {
				switch {
				case name == "script" && (a.Key == "src" || a.Key == "href"):
					out = append(out, a.Val)
				case name == "script" && a.Key == "type":
					inMap = mediaEssence(a.Val) == "importmap"
				case name == "link" && a.Key == "href":
					out = append(out, a.Val)
				}
			}
		case html.TextToken:
			switch {
			case inStyle:
				for _, r := range scanCSS(z.Raw()) {
					out = append(out, r.url)
				}
			case inMap:
				// A map that does not parse names nothing a browser loads.
				entries, _, _ := importMapEntries(z.Raw())
				for _, e := range entries {
					out = append(out, e.address)
				}
			}
		case html.EndTagToken:
			inStyle, inMap = false, false
		}
	}
}

// Text is a page's readable text, at most limit bytes cut at a character
// boundary: what the gallery's search indexes. Scripts, styles and templates
// are not text.
func Text(text []byte, limit int) string {
	var b strings.Builder
	z := html.NewTokenizer(bytes.NewReader(text))
	skip := 0
	for b.Len() < limit {
		switch z.Next() {
		case html.ErrorToken:
			return truncate(b.String(), limit)
		case html.StartTagToken:
			if skipped(z.Token().Data) {
				skip++
			}
		case html.EndTagToken:
			if skipped(z.Token().Data) && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip > 0 {
				continue
			}
			for _, f := range strings.Fields(string(z.Text())) {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(f)
			}
		}
	}
	return truncate(b.String(), limit)
}

// skipped reports an element whose content is not readable text.
func skipped(tag string) bool {
	switch strings.ToLower(tag) {
	case "script", "style", "template", "noscript":
		return true
	}
	return false
}

// truncate cuts s to at most n bytes at a character boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
