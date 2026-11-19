package cdp

// InputDispatchMouseEvent is Input.dispatchMouseEvent: a pointer event at a
// point of the viewport, in CSS pixels. First consumer: the render's actions
// (internal/artifactrender).
var InputDispatchMouseEvent = Method[InputDispatchMouseEventParams, struct{}]{name: "Input.dispatchMouseEvent"}

// InputDispatchMouseEventParams is Input.dispatchMouseEvent's parameters.
type InputDispatchMouseEventParams struct {
	// Type is "mousePressed", "mouseReleased", "mouseMoved" or
	// "mouseWheel".
	Type string  `json:"type"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	// Button is "none", "left", "middle", "right", "back" or "forward".
	Button     string `json:"button,omitempty"`
	Buttons    int64  `json:"buttons,omitempty"`
	ClickCount int64  `json:"clickCount,omitempty"`
}

// InputDispatchKeyEvent is Input.dispatchKeyEvent: a key, to whatever holds
// the focus. Like every parameter, its text is never logged or reported. First
// consumer: the render's actions.
var InputDispatchKeyEvent = Method[InputDispatchKeyEventParams, struct{}]{name: "Input.dispatchKeyEvent"}

// InputDispatchKeyEventParams is Input.dispatchKeyEvent's parameters.
type InputDispatchKeyEventParams struct {
	// Type is "keyDown", "keyUp", "rawKeyDown" or "char".
	Type                  string `json:"type"`
	Modifiers             int64  `json:"modifiers,omitempty"`
	Text                  string `json:"text,omitempty"`
	UnmodifiedText        string `json:"unmodifiedText,omitempty"`
	Key                   string `json:"key,omitempty"`
	Code                  string `json:"code,omitempty"`
	WindowsVirtualKeyCode int64  `json:"windowsVirtualKeyCode,omitempty"`
}
