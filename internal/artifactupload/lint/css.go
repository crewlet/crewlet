package lint

import (
	"bytes"
	"net/url"
	"path"
	"strings"
)

// cssRef is one reference a stylesheet makes: an @import or a url().
type cssRef struct {
	// start and end bound what a rewrite replaces: the string or the
	// unquoted text, quotes included.
	start, end int

	// url is the reference as written, unescaped.
	url  string
	kind RefKind
	line int
}

// fontExtensions are the file types a url() names as a font.
var fontExtensions = []string{".woff2", ".woff", ".ttf", ".otf", ".eot"}

// scanCSS finds every @import and url() in a stylesheet, skipping comments and
// every string that is not one of them, so `content: "url(x)"` is text rather
// than a reference.
func scanCSS(text []byte) []cssRef {
	var out []cssRef
	line := 1
	i := 0
	for i < len(text) {
		c := text[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '/' && i+1 < len(text) && text[i+1] == '*':
			end := bytes.Index(text[i+2:], []byte("*/"))
			if end < 0 {
				return out
			}
			line += bytes.Count(text[i:i+2+end], []byte("\n"))
			i += end + 4
		case c == '"' || c == '\'':
			end := stringEnd(text, i)
			line += bytes.Count(text[i:end], []byte("\n"))
			i = end
		case c == '@' && hasFold(text[i:], "@import"):
			j := skipCSSSpace(text, i+len("@import"))
			switch {
			case j < len(text) && (text[j] == '"' || text[j] == '\''):
				end := stringEnd(text, j)
				out = append(out, cssRef{start: j, end: end, url: unescapeCSS(text[j+1 : max(j+1, end-1)]),
					kind: RefStylesheet, line: line})
				i = end
			case hasFold(text[j:], "url("):
				ref, next := readURL(text, j)
				ref.kind, ref.line = RefStylesheet, line
				out = append(out, ref)
				i = next
			default:
				i = j
			}
		case (c == 'u' || c == 'U') && hasFold(text[i:], "url(") &&
			(i == 0 || !identChar(text[i-1])):
			ref, next := readURL(text, i)
			ref.kind, ref.line = RefAsset, line
			lower := strings.ToLower(strings.SplitN(ref.url, "?", 2)[0])
			for _, ext := range fontExtensions {
				if strings.HasSuffix(lower, ext) {
					ref.kind = RefFont
				}
			}
			out = append(out, ref)
			i = next
		default:
			i++
		}
	}
	return out
}

// readURL reads a url() starting at i, answering the reference and where
// scanning resumes.
func readURL(text []byte, i int) (cssRef, int) {
	j := skipCSSSpace(text, i+len("url("))
	if j < len(text) && (text[j] == '"' || text[j] == '\'') {
		end := stringEnd(text, j)
		next := skipCSSSpace(text, end)
		if next < len(text) && text[next] == ')' {
			next++
		}
		return cssRef{start: j, end: end, url: unescapeCSS(text[j+1 : max(j+1, end-1)])}, next
	}
	end := j
	for end < len(text) && text[end] != ')' {
		if text[end] == '\\' && end+1 < len(text) {
			end++
		}
		end++
	}
	raw := bytes.TrimRight(text[j:end], " \t\n\r\f")
	next := end
	if next < len(text) {
		next++
	}
	return cssRef{start: j, end: j + len(raw), url: unescapeCSS(raw)}, next
}

// stringEnd is the index just past the CSS string opened at i.
func stringEnd(text []byte, i int) int {
	q := text[i]
	j := i + 1
	for j < len(text) {
		switch text[j] {
		case '\\':
			j += 2
			continue
		case q:
			return j + 1
		case '\n':
			// An unterminated string ends at the line, as CSS reads it.
			return j
		}
		j++
	}
	return len(text)
}

func skipCSSSpace(text []byte, i int) int {
	for i < len(text) && strings.ContainsRune(" \t\n\r\f", rune(text[i])) {
		i++
	}
	return i
}

func hasFold(text []byte, prefix string) bool {
	return len(text) >= len(prefix) && strings.EqualFold(string(text[:len(prefix)]), prefix)
}

func identChar(c byte) bool {
	return c == '-' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// unescapeCSS removes CSS's backslash escapes of single characters, which is
// all a URL in a stylesheet carries in practice; a hex escape is decoded too.
func unescapeCSS(raw []byte) string {
	if !bytes.ContainsRune(raw, '\\') {
		return string(raw)
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 >= len(raw) {
			b.WriteByte(raw[i])
			continue
		}
		i++
		hex := 0
		for hex < 6 && i+hex < len(raw) && isHex(raw[i+hex]) {
			hex++
		}
		if hex == 0 {
			b.WriteByte(raw[i])
			continue
		}
		var r rune
		for _, c := range raw[i : i+hex] {
			r = r*16 + rune(hexValue(c))
		}
		b.WriteRune(r)
		i += hex - 1
		if i+1 < len(raw) && raw[i+1] == ' ' {
			i++
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexValue(c byte) int {
	switch {
	case c >= 'a':
		return int(c-'a') + 10
	case c >= 'A':
		return int(c-'A') + 10
	}
	return int(c - '0')
}

// cssString renders a value as a double-quoted CSS string.
func cssString(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\a `).Replace(v) + `"`
}

// CSS is a stylesheet with every reference to an allowlisted CDN file replaced
// by the path the version serves its copy at, and what was found.
//
// base is the URL the stylesheet itself was fetched from when it is a copied
// CDN file, so its relative references are the CDN's files and are copied in
// too; nil for an author's own stylesheet, whose relative references are the
// artifact's files and are left as written. A reference another origin serves
// and the allowlist does not admit is REFUSED where it is an @import (a
// stylesheet a page would depend on) and WARNED where it is a url() (an image
// or a font the policy will not load). From is the stylesheet's own path in
// the version.
func CSS(from string, text []byte, opts Options, base *url.URL) ([]byte, []Finding, error) {
	fs := &findings{path: from}
	out := cssRewrite(fs, from, text, opts, base, 0)
	if err := fs.err(); err != nil {
		return nil, nil, err
	}
	return out, fs.warnings, nil
}

// cssRewrite is [CSS] into fs, with lineBase added to every line it reports
// (a style element's lines are the page's).
func cssRewrite(fs *findings, from string, text []byte, opts Options, base *url.URL, lineBase int) []byte {
	refs := scanCSS(text)
	if len(refs) == 0 {
		return text
	}
	var b bytes.Buffer
	b.Grow(len(text))
	at := 0
	for _, r := range refs {
		line := lineBase + r.line
		if strings.HasPrefix(strings.ToLower(normalizeRef(r.url)), "data:") || r.url == "" {
			continue
		}
		var u *url.URL
		switch {
		case IsRemote(r.url):
			parsed, err := absolute(r.url)
			if err != nil {
				fs.add(line, refusingFor(r.kind), "%q is not a URL a stylesheet can load: %v", r.url, err)
				continue
			}
			u = parsed
		case base != nil:
			rel, err := url.Parse(normalizeRef(r.url))
			if err != nil {
				fs.add(line, CodeVendorFailed, "%q in a copied stylesheet is not a URL: %v", r.url, err)
				continue
			}
			u = base.ResolveReference(rel)
		default:
			continue
		}
		local, ok := vendorRef(fs, opts, Ref{URL: u, Kind: r.kind, From: from, Line: line})
		if !ok {
			continue
		}
		b.Write(text[at:r.start])
		b.WriteString(cssString(local))
		at = r.end
	}
	b.Write(text[at:])
	return b.Bytes()
}

// refusingFor is the code a stylesheet reference that cannot be read is
// reported under: refused for an @import, warned for a url().
func refusingFor(k RefKind) Code {
	if k == RefStylesheet {
		return CodeRemoteStylesheet
	}
	return CodeRemoteBlocked
}

// vendorRef asks the allowlist and then the resolver about one remote
// reference, recording what either refused, and answers the local path the
// version serves the copy at.
func vendorRef(fs *findings, opts Options, ref Ref) (string, bool) {
	if err := opts.Hosts.Check(ref.URL, ref.Kind); err != nil {
		var refused *RefusedRef
		code := CodeRemoteBlocked
		if asRefused(err, &refused) {
			code = refused.Code
		}
		fs.add(ref.Line, code, "%v", err)
		return "", false
	}
	if opts.Resolve == nil {
		fs.add(ref.Line, CodeVendorOff, "%s is an allowlisted CDN file, and this engine copies "+
			"no CDN file into an artifact; upload the file into the artifact instead",
			ref.URL.Redacted())
		return "", false
	}
	local, err := opts.Resolve(ref)
	if err != nil {
		fs.add(ref.Line, CodeVendorFailed, "%s could not be copied into the artifact: %v",
			ref.URL.Redacted(), err)
		return "", false
	}
	return local, true
}

// LocalRefs is every reference a stylesheet or a page the linter already wrote
// makes to a file of the version, resolved against from's directory: what a
// carried file needs to find in the version beside it. A reference that leaves
// the version's root is not one.
func LocalRefs(from string, text []byte, isHTML bool) []string {
	var raws []string
	if isHTML {
		raws = htmlLocalRefs(text)
	} else {
		for _, r := range scanCSS(text) {
			raws = append(raws, r.url)
		}
	}
	dir := path.Dir(from)
	seen := map[string]bool{}
	var out []string
	for _, raw := range raws {
		n := normalizeRef(raw)
		if n == "" || IsRemote(n) || strings.HasPrefix(n, "/") || strings.HasPrefix(n, "#") {
			continue
		}
		n, _, _ = strings.Cut(n, "#")
		n, _, _ = strings.Cut(n, "?")
		if unescaped, err := url.PathUnescape(n); err == nil {
			n = unescaped
		}
		p := path.Clean(path.Join(dir, n))
		if p == "." || p == ".." || strings.HasPrefix(p, "../") || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
