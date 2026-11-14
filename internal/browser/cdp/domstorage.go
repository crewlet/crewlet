package cdp

// StorageID is DOMStorage.StorageId, named by its storage key, never by the
// security origin, which a partitioned site's storage does not have alone.
type StorageID struct {
	StorageKey     string `json:"storageKey"`
	IsLocalStorage bool   `json:"isLocalStorage"`
}

// DOMStorageEnable is DOMStorage.enable: report changes to the target's
// storage. First consumer: sign-in capture (internal/browser).
var DOMStorageEnable = Method[struct{}, struct{}]{name: "DOMStorage.enable"}

// DOMStorageGetDOMStorageItems is DOMStorage.getDOMStorageItems. First
// consumer: sign-in capture.
var DOMStorageGetDOMStorageItems = Method[DOMStorageGetDOMStorageItemsParams, DOMStorageGetDOMStorageItemsResult]{name: "DOMStorage.getDOMStorageItems"}

// DOMStorageGetDOMStorageItemsParams is DOMStorage.getDOMStorageItems'
// parameters.
type DOMStorageGetDOMStorageItemsParams struct {
	StorageID StorageID `json:"storageId"`
}

// DOMStorageGetDOMStorageItemsResult is DOMStorage.getDOMStorageItems'
// result: each entry its key and its value.
type DOMStorageGetDOMStorageItemsResult struct {
	Entries [][]string `json:"entries"`
}

// DOMStorageSetDOMStorageItem is DOMStorage.setDOMStorageItem. First
// consumer: sign-in restore.
var DOMStorageSetDOMStorageItem = Method[DOMStorageSetDOMStorageItemParams, struct{}]{name: "DOMStorage.setDOMStorageItem"}

// DOMStorageSetDOMStorageItemParams is DOMStorage.setDOMStorageItem's
// parameters.
type DOMStorageSetDOMStorageItemParams struct {
	StorageID StorageID `json:"storageId"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
}

// DOMStorageClear is DOMStorage.clear. First consumer: forgetting a sign-in.
var DOMStorageClear = Method[DOMStorageClearParams, struct{}]{name: "DOMStorage.clear"}

// DOMStorageClearParams is DOMStorage.clear's parameters.
type DOMStorageClearParams struct {
	StorageID StorageID `json:"storageId"`
}

// DOMStorageDomStorageItemAdded is DOMStorage.domStorageItemAdded. First
// consumer: sign-in checkpoints.
var DOMStorageDomStorageItemAdded = Event[DOMStorageDomStorageItemAddedEvent]{name: "DOMStorage.domStorageItemAdded"}

// DOMStorageDomStorageItemAddedEvent is DOMStorage.domStorageItemAdded's
// parameters.
type DOMStorageDomStorageItemAddedEvent struct {
	StorageID StorageID `json:"storageId"`
	Key       string    `json:"key"`
	NewValue  string    `json:"newValue"`
}

// DOMStorageDomStorageItemRemoved is DOMStorage.domStorageItemRemoved. First
// consumer: sign-in checkpoints.
var DOMStorageDomStorageItemRemoved = Event[DOMStorageDomStorageItemRemovedEvent]{name: "DOMStorage.domStorageItemRemoved"}

// DOMStorageDomStorageItemRemovedEvent is DOMStorage.domStorageItemRemoved's
// parameters.
type DOMStorageDomStorageItemRemovedEvent struct {
	StorageID StorageID `json:"storageId"`
	Key       string    `json:"key"`
}

// DOMStorageDomStorageItemUpdated is DOMStorage.domStorageItemUpdated. First
// consumer: sign-in checkpoints.
var DOMStorageDomStorageItemUpdated = Event[DOMStorageDomStorageItemUpdatedEvent]{name: "DOMStorage.domStorageItemUpdated"}

// DOMStorageDomStorageItemUpdatedEvent is DOMStorage.domStorageItemUpdated's
// parameters.
type DOMStorageDomStorageItemUpdatedEvent struct {
	StorageID StorageID `json:"storageId"`
	Key       string    `json:"key"`
	OldValue  string    `json:"oldValue"`
	NewValue  string    `json:"newValue"`
}

// DOMStorageDomStorageItemsCleared is DOMStorage.domStorageItemsCleared.
// First consumer: sign-in checkpoints.
var DOMStorageDomStorageItemsCleared = Event[DOMStorageDomStorageItemsClearedEvent]{name: "DOMStorage.domStorageItemsCleared"}

// DOMStorageDomStorageItemsClearedEvent is DOMStorage.domStorageItemsCleared's
// parameters.
type DOMStorageDomStorageItemsClearedEvent struct {
	StorageID StorageID `json:"storageId"`
}
