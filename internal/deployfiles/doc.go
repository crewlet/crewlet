// Package deployfiles is THE CLOSED LIST of a repository's files the atlas may
// be told about: the deploy files that say what a repository runs as, and the
// dependency files that say which vendors its code calls. It decides by a
// file's PATH alone, reads no file and imports nothing of the engine, so
// Gitlet (which lists and serves the files) and the atlas's scan (which
// parses them) hold one copy of the list.
//
// # What is on the list, and why
//
//   - A root Dockerfile and a root Procfile, BY PRESENCE ONLY ([Format.Parsed]
//     is false): each says the repository builds one service, and nothing in
//     their bytes is needed. Their lines are build arguments, environment
//     values and command lines, which is where a secret sits, so they are
//     listed and never read.
//   - Compose files: the services a repository runs together, the images they
//     run and what each waits for.
//   - Kubernetes manifests, under a directory named k8s, kubernetes, manifests
//     or deploy: the workloads a cluster runs, and the databases, queues and
//     buckets an operator's resource asks for.
//   - A Helm chart's Chart.yaml: the chart's name and the charts it depends
//     on, a fixed schema. A VALUES FILE IS NEVER READ: it has no schema, and
//     it is where a chart's environment values and passwords are written.
//   - Terraform and OpenTofu files: the providers and the resource types, which
//     say the clouds a company builds on. Variable files and state are never
//     read, and no attribute's value is.
//   - fly.toml and a root render.yaml: the application a platform runs.
//   - package.json, go.mod, requirements files, pyproject.toml, Gemfile and
//     Cargo.toml: package NAMES, matched against a closed table of vendor
//     SDKs. A version, a URL or an index address beside a name is never read.
//
// # What is never on it
//
// A file in a directory whose name starts with a dot, or under a vendored,
// test or example tree, describes tooling or somebody else's software. A
// file whose own name starts with a dot (every .env file), a values file, a
// variable or state file, and a manifest named for secrets are refused by
// name. A symbolic link is refused by its mode, by whoever lists the tree.
// Everything else is simply not on the list, and a file not on it is never
// read: Gitlet refuses a read of one (gitlet.GitCodeFiles).
package deployfiles
