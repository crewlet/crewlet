package commentgate

// docInventory is, per package directory, the size in bytes of each package
// doc that ran past docBound when the bound arrived. It only moves down.
var docInventory = map[string]int{
	"internal/agent/builtin":    29064,
	"internal/api/mcpbridge":    50476,
	"internal/coord/kv":         31396,
	"internal/engine":           113165,
	"internal/estate":           46684,
	"internal/gitlet":           136353,
	"internal/gitlet/cache":     107050,
	"internal/gitlet/compact":   35774,
	"internal/gitlet/gitproc":   37530,
	"internal/gitlet/host":      62343,
	"internal/gitlet/query":     30244,
	"internal/objstore/natsobj": 20847,
	"internal/objstore/s3obj":   26571,
	"internal/queue/jetstream":  34853,
	"internal/redact":           23747,
	"internal/sandbox":          394112,
	"internal/sandbox/executor": 83104,
	"internal/statelog":         64169,
}
