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
