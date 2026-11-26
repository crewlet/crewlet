package config

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// THE ZERO BLOCK IS DICTATION ON, WITH THE DEFAULTS: no setup is the product.
func TestAnUnsetDictationBlockIsOnWithItsDefaults(t *testing.T) {
	var d Dictation
	if !d.On() || d.Session() != 5*time.Minute || d.Idle() != 15*time.Second || d.RecognizerDirFor(Store{}) != "" {
		t.Errorf("the zero block is on %v, session %s, idle %s, recognizer dir %q",
			d.On(), d.Session(), d.Idle(), d.RecognizerDirFor(Store{}))
	}
	if err := d.validate(field("dictation")); err != nil {
		t.Errorf("the zero block is refused: %v", err)
	}
	off := false
	if (Dictation{Enabled: &off}).On() {
		t.Error("enabled: false still serves dictation")
	}
}

// OUT-OF-RANGE LIMITS ARE REFUSED NAMING THE FIELD, and an idle cut longer
// than the session (which could never fire) is a conflict.
func TestDictationLimitsAreBounded(t *testing.T) {
	for name, c := range map[string]struct {
		d     Dictation
		field string
		want  error
	}{
		"negative cap":     {Dictation{MaxConcurrent: -1}, "dictation.max_concurrent", ErrOutOfRange},
		"cap too high":     {Dictation{MaxConcurrent: 1025}, "dictation.max_concurrent", ErrOutOfRange},
		"session too long": {Dictation{SessionSeconds: 3601}, "dictation.session_seconds", ErrOutOfRange},
		"negative idle":    {Dictation{IdleSeconds: -1}, "dictation.idle_seconds", ErrOutOfRange},
		"idle too long":    {Dictation{IdleSeconds: 301}, "dictation.idle_seconds", ErrOutOfRange},
		"idle past length": {Dictation{SessionSeconds: 10, IdleSeconds: 20}, "dictation.idle_seconds", ErrConflict},
	} {
		err := c.d.validate(field("dictation"))
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
			continue
		}
		if got := Problems(err); len(got) != 1 || got[0].Path != c.field {
			t.Errorf("%s: %v does not name %s alone", name, err, c.field)
		}
	}
	ok := Dictation{MaxConcurrent: 8, SessionSeconds: 600, IdleSeconds: 30}
	if err := ok.validate(field("dictation")); err != nil {
		t.Errorf("valid limits refused: %v", err)
	}
}

// A RELATIVE RECOGNIZER DIRECTORY IS THE STORE'S, as the snapshots' is: the same
// file applied to a container and a shell names one directory.
func TestARelativeRecognizerDirIsResolvedAgainstTheStore(t *testing.T) {
	store := Store{Path: filepath.Join("/var", "lib", "crewlet", "crewlet.db")}
	if got := (Dictation{RecognizerDir: "models"}).RecognizerDirFor(store); got != filepath.Join("/var", "lib", "crewlet", "models") {
		t.Errorf("relative recognizer_dir resolved to %s", got)
	}
	if got := (Dictation{RecognizerDir: "/opt/speech/"}).RecognizerDirFor(store); got != "/opt/speech" {
		t.Errorf("absolute recognizer_dir resolved to %s", got)
	}
}
