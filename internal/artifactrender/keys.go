package artifactrender

import (
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/browser/cdp"
)

// THE KEYS a press plays: a named key or one character, after any modifiers,
// sent as a keyboard sends them, so a page's own key handlers and the
// browser's defaults (Tab moving the focus, Enter activating a button) run.

// namedKey is one non-character key: its DOM code, its Windows virtual key
// code and the text it types, if any.
type namedKey struct {
	code string
	vk   int64
	text string
}

var namedKeys = map[string]namedKey{
	"Enter":      {"Enter", 13, "\r"},
	"Tab":        {"Tab", 9, ""},
	"Escape":     {"Escape", 27, ""},
	"Backspace":  {"Backspace", 8, ""},
	"Delete":     {"Delete", 46, ""},
	"Space":      {"Space", 32, " "},
	"ArrowUp":    {"ArrowUp", 38, ""},
	"ArrowDown":  {"ArrowDown", 40, ""},
	"ArrowLeft":  {"ArrowLeft", 37, ""},
	"ArrowRight": {"ArrowRight", 39, ""},
	"Home":       {"Home", 36, ""},
	"End":        {"End", 35, ""},
	"PageUp":     {"PageUp", 33, ""},
	"PageDown":   {"PageDown", 34, ""},
}

// modifierBits are the protocol's modifier flags.
var modifierBits = map[string]int64{"Alt": 1, "Control": 2, "Meta": 4, "Shift": 8}

// parseKey splits a key spec into its modifiers and its key, false for one a
// press does not play.
func parseKey(spec string) (mods int64, key string, ok bool) {
	parts := strings.Split(spec, "+")
	// A PLUS IS A KEY TOO: `Shift++` and `+` end in an empty part.
	if strings.HasSuffix(spec, "+") {
		parts = append(parts[:len(parts)-2], "+")
	}
	key = parts[len(parts)-1]
	for _, m := range parts[:len(parts)-1] {
		bit, ok := modifierBits[m]
		if !ok {
			return 0, "", false
		}
		mods |= bit
	}
	if _, named := namedKeys[key]; named {
		return mods, key, true
	}
	return mods, key, utf8.RuneCountInString(key) == 1 && key != " "
}

// validKey reports whether a press plays spec.
func validKey(spec string) bool {
	_, _, ok := parseKey(spec)
	return ok
}

// keyEvents are the events a press of spec sends: down, then up.
func keyEvents(spec string) []cdp.InputDispatchKeyEventParams {
	mods, key, _ := parseKey(spec)
	typing := mods&(modifierBits["Alt"]|modifierBits["Control"]|modifierBits["Meta"]) == 0
	down := cdp.InputDispatchKeyEventParams{Type: "keyDown", Modifiers: mods, Key: key}
	if k, ok := namedKeys[key]; ok {
		down.Code, down.WindowsVirtualKeyCode = k.code, k.vk
		if key == "Space" {
			down.Key = " "
		}
		if typing && k.text != "" {
			down.Text, down.UnmodifiedText = k.text, k.text
		}
	} else {
		r, _ := utf8.DecodeRuneInString(key)
		upper := strings.ToUpper(key)
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			down.Code, down.WindowsVirtualKeyCode = "Key"+upper, int64(upper[0])
		case r >= '0' && r <= '9':
			down.Code, down.WindowsVirtualKeyCode = "Digit"+key, int64(key[0])
		}
		if typing {
			down.Text, down.UnmodifiedText = key, key
		}
	}
	if down.Text == "" {
		down.Type = "rawKeyDown"
	}
	up := down
	up.Type, up.Text, up.UnmodifiedText = "keyUp", "", ""
	return []cdp.InputDispatchKeyEventParams{down, up}
}
