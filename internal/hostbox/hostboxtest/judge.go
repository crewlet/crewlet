package hostboxtest

import (
	"fmt"
	"maps"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// announcement is one line a writer sends the matrix.
type announcement struct {
	Kind string `json:"kind"`
	// Index, Step, Name and Stopped are a step's: its place among the
	// script's steps from 1, what the write reported, the file it is about
	// ("" for the whole login), and whether the writer parked there.
	Index   int                    `json:"index,omitempty"`
	Step    hostbox.CredentialStep `json:"step,omitempty"`
	Name    string                 `json:"name,omitempty"`
	Stopped bool                   `json:"stopped,omitempty"`
}

// The kinds of announcement, in the order a run makes them: ready once, a
// step for each step the script reaches, done once.
const (
	kindReady = "ready"
	kindStep  = "step"
	kindDone  = "done"
)

// Stop is one step a writer's script reaches, as the run to its end reported
// it and the replay of the script's logins placed it.
type Stop struct {
	// Index is its place among the script's steps, from 1.
	Index int
	// Write is the script's write it is a step of, from 1.
	Write int
	// Step is what the write reported.
	Step hostbox.CredentialStep
	// Name is the file it is about, "" for a step about the whole login.
	Name string

	// files are the login files the steps so far leave, by name, over the
	// script's Names.
	files map[string]string
	// standing is whether a journal stands once the step has happened.
	standing bool
}

// String names the stop in a failure: its place, its write, and its step.
func (s Stop) String() string {
	if s.Name == "" {
		return fmt.Sprintf("step %d (write %d, %s)", s.Index, s.Write, s.Step)
	}
	return fmt.Sprintf("step %d (write %d, %s %s)", s.Index, s.Write, s.Step, s.Name)
}

// number turns the announcements of a run to its end into the script's stops,
// replaying each step over the script's logins, and refuses, naming the step,
// a run that does not fit its script.
func number(script Script, run []announcement) ([]Stop, error) {
	if len(run) < 2 || run[0].Kind != kindReady || run[len(run)-1].Kind != kindDone {
		return nil, fmt.Errorf("the run announced %v, which is not ready, its steps, then done", kinds(run))
	}
	steps := run[1 : len(run)-1]
	writes := 0
	for i, a := range steps {
		if a.Kind != kindStep {
			return nil, fmt.Errorf("the run announced %q among its steps, as its announcement %d", a.Kind, i+2)
		}
		if a.Step == hostbox.StepCommitted {
			writes++
		}
	}
	if writes != len(script.Logins)-1 {
		return nil, fmt.Errorf("the script made %d writes of the shared login, where its %d logins of "+
			"record take %d — a writer that makes more has split one login over two, and one that makes "+
			"fewer wrote a login outside the journal, which reaches no step",
			writes, len(script.Logins), len(script.Logins)-1)
	}
	names := map[string]bool{}
	for _, name := range script.Names {
		names[name] = true
	}
	files := within(script.Logins[0], names)
	standing, write := false, 0
	stops := make([]Stop, 0, len(steps))
	for i, a := range steps {
		at := Stop{Index: a.Index, Write: write, Step: a.Step, Name: a.Name}
		if a.Step == hostbox.StepCommitted {
			at.Write = write + 1
		}
		switch {
		case a.Index != i+1:
			return nil, fmt.Errorf("%v came as step %d of the run", at, i+1)
		case !a.Step.Valid():
			return nil, fmt.Errorf("%v is no step a login write reports", at)
		case a.Stopped:
			return nil, fmt.Errorf("%v was announced as stopped in a run told to stop nowhere", at)
		case a.Step != hostbox.StepCommitted && !standing:
			if write == 0 {
				return nil, fmt.Errorf("%v came before the script's first commit: a writer's setup "+
					"ends before it calls Ready", at)
			}
			return nil, fmt.Errorf("%v came outside a write: no journal stands there", at)
		}
		login := script.Logins[at.Write]
		switch a.Step {
		case hostbox.StepCommitted:
			if standing {
				return nil, fmt.Errorf("%v committed while write %d's journal still stood: each write "+
					"finishes before the next begins", at, write)
			}
			write++
			standing = true
		case hostbox.StepWritten:
			if names[a.Name] {
				content, ok := login[a.Name]
				if !ok {
					return nil, fmt.Errorf("%v wrote a file that write %d's login does not hold", at, write)
				}
				files[a.Name] = content
			}
		case hostbox.StepRemoved:
			if names[a.Name] {
				if _, ok := login[a.Name]; ok {
					return nil, fmt.Errorf("%v removed a file that write %d's login holds", at, write)
				}
				delete(files, a.Name)
			}
		case hostbox.StepClosed:
			standing = false
			if want := within(login, names); !maps.Equal(files, want) {
				return nil, fmt.Errorf("%v closed a write whose steps leave %v, not its login %v",
					at, files, want)
			}
		}
		at.files, at.standing = maps.Clone(files), standing
		stops = append(stops, at)
	}
	if standing {
		return nil, fmt.Errorf("the run ended inside write %d, its journal standing", write)
	}
	return stops, nil
}

// judge names every way what a writer killed at stop left falls short of what
// the stop says it leaves: files, the login files on disk, and standing,
// whether a journal stands; then read, what the next reader read, and
// standingAfter, whether a journal stood after it.
func judge(script Script, stop Stop, files map[string]string, standing bool, read map[string]string,
	standingAfter bool,
) []string {
	var problems []string
	if !maps.Equal(files, stop.files) {
		problems = append(problems, fmt.Sprintf("killed at %v, the writer left the login files %v, "+
			"where the steps so far leave %v", stop, files, stop.files))
	}
	if standing != stop.standing {
		problems = append(problems, fmt.Sprintf("killed at %v, a journal stands: %t, where the steps "+
			"so far leave one standing: %t", stop, standing, stop.standing))
	}
	if want := script.Logins[stop.Write]; !maps.Equal(read, want) {
		problems = append(problems, fmt.Sprintf("killed at %v, the next reader read %v, where the "+
			"write it interrupted committed %v", stop, read, want))
	}
	if standingAfter {
		problems = append(problems, fmt.Sprintf("killed at %v, a journal still stood after the next "+
			"reader read: the reader did not finish the write", stop))
	}
	return problems
}

// within is login's files among names.
func within(login map[string]string, names map[string]bool) map[string]string {
	out := map[string]string{}
	for name, content := range login {
		if names[name] {
			out[name] = content
		}
	}
	return out
}

// kinds is the kinds of a run's announcements, for a failure that names them.
func kinds(run []announcement) []string {
	out := make([]string, 0, len(run))
	for _, a := range run {
		out = append(out, a.Kind)
	}
	return out
}
