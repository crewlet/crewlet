package cdp

// InputDispatchMouseEvent is Input.dispatchMouseEvent: a pointer event at a
// point of the viewport, in CSS pixels. First consumer: input, at a point a
// hit test has checked.
var InputDispatchMouseEvent = Method[InputDispatchMouseEventParams, struct{}]{name: "Input.dispatchMouseEvent"}

// InputDispatchMouseEventParams is Input.dispatchMouseEvent's parameters.
type InputDispatchMouseEventParams struct {
	// Type is "mousePressed", "mouseReleased", "mouseMoved" or
	// "mouseWheel".
	Type      string  `json:"type"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Modifiers int64   `json:"modifiers,omitempty"`
	// Button is "none", "left", "middle", "right", "back" or "forward".
	Button     string  `json:"button,omitempty"`
	Buttons    int64   `json:"buttons,omitempty"`
	ClickCount int64   `json:"clickCount,omitempty"`
	DeltaX     float64 `json:"deltaX,omitempty"`
	DeltaY     float64 `json:"deltaY,omitempty"`
}

// InputDispatchKeyEvent is Input.dispatchKeyEvent: a key, to whatever holds
// the focus. Its text may be a password being typed, which this package never
// logs or reports. First consumer: input.
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
	NativeVirtualKeyCode  int64  `json:"nativeVirtualKeyCode,omitempty"`
}

// InputInsertText is Input.insertText: text into what holds the focus, as an
// input method would commit it. First consumer: input and credential fills.
var InputInsertText = Method[InputInsertTextParams, struct{}]{name: "Input.insertText"}

// InputInsertTextParams is Input.insertText's parameters.
type InputInsertTextParams struct {
	Text string `json:"text"`
}
