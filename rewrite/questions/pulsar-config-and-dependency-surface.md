# q — two things the Pulsar backend needs from outside its own package

Raised by: building `internal/queue/pulsar`. Neither is a contract change, and
I have proceeded with the most defensible reading of both; they are here
because the decision belongs to somebody else's file.

## 1. `stream.tenant` and `stream.namespace` are optional in config and
required by the backend

`internal/config/bootstrap.go` allows a `type: pulsar` stream with no tenant
and no namespace (both `omitempty`, validated only for shape when present).
`pulsar.Config.Validate` **requires** both.

The reason is the whole reason this backend exists over embedded JetStream.
Pulsar ships `public/default` enabled with auto-topic-creation, so an unset
tenant does not fail — it silently puts every company on one estate into one
namespace: same topics, same subscription names, same seat inboxes. From the
broker's point of view they are one deployment, and nothing anywhere reports
an error. Multi-tenancy is not a feature you can leave at its default and
still have.

So the backend refuses, with a message that says why:

```
pulsar: invalid configuration: tenant is required: a Pulsar estate serves
many companies and the tenant is the boundary between them
```

**What I would like from `config`:** make `tenant` and `namespace` required
when `type: pulsar`, so an operator learns at config-validation time rather
than at engine start. The rule is one clause in `Stream.validate` beside the
pattern check that is already there. If instead they should default to
`public`/`default` for a dev loop, that is a legitimate call — but then it
should be an explicit default written in the config layer, not an empty string
the backend has to interpret.

Neither is auto-created: Pulsar auto-creates topics, never tenants or
namespaces. Both must exist before the engine starts, made out of band with
`pulsar-admin`. That is an operational note the deployment docs already carry
for the Python engine and it is unchanged.

## 2. `apache/pulsar-client-go` adds ~35 transitive modules to the default
build

Adding the client pulled in, among others, `k8s.io/client-go` and
`k8s.io/apimachinery` (for its Kubernetes-flavoured auth providers),
`prometheus/client_golang` (its metrics), `sirupsen/logrus`, `AthenZ/athenz`,
`RoaringBitmap`, `DataDog/zstd` and `hamba/avro`. Full list in the `go.mod`
diff for this change.

Every one of them is now in the single binary, including on the deployments
that run embedded JetStream and will never dial a Pulsar broker — which is the
majority topology and the one the rewrite exists to make possible.

**What I did:** nothing. The package builds unconditionally, because a build
tag would take the conformance suite out of the default `go build ./...` and
`go vet ./...` gates, and an uncertified backend does not exist as far as this
repo is concerned.

**What is worth deciding, by whoever owns the binary's shape:** whether the
Pulsar backend should be selected at build time (`//go:build pulsar`) or moved
behind a plugin seam, once there is a wiring layer that chooses a backend at
all. If so, the conformance job must still build and run it — the tag would
gate the *default* build, not the certification. Right now nothing constructs
a `pulsar.Queue`, so the question can be answered later without rework; it
just gets more expensive the longer the dependency set sits in `go.sum`
unchallenged.
