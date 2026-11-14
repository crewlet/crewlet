package cdp

// StorageGetCookies is Storage.getCookies: every cookie of a context. First
// consumer: sign-in capture (internal/browser).
var StorageGetCookies = Method[StorageGetCookiesParams, StorageGetCookiesResult]{name: "Storage.getCookies"}

// StorageGetCookiesParams is Storage.getCookies' parameters.
type StorageGetCookiesParams struct {
	BrowserContextID BrowserContextID `json:"browserContextId,omitempty"`
}

// StorageGetCookiesResult is Storage.getCookies' result.
type StorageGetCookiesResult struct {
	Cookies []Cookie `json:"cookies"`
}

// StorageSetCookies is Storage.setCookies: cookies into a context, as a kept
// sign-in is restored. First consumer: sign-in restore.
var StorageSetCookies = Method[StorageSetCookiesParams, struct{}]{name: "Storage.setCookies"}

// StorageSetCookiesParams is Storage.setCookies' parameters.
type StorageSetCookiesParams struct {
	Cookies          List[CookieParam] `json:"cookies"`
	BrowserContextID BrowserContextID  `json:"browserContextId,omitempty"`
}

// StorageGetStorageKey is Storage.getStorageKey: the storage key of a frame,
// what DOMStorage names a site's storage by. It replaces
// Storage.getStorageKeyForFrame, which the protocol deprecates. First
// consumer: sign-in capture.
var StorageGetStorageKey = Method[StorageGetStorageKeyParams, StorageGetStorageKeyResult]{name: "Storage.getStorageKey"}

// StorageGetStorageKeyParams is Storage.getStorageKey's parameters: the
// frame, or with none the target the command is sent to.
type StorageGetStorageKeyParams struct {
	FrameID FrameID `json:"frameId,omitempty"`
}

// StorageGetStorageKeyResult is Storage.getStorageKey's result.
type StorageGetStorageKeyResult struct {
	StorageKey string `json:"storageKey"`
}
