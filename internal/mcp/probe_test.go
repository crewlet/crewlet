package mcp

import (
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestProbeRecoversTheTriStateOverARealWire is the end-to-end for probe.go.
//
// Three annotation shapes a real server produces, on a real pipe, through the
// real SDK client. The middle one — an annotations object that mentions some
// hints and not others — is the case the SDK's typed struct cannot represent
// and an SDK-built test server cannot even emit.
func TestProbeRecoversTheTriStateOverARealWire(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "hints", "serve", map[string]string{
		helperToolsEnv: toolsJSON(
			[3]string{"silent", "No annotations at all", ""},
			[3]string{"partial", "Some hints, not others", `{"destructiveHint":true}`},
			[3]string{"explicit", "Every hint stated", `{"readOnlyHint":false,"openWorldHint":true,"idempotentHint":false,"title":"Open a PR"}`},
			[3]string{"reader", "Says it reads", `{"readOnlyHint":true}`},
		),
	}))
	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	byName := map[string]Annotations{}
	for _, d := range defs {
		byName[d.Name] = d.Annotations
	}

	cases := []struct {
		tool  string
		want  Annotations
		write bool
	}{
		{
			tool:  "silent",
			want:  Annotations{},
			write: false, // the whole point: nothing said is not "said no"
		},
		{
			tool:  "partial",
			want:  Annotations{Destructive: Yes},
			write: true,
		},
		{
			tool:  "explicit",
			want:  Annotations{Title: "Open a PR", ReadOnly: No, OpenWorld: Yes, Idempotent: No},
			write: true,
		},
		{
			tool:  "reader",
			want:  Annotations{ReadOnly: Yes},
			write: false,
		},
	}
	for _, tc := range cases {
		got, ok := byName[tc.tool]
		if !ok {
			t.Fatalf("tool %q missing from the listing", tc.tool)
		}
		if got != tc.want {
			t.Errorf("%s annotations = %+v, want %+v", tc.tool, got, tc.want)
		}
		if w := WritesToSharedSurface(got); w != tc.write {
			t.Errorf("%s WritesToSharedSurface = %v, want %v", tc.tool, w, tc.write)
		}
	}
}

// TestProbeDoesNotWarnWhenItSawTheListing pins the degradation signal.
//
// annotation_probe_missed means the wire shape moved under the probe and the
// tri-state is being guessed. A healthy listing must never log it, or the
// warning stops meaning anything.
func TestProbeDoesNotWarnWhenItSawTheListing(t *testing.T) {
	t.Parallel()
	log, rec := recorder()
	spec := helperSpec(t, "hints2", "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"silent", "d", ""}),
	})
	c, err := connect(t.Context(), spec, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.stop(t.Context()) })
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if rec.has("annotation_probe_missed") {
		t.Fatal("the probe reported a miss on a listing it must have seen")
	}
}

// TestProbeFallbackWarnsWhenItHasNoRecord probes the OTHER branch of defOf:
// the degraded read. Reached here by asking about a tool the probe never saw,
// which is what a changed wire shape would look like from inside.
func TestProbeFallbackWarnsWhenItHasNoRecord(t *testing.T) {
	t.Parallel()
	log, rec := recorder()
	c := &client{name: "unwatched", log: log, hints: newHintTable()}

	got := c.defOf(&sdk.Tool{Name: "ghost"})
	if got.Annotations != (Annotations{}) {
		t.Fatalf("fallback invented an assertion: %+v", got.Annotations)
	}
	if !rec.has("annotation_probe_missed") {
		t.Fatal("a probe miss must be loud: the tri-state is being guessed from here on")
	}
}
