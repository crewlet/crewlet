package lint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// AN IMPORT MAP IS A LIST OF MODULE SOURCES. A `<script type="importmap">`
// tells the browser where each bare specifier a module imports is fetched
// from, and a nonced module's imports load under its nonce from wherever the
// map points. So an entry naming another origin is the same reference as a
// literal remote import, written where the import scan never looks: left as
// written, a viewer's browser fetches the module from that origin at view
// time, outside the copy-in and its integrity check. Every remote entry is
// therefore held to the rule a module script's source is: an allowlisted CDN
// file at an exact version, copied into the version and the entry rewritten
// to the copy, or the page is refused.

// mapEntry is one module address an import map names: the byte range of its
// JSON string literal in the map's text, and the address it decodes to.
type mapEntry struct {
	start, end int
	specifier  string
	address    string
}

// errImportMap is an import map this reading could not take apart. A browser
// ignores a map it cannot parse, so refusing it loses nothing a page relied
// on, and publishing a map this reading disagrees with the browser about
// would be the escape the reading exists to close.
var errImportMap = errors.New("not an import map a browser reads")

// importMapEntries is every module address in an import map's `imports` and
// in each of its `scopes`, each with the range of its literal, and the map's
// `integrity` table (a URL to its subresource integrity value).
//
// Read by the decoder's own token stream rather than unmarshalled into a map,
// so each address keeps its position and a rewrite replaces exactly that
// literal, leaving the rest of the author's text as written.
func importMapEntries(text []byte) ([]mapEntry, map[string]string, error) {
	if !json.Valid(text) {
		return nil, nil, fmt.Errorf("%w: the text is not JSON", errImportMap)
	}
	dec := json.NewDecoder(bytes.NewReader(text))
	if err := expectDelim(dec, '{'); err != nil {
		return nil, nil, fmt.Errorf("%w: an import map is a JSON object", errImportMap)
	}
	var (
		entries   []mapEntry
		integrity = map[string]string{}
	)
	for dec.More() {
		key, err := objectKey(dec)
		if err != nil {
			return nil, nil, err
		}
		switch key {
		case "imports":
			found, err := specifierMap(dec, text)
			if err != nil {
				return nil, nil, fmt.Errorf("imports: %w", err)
			}
			entries = append(entries, found...)
		case "scopes":
			if err := expectDelim(dec, '{'); err != nil {
				return nil, nil, fmt.Errorf("%w: scopes is an object of specifier maps", errImportMap)
			}
			for dec.More() {
				scope, err := objectKey(dec)
				if err != nil {
					return nil, nil, err
				}
				found, err := specifierMap(dec, text)
				if err != nil {
					return nil, nil, fmt.Errorf("scopes[%q]: %w", scope, err)
				}
				entries = append(entries, found...)
			}
			if err := expectDelim(dec, '}'); err != nil {
				return nil, nil, fmt.Errorf("%w: scopes", errImportMap)
			}
		case "integrity":
			// A table of URL to digest. One that is not a table of strings is
			// ignored by a browser, and is here: it only feeds the copy's
			// integrity check.
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, nil, fmt.Errorf("%w: %w", errImportMap, err)
			}
			var table map[string]string
			if json.Unmarshal(raw, &table) == nil {
				integrity = table
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, nil, fmt.Errorf("%w: %w", errImportMap, err)
			}
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, nil, fmt.Errorf("%w: the object does not close", errImportMap)
	}
	return entries, integrity, nil
}

// specifierMap reads one object of specifier to address, answering every
// address that is a string. An address that is not one is ignored by a
// browser (it maps nothing), so it is not an entry.
func specifierMap(dec *json.Decoder, text []byte) ([]mapEntry, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return nil, fmt.Errorf("%w: a specifier map is a JSON object", errImportMap)
	}
	var out []mapEntry
	for dec.More() {
		specifier, err := objectKey(dec)
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("%w: %w", errImportMap, err)
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		if start < 0 || end > len(text) || !bytes.Equal(text[start:end], raw) {
			return nil, fmt.Errorf("%w: the address of %q could not be located", errImportMap, specifier)
		}
		var address string
		if json.Unmarshal(raw, &address) != nil {
			continue
		}
		out = append(out, mapEntry{start: start, end: end, specifier: specifier, address: address})
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, fmt.Errorf("%w: a specifier map does not close", errImportMap)
	}
	return out, nil
}

// expectDelim reads one delimiter token, refusing anything else.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("want %q, have %v", want, tok)
	}
	return nil
}

// objectKey reads one object key.
func objectKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("%w: %w", errImportMap, err)
	}
	key, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("%w: an object key is %v", errImportMap, tok)
	}
	return key, nil
}

// importMapText lints one import map's text and answers it as stored: every
// remote address copied in and rewritten to the copy, or refused.
func (l *linter) importMapText(text []byte, line int) []byte {
	entries, integrity, err := importMapEntries(text)
	if err != nil {
		l.fs.add(line, CodeUnparseable, "the import map could not be read (%v); a browser "+
			"ignores a map it cannot parse, so fix the JSON or remove the map", err)
		return text
	}
	var (
		out bytes.Buffer
		at  int
	)
	for _, e := range entries {
		if !IsRemote(e.address) {
			continue
		}
		entryLine := line + bytes.Count(text[:e.start], []byte("\n"))
		local, ok := l.importMapAddress(e, integrity[e.address], entryLine)
		if !ok {
			continue
		}
		encoded, err := json.Marshal(local)
		if err != nil {
			l.fs.add(entryLine, CodeUnparseable, "the import map entry %q could not be rewritten: %v",
				e.specifier, err)
			continue
		}
		out.Write(text[at:e.start])
		out.Write(encoded)
		at = e.end
	}
	if at == 0 {
		return text
	}
	out.Write(text[at:])
	return out.Bytes()
}

// importMapAddress holds one remote import map address to the rule a module
// script's source is held to, answering the address of the version's copy.
func (l *linter) importMapAddress(e mapEntry, integrity string, line int) (string, bool) {
	refuse := func(format string, args ...any) (string, bool) {
		l.fs.add(line, CodeRemoteImport, "the import map maps %q to %s; "+format+". Load "+
			"the module from an allowlisted CDN at an exact version, or upload it into the "+
			"artifact", append([]any{e.specifier, e.address}, args...)...)
		return "", false
	}
	if s := scheme(normalizeRef(e.address)); s != "" && s != "https" && s != "http" {
		return refuse("a %s: address is not a file the version can hold", s)
	}
	if strings.HasSuffix(strings.SplitN(e.address, "?", 2)[0], "/") {
		return refuse("a folder on another origin cannot be copied in, so map each module " +
			"to its own file")
	}
	u, err := absolute(e.address)
	if err != nil {
		return refuse("which is not a URL a page can load (%v)", err)
	}
	if err := l.opts.Hosts.Check(u, RefModule); err != nil {
		var refused *RefusedRef
		if asRefused(err, &refused) && refused.Code != CodeRemoteScript {
			l.fs.add(line, refused.Code, "the import map maps %q to %v", e.specifier, err)
			return "", false
		}
		return refuse("%v", err)
	}
	local, ok := vendorRef(l.fs, l.opts, Ref{URL: u, Kind: RefModule, Integrity: integrity,
		From: l.path, Line: line})
	if !ok {
		return "", false
	}
	return moduleAddress(local), true
}

// moduleAddress is a version path written as an import map address: a
// browser reads an address as a URL only when it is absolute or starts with
// `/`, `./` or `../`, and maps nothing to a bare one.
func moduleAddress(local string) string {
	if strings.HasPrefix(local, "/") || strings.HasPrefix(local, "./") ||
		strings.HasPrefix(local, "../") {
		return local
	}
	return "./" + local
}
