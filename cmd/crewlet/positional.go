package main

import "flag"

// How a command reads its positional arguments.
//
// # Why a value can arrive from two places
//
// Go's flag package stops parsing at the FIRST non-flag token, so `crewlet run
// /etc/crewlet.yaml -debug` parses no flags and would boot from ./crewlet.yaml.
// Every command therefore peels a LEADING positional with [splitSubject]
// before parsing, and the value may also arrive TRAILING; both are accepted.
//
// # Why the count, rather than an ok
//
// `len(tail)+1` reports THREE for `config import a.yaml b.yaml`: done once, here.

// onePositional resolves the single value a command takes, whether it arrived
// leading or trailing, and reports how many were given in total.
//
// leading is what [splitSubject] already peeled — "" when there was none.
//
// A command that takes the value OPTIONALLY refuses `given > 1`; one that
// REQUIRES it refuses `given != 1`. That distinction is the command's, not
// this function's: "at most one config document" and "exactly one company
// document" are different contracts and each says so in its own usage line.
func onePositional(fs *flag.FlagSet, leading string) (value string, given int) {
	tail := fs.Args()
	given = len(tail)
	if leading != "" {
		return leading, given + 1
	}
	if given == 1 {
		return tail[0], 1
	}
	// Either nothing was given or too much was; the caller judges which,
	// and there is no single value to hand back for the second.
	return "", given
}

// twoPositionals is [onePositional] for a command taking an ordered PAIR,
// each of which may arrive leading or trailing.
//
// The trailing arguments fill the empty slots IN ORDER, so
// `crewlet confluence import company.yaml ./docs` and
// `crewlet confluence import -space ENG company.yaml ./docs` both land the
// same way. given is the total, so a caller refuses `given != 2` and gets the
// count right in its message.
func twoPositionals(fs *flag.FlagSet, first, second string) (a, b string, given int) {
	tail := fs.Args()
	given = len(tail)
	if first != "" {
		given++
	}
	if second != "" {
		given++
	}
	if first == "" && len(tail) > 0 {
		first, tail = tail[0], tail[1:]
	}
	if second == "" && len(tail) > 0 {
		second = tail[0]
	}
	return first, second, given
}
