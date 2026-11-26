package config

import (
	"path/filepath"
	"strings"
	"time"
)

// ---- dictation -------------------------------------------------------------- //

// Dictation is this node's voice dictation: the local recognizer
// internal/speech runs for /ws/dictation. Tier A because what it sizes is this
// node's CPU and memory and where its files live; every field is optional and
// the zero block is dictation on, with no setup.
type Dictation struct {
	// Enabled switches dictation on this node. Unset is on.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this node serves voice dictation from its local recognizer. Unset = true: no key, vendor or download step is needed, and audio never leaves the engine."`

	// RecognizerDir is a prepared recognizer, for a host with no route to
	// its release URLs.
	RecognizerDir string `yaml:"recognizer_dir,omitempty" json:"recognizer_dir,omitempty" js:"maxlen=4096" desc:"Directory holding the dictation recognizer (its library for this platform and its model), as crewlet speech prepare -dir writes it; read, never written. Relative paths are resolved against store.path's directory. Empty uses the recognizer baked into the container image, else downloads it once: the library into the native-library cache, the model into the data directory."`

	// MaxConcurrent caps live dictations on this node.
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty" js:"min=0;max=1024" desc:"Most dictations this node transcribes at once; past it a person is told busy. 0 = 4 per available core (a dictation uses about a fifth of a core)."`

	// SessionSeconds is the longest dictation.
	SessionSeconds int `yaml:"session_seconds,omitempty" json:"session_seconds,omitempty" js:"min=0;max=3600" desc:"Longest single dictation, in seconds of time and of audio. 0 = 300. At most 3600."`

	// IdleSeconds ends a dictation that sent no audio for this long.
	IdleSeconds int `yaml:"idle_seconds,omitempty" json:"idle_seconds,omitempty" js:"min=0;max=300" desc:"Seconds without audio after which a dictation ends. 0 = 15. At most 300, and no more than session_seconds."`
}

// The defaults of an unset [Dictation] field, and their bounds.
const (
	// DefaultDictationSessionSeconds is five minutes: a long spoken message,
	// and a bound on what one person holds of the recognizer.
	DefaultDictationSessionSeconds = 300
	// DefaultDictationIdleSeconds ends a dictation left open by a person
	// who walked away, well past any pause in speech.
	DefaultDictationIdleSeconds = 15

	maxDictationSessionSeconds = 3600
	maxDictationIdleSeconds    = 300
	maxDictationConcurrent     = 1024
)

// IsZero lets an unset block drop out of a JSON round trip.
func (d Dictation) IsZero() bool {
	return d.Enabled == nil && d.RecognizerDir == "" && d.MaxConcurrent == 0 &&
		d.SessionSeconds == 0 && d.IdleSeconds == 0
}

// On reports whether this node serves dictation.
func (d Dictation) On() bool { return d.Enabled == nil || *d.Enabled }

// Session is the longest dictation.
func (d Dictation) Session() time.Duration {
	if d.SessionSeconds == 0 {
		return DefaultDictationSessionSeconds * time.Second
	}
	return time.Duration(d.SessionSeconds) * time.Second
}

// Idle is how long a dictation may send no audio.
func (d Dictation) Idle() time.Duration {
	if d.IdleSeconds == 0 {
		return DefaultDictationIdleSeconds * time.Second
	}
	return time.Duration(d.IdleSeconds) * time.Second
}

// RecognizerDirFor is [Dictation.RecognizerDir] with a relative value
// resolved against the store's directory, as [Store.SnapshotDirFor] resolves
// its own; "" when unset.
func (d Dictation) RecognizerDirFor(store Store) string {
	dir := strings.TrimSpace(d.RecognizerDir)
	switch {
	case dir == "":
		return ""
	case filepath.IsAbs(dir):
		return filepath.Clean(dir)
	}
	return filepath.Join(filepath.Dir(store.Path), dir)
}

func (d *Dictation) validate(path Path) error {
	var p problems
	if d.MaxConcurrent < 0 || d.MaxConcurrent > maxDictationConcurrent {
		p.add(at(path, "max_concurrent"), ErrOutOfRange,
			"must be 0 (4 per available core) or 1..%d, got %d", maxDictationConcurrent, d.MaxConcurrent)
	}
	if d.SessionSeconds < 0 || d.SessionSeconds > maxDictationSessionSeconds {
		p.add(at(path, "session_seconds"), ErrOutOfRange,
			"must be 0 (%d) or 1..%d, got %d", DefaultDictationSessionSeconds,
			maxDictationSessionSeconds, d.SessionSeconds)
	}
	if d.IdleSeconds < 0 || d.IdleSeconds > maxDictationIdleSeconds {
		p.add(at(path, "idle_seconds"), ErrOutOfRange,
			"must be 0 (%d) or 1..%d, got %d", DefaultDictationIdleSeconds,
			maxDictationIdleSeconds, d.IdleSeconds)
	} else if d.Idle() > d.Session() {
		p.add(at(path, "idle_seconds"), ErrConflict,
			"is %s, longer than session_seconds (%s): a dictation would end at its "+
				"length before it could ever be idle that long. Lower idle_seconds or "+
				"raise session_seconds", d.Idle(), d.Session())
	}
	if len(d.RecognizerDir) > 4096 {
		p.add(at(path, "recognizer_dir"), ErrOutOfRange, "is %d characters, longer than 4096", len(d.RecognizerDir))
	}
	return p.err()
}
