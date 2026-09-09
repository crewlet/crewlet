package config

import (
	"errors"
	"strings"
	"testing"
)

// THE FIVE RETIRED HORIZONS NAME WHAT REPLACED THEM, and two of them say
// plainly that the replacement is not the same thing.
//
// Every one of these shipped in a plan, a draft config or a doc, so an
// operator has them written down. "check the spelling" would send somebody to
// look for a typo in a key they were told to write, and for two of them the
// only true answer is that the feature does not exist: nothing on the native
// tracker deletes a removed item, because the removal IS the record.
//
// The other two point at `stream.tracker_retention.min_age` and then refuse
// to let it be read as a horizon — it bounds the log's replay window and
// deletes no history — because an operator who set `change_compaction_days:
// 90` and reads "use min_age instead" would set `min_age: 90d` believing
// their history is being trimmed, and it is not.
func TestTheRetiredTrackerHorizonsNameWhatReplacedThem(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc  string
		says []string
	}{
		"trash_retention_days": {
			"name: Acme\ntracker:\n  native:\n    trash_retention_days: 30\n",
			[]string{"NO horizon at all", "Delete the line"},
		},
		"trash_compaction_days": {
			"name: Acme\ntracker:\n  native:\n    trash_compaction_days: 30\n",
			[]string{"the removal IS the record"},
		},
		"change_compaction_days": {
			"name: Acme\ntracker:\n  native:\n    change_compaction_days: 90\n",
			[]string{"stream.tracker_retention.min_age", "NOT the same thing",
				"The history is kept"},
		},
		"turn_compaction_days": {
			"name: Acme\ntracker:\n  native:\n    turn_compaction_days: 90\n",
			[]string{"stream.tracker_retention.min_age", "deletes no history"},
		},
		"a whole retention block": {
			"name: Acme\ntracker:\n  retention:\n    min_age: 7d\n",
			[]string{"stream.tracker_retention", "OPERATOR's estate"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseCompany([]byte(tc.doc))
			if err == nil {
				t.Fatal("a retired key was accepted")
			}
			if !errors.Is(err, ErrUnknownField) {
				t.Errorf("want %v, got %v", ErrUnknownField, err)
			}
			if strings.Contains(err.Error(), "check the spelling") {
				t.Errorf("a retired key was reported as a misspelling: %v", err)
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}
}

// AND THE RETIRED SNAPSHOT CEILING NAMES A DIFFERENT KIND OF ANSWER: the
// setting did not move, the mechanism did. Snapshots are files on a disk now
// rather than objects in the broker, so what bounds them is where they are
// kept.
func TestTheRetiredSnapshotCeilingNamesTheDirectory(t *testing.T) {
	t.Parallel()
	_, err := ParseBootstrap([]byte("stream:\n  tracker_snapshot_max_bytes: 1073741824\n"), EnvOnly())
	if err == nil {
		t.Fatal("the retired ceiling was accepted")
	}
	if !errors.Is(err, ErrUnknownField) {
		t.Errorf("want %v, got %v", ErrUnknownField, err)
	}
	for _, want := range []string{"store.snapshot_dir", "files on this node's disk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}
