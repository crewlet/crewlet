package config

import (
	"encoding/csv"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// hostNetworkSetting is a setting that joins a container run to its host's
// network, as [hostNetwork] found it.
type hostNetworkSetting struct {
	// Arg is the index in run_args the setting starts at, or -1 for the
	// network setting.
	Arg int
	// Written is the setting as written: the network setting's value, or the
	// flag and its value, joined by a space where they are two arguments.
	Written string
}

// String names the setting where it is written: "network: host", or
// "run_args[3]: --network host".
func (s hostNetworkSetting) String() string {
	if s.Arg < 0 {
		return "network: " + s.Written
	}
	return fmt.Sprintf("run_args[%d]: %s", s.Arg, s.Written)
}

// hostNetwork is the first setting that joins a container run to its host's
// network or lets it reach the host's loopback, read in the order the runtime
// is handed them: network, passed as --network, then run_args. Nil for none.
//
// Every --network and --net is read, not only the last: a runtime attaches
// the run to each, and the first is its mode. So are Podman's --pod and
// --pod-id-file, which join an existing pod's network. A value joins the host
// as host; as container:<id> or Podman's ns:<path>, another's network, which
// may be the host's; by its name in Docker's extended form; or as slirp4netns
// or pasta reaching the host's loopback. Each argument is read as a flag
// even where a flag before it takes it as its value, as that flag may be a
// value itself (-e --network --net=host): a find too many, never too few.
func hostNetwork(network string, runArgs []string) *hostNetworkSetting {
	if joinsHost(network) {
		return &hostNetworkSetting{Arg: -1, Written: network}
	}
	for i, arg := range runArgs {
		flag, value, inline := strings.Cut(arg, "=")
		joins := networkFlag(flag)
		if joins == nil {
			continue
		}
		written := arg
		if !inline {
			// The value is the next argument. A last flag has none in
			// run_args: the runtime takes the image, which follows, for it.
			if i+1 == len(runArgs) {
				break
			}
			value = runArgs[i+1]
			written += " " + value
		}
		if joins(value) {
			return &hostNetworkSetting{Arg: i, Written: written}
		}
	}
	return nil
}

// networkFlag is the test a run argument's value takes when the argument
// sets the run's network; nil when it does not.
func networkFlag(flag string) func(value string) bool {
	switch flag {
	case "--network", "--net":
		return joinsHost
	case "--pod":
		return joinsPod
	case "--pod-id-file":
		return namesPod
	}
	return nil
}

// joinsPod reports whether a --pod value names an existing pod, whose network
// may be the host's; new:<name> makes one for this run.
func joinsPod(pod string) bool {
	return pod != "" && !strings.HasPrefix(pod, "new:")
}

// namesPod reports whether a --pod-id-file value names a file, which holds
// an existing pod's id.
func namesPod(file string) bool {
	return file != ""
}

// joinsHost reports whether a --network value joins the host's network as
// either runtime reads it: host; container:<id>, or Podman's ns:<path>,
// whose network may be the host's; slirp4netns allowing the host's loopback
// (Podman 5's, which Podman 6 refuses); pasta forwarding to it; or a network
// named host or container:<id> in Docker's extended form.
func joinsHost(value string) bool {
	if value == "host" || strings.HasPrefix(value, "container:") || strings.HasPrefix(value, "ns:") {
		return true
	}
	if options, ok := strings.CutPrefix(value, "slirp4netns:"); ok {
		return slirpReachesHost(strings.Split(options, ","))
	}
	if options, ok := strings.CutPrefix(value, "pasta:"); ok {
		return pastaReachesHost(strings.Split(options, ","))
	}
	name := extendedName(value)
	return name == "host" || strings.HasPrefix(name, "container:")
}

// extendedForm is the test Docker's CLI reads a --network value in its
// extended form by, matched anywhere in the value (opts/network.go).
var extendedForm = regexp.MustCompile(`\w+=\w+(,\w+=\w+)*`)

// extendedName is the network a --network value names in Docker's extended
// form, read as Docker's CLI reads one (opts/network.go): a value
// [extendedForm] matches is a CSV record of key=value fields, each lowercased
// and trimmed, and its last name field is the network. Empty for a value in
// the plain form, which is a network's name as written.
func extendedName(value string) string {
	if !extendedForm.MatchString(value) {
		return ""
	}
	fields, err := csv.NewReader(strings.NewReader(value)).Read()
	if err != nil {
		return ""
	}
	var name string
	for _, field := range fields {
		key, val, ok := strings.Cut(strings.ToLower(field), "=")
		if ok && strings.TrimSpace(key) == "name" {
			name = strings.TrimSpace(val)
		}
	}
	return name
}

// slirpReachesHost reports whether slirp4netns's options, as Podman 5 splits
// them at commas and reads them in order, allow the host's loopback: the last
// allow_host_loopback is true (slirp4netns(1)'s --disable-host-loopback,
// which Podman passes otherwise).
func slirpReachesHost(options []string) bool {
	allowed := false
	for _, option := range options {
		if value, ok := strings.CutPrefix(option, "allow_host_loopback="); ok {
			allowed = value == "true"
		}
	}
	return allowed
}

// pastaReachesHost reports whether pasta's options, as Podman splits them at
// commas, reach the host's loopback (pasta(1)): -T or -U (--tcp-ns,
// --udp-ns) forwarding the container's ports to the host's, or
// --map-host-loopback mapping an address to it, with any value but none; or
// Podman's own --map-gw, which drops the --no-map-gw it passes otherwise.
//
// Read as getopt_long(3) may read them: a long option by any prefix of its
// name, a short one anywhere in a cluster. So a value holding a T or a U
// reads as one too, which can only refuse a box that would not have reached
// the host, never pass one that would.
func pastaReachesHost(options []string) bool {
	for i := 0; i < len(options); i++ {
		option := options[i]
		var value string
		var inline bool
		switch {
		case option == "--map-gw":
			return true
		case strings.HasPrefix(option, "--"):
			name, v, ok := strings.Cut(option[len("--"):], "=")
			if !slices.ContainsFunc(pastaToHost, func(long string) bool { return abbreviates(name, long) }) {
				continue
			}
			value, inline = v, ok
		case strings.HasPrefix(option, "-"):
			at := strings.IndexAny(option, "TU")
			if at < 0 {
				continue
			}
			value = option[at+1:]
			inline = value != ""
		default:
			continue
		}
		if !inline && i+1 < len(options) {
			i++
			value = options[i]
		}
		if value != "none" {
			return true
		}
	}
	return false
}

// pastaToHost are pasta's long options that let the container reach the
// host's loopback, each taking a value: the long names of -T and -U, and
// --map-host-loopback.
var pastaToHost = []string{"tcp-ns", "udp-ns", "map-host-loopback"}

// abbreviates reports whether name is the long option full or a prefix of
// it, as getopt_long(3) takes an unambiguous one.
func abbreviates(name, full string) bool {
	return name != "" && strings.HasPrefix(full, name)
}
