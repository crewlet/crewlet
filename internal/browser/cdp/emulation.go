package cdp

// EmulationSetDeviceMetricsOverride is Emulation.setDeviceMetricsOverride: a
// fixed viewport, so screenshots and hit tests read the same pixels. First
// consumer: perception (internal/browser).
var EmulationSetDeviceMetricsOverride = Method[EmulationSetDeviceMetricsOverrideParams, struct{}]{name: "Emulation.setDeviceMetricsOverride"}

// EmulationSetDeviceMetricsOverrideParams is
// Emulation.setDeviceMetricsOverride's parameters.
type EmulationSetDeviceMetricsOverrideParams struct {
	Width             int64   `json:"width"`
	Height            int64   `json:"height"`
	DeviceScaleFactor float64 `json:"deviceScaleFactor"`
	Mobile            bool    `json:"mobile"`
}

// EmulationSetFocusEmulationEnabled is Emulation.setFocusEmulationEnabled: a
// background tab keeps its focus, as a seat's tabs all are background ones.
// First consumer: input.
var EmulationSetFocusEmulationEnabled = Method[EmulationSetFocusEmulationEnabledParams, struct{}]{name: "Emulation.setFocusEmulationEnabled"}

// EmulationSetFocusEmulationEnabledParams is
// Emulation.setFocusEmulationEnabled's parameters.
type EmulationSetFocusEmulationEnabledParams struct {
	Enabled bool `json:"enabled"`
}

// EmulationSetEmulatedMedia is Emulation.setEmulatedMedia: the media type and
// features a page's queries read, such as prefers-color-scheme. First
// consumer: the artifact render's themes (internal/artifactrender).
var EmulationSetEmulatedMedia = Method[EmulationSetEmulatedMediaParams, struct{}]{name: "Emulation.setEmulatedMedia"}

// EmulationSetEmulatedMediaParams is Emulation.setEmulatedMedia's parameters.
type EmulationSetEmulatedMediaParams struct {
	Media    string         `json:"media,omitempty"`
	Features []MediaFeature `json:"features,omitempty"`
}

// MediaFeature is Emulation.MediaFeature: one media feature and its value.
type MediaFeature struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
