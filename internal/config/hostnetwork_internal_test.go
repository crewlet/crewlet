package config

import (
	"fmt"
	"strings"
	"testing"
)

// EVERY SPELLING THAT JOINS A CONTAINER RUN TO ITS HOST'S NETWORK IS FOUND,
// WHERE IT IS WRITTEN, AND NO OTHER, pasta's forwards and mappings to the
// host's loopback among them. Each value is tried as the network setting and
// as every run_args spelling of it, one argument and two, after an argument
// that sets no network.
//
// Mutations: read only the last --network (a host mode before a bridge
// passes), skip the argument a flag took for its value (-e --network
// --network=host passes), read the extended form as a network named
// name=host, by a name but its last or wherever an "=" is, take pasta's -T
// none for forwarding, or any allow_host_loopback=true, and this goes red.
func TestEveryHostNetworkSpellingIsFound(t *testing.T) {
	t.Parallel()
	joins := []string{
		"host",
		"container:db",
		"ns:/proc/1/ns/net",
		// Docker's extended form, by its last name, which Docker lowercases.
		"name=host",
		"NAME=Host",
		"alias=db,name=host",
		`"name=host",alias=db`,
		"name=container:db",
		"name=mynet,name=host",
		"slirp4netns:allow_host_loopback=true",
		"slirp4netns:mtu=1500,allow_host_loopback=true",
		"slirp4netns:allow_host_loopback=false,allow_host_loopback=true",
		"pasta:--map-gw",
		"pasta:--map-host-loopback,10.0.2.2",
		"pasta:--map-host-loopback=10.0.2.2",
		"pasta:-T,5201",
		"pasta:-T5201",
		"pasta:--tcp-ns,5201",
		"pasta:--tcp-ns=auto",
		"pasta:-U,53",
		"pasta:--udp-ns=53",
		"pasta:--mtu,1500,-T,auto",
		"pasta:-T,none,--tcp-ns=5201", // a forward after a none
		"pasta:-4T5201",               // a cluster of short options
		"pasta:--tcp-n,5201",          // an abbreviation getopt_long takes
	}
	stays := []string{
		"",
		"bridge",
		"bridge:ip=10.88.0.10",
		"mynet",
		"mynet:alias=db",
		"mynet,othernet",
		"name=mynet,ip=10.0.0.5",
		"name=host,name=mynet", // Docker keeps the last name
		"name= host",           // not Docker's extended form: a network's name
		"none",
		"private",
		"default",
		"container", // a network of that name
		"HOST",      // likewise: only the extended form is lowercased
		"slirp4netns",
		"slirp4netns:allow_host_loopback=false",
		"slirp4netns:allow_host_loopback=true,allow_host_loopback=false", // Podman keeps the last
		"slirp4netns:port_handler=slirp4netns",
		"pasta",
		"pasta:-T,none",
		"pasta:--tcp-ns=none",
		"pasta:-Unone",
		"pasta:-t,8080,-u,8080", // into the container, not out of it
		"pasta:--map-host-loopback,none",
		"pasta:--no-map-gw",
		"pasta:--map-guest-addr,169.254.1.2",
		"pasta:--mtu,1500",
	}
	type spelling struct {
		network string
		args    []string
		want    string // the setting where it is written; empty: none
	}
	spellings := func(value string, joins bool) []spelling {
		at := func(where string) string {
			if !joins {
				return ""
			}
			return where
		}
		out := []spelling{{network: value, want: at("network: " + value)}}
		for _, flag := range []string{"--network", "--net"} {
			out = append(out,
				spelling{args: []string{"--memory=1g", flag + "=" + value}, want: at("run_args[1]: " + flag + "=" + value)},
				spelling{args: []string{"--memory=1g", flag, value}, want: at("run_args[1]: " + flag + " " + value)})
		}
		return out
	}
	var cases []spelling
	for _, value := range joins {
		cases = append(cases, spellings(value, true)...)
	}
	for _, value := range stays {
		cases = append(cases, spellings(value, false)...)
	}
	cases = append(cases,
		spelling{args: []string{"--network", "bridge", "--network=host"}, want: "run_args[2]: --network=host"},
		spelling{args: []string{"--network=host", "--network", "bridge"}, want: "run_args[0]: --network=host"},
		spelling{network: "bridge", args: []string{"--net", "host"}, want: "run_args[0]: --net host"},
		spelling{network: "host", args: []string{"--network=mynet"}, want: "network: host"},
		// -e takes the --network after it, so the flag that follows is the run's.
		spelling{args: []string{"-e", "--network", "--network=host"}, want: "run_args[2]: --network=host"},
		spelling{args: []string{"-e", "--net", "--net", "host"}, want: "run_args[2]: --net host"},
		spelling{args: []string{"--pod", "agents"}, want: "run_args[0]: --pod agents"},
		spelling{args: []string{"--pod=agents"}, want: "run_args[0]: --pod=agents"},
		spelling{args: []string{"--pod-id-file", "/run/agents.id"}, want: "run_args[0]: --pod-id-file /run/agents.id"},
		spelling{args: []string{"--pod=new:agents", "--pod", "new:agents"}},
		spelling{args: []string{"--memory=1g", "--network"}},
		spelling{args: []string{"--network-alias", "host", "--pid=host", "--uts=host", "--ipc=host"}},
	)
	for _, c := range cases {
		t.Run(fmt.Sprintf("network %q run_args %q", c.network, strings.Join(c.args, " ")), func(t *testing.T) {
			t.Parallel()
			got := hostNetwork(c.network, c.args)
			switch {
			case got == nil && c.want != "":
				t.Fatalf("hostNetwork found nothing, want %q", c.want)
			case got != nil && c.want == "":
				t.Fatalf("hostNetwork found %q, want nothing", got)
			case got != nil && got.String() != c.want:
				t.Fatalf("hostNetwork found %q, want %q", got, c.want)
			}
		})
	}
}
