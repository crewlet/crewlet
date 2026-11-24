module github.com/crewlet/crewlet

go 1.27.2

// No package pattern reads an npm install as this module's, at any depth:
// flatted ships a Go package, so once `npm ci` had run in dashboard/, every
// ./... (vet, build, golangci-lint, the test partition) took it in. The rule
// is internal/sourcetree's, and a test there holds this line to it.
ignore node_modules

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/aws/aws-sdk-go-v2 v1.48.0
	github.com/aws/aws-sdk-go-v2/config v1.33.20
	github.com/aws/aws-sdk-go-v2/credentials v1.21.3
	github.com/aws/aws-sdk-go-v2/service/iam v1.67.0
	github.com/aws/aws-sdk-go-v2/service/s3 v1.116.0
	github.com/aws/aws-sdk-go-v2/service/sts v1.53.0
	github.com/aws/smithy-go v1.28.9
	github.com/coder/websocket v1.8.15
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/klauspost/compress v1.20.4
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/nats-io/nats-server/v2 v2.15.3
	github.com/nats-io/nats.go v1.56.0
	github.com/nats-io/nkeys v0.4.17
	github.com/nats-io/nuid v1.0.1
	github.com/openai/openai-go/v3 v3.103.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	github.com/tursodatabase/turso-go-platform-libs v0.8.3
	github.com/yuin/goldmark v1.8.7
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0
	go.opentelemetry.io/otel/metric v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/sdk/metric v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	golang.org/x/crypto v0.59.0
	golang.org/x/net v0.62.0
	golang.org/x/sync v0.25.0
	golang.org/x/sys v0.50.0
	golang.org/x/term v0.48.0
	golang.org/x/text v0.44.0
	gopkg.in/yaml.v3 v3.0.1
	turso.tech/database/tursogo v0.8.3
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.9.0-default-no-op // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.27 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.25 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.14.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.39.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.44.5 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/nats-io/jwt/v2 v2.8.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/tidwall/gjson v1.20.1 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
