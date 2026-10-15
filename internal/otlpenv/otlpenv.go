// Package otlpenv resolves the standard OTLP exporter environment: for each
// signal, where its exports go, with which headers, over which protocol, and
// whether the operator switched it off. Every reader of those variables in the
// engine resolves them here.
//
// # Why one resolver, when the variables were meant to be the shared interface — ADR-0010
//
// Two halves read the OTLP environment: the engine's own exporters
// (internal/tracing, for traces and metrics) and the sandbox OTLP receiver
// (internal/sandbox), which forwards a coding box's telemetry to the backend
// the engine exports to, so the box's spans nest under the turn that started
// them. ADR-0010 made the standard variables the shared interface and let each
// half read them directly, on the theory that one set of variable names could
// not be read two ways. It was read three ways while every test passed: the
// receiver ignored OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, so a deployment that set
// only that variable exported the engine's spans and silently dropped every
// box's; the receiver ignored the protocol; and neither half percent-decoded a
// header value, so the documented `authorization=Bearer%20…` reached the
// backend with its `%20` intact and was refused. The names agreed and the
// meanings did not. So both halves call [Resolve], and the agreement test in
// internal/tracing holds them to it.
//
// # Whose rules these are
//
// The OTLP exporter specification's, as the OpenTelemetry Go SDK implements
// them — mirrored rather than reinvented, because the engine's own exporters
// ARE that SDK, and a resolver that disagreed with it would be a third reading:
//
//   - ENDPOINT. OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT is the full URL and is used
//     verbatim; OTEL_EXPORTER_OTLP_ENDPOINT is a BASE, and the signal's path
//     (`/v1/traces`, `/v1/metrics`, `/v1/logs`) is appended to it after a
//     trailing slash is trimmed. Neither set means the signal is not exported.
//   - HEADERS. `name=value` pairs separated by commas, each value
//     PERCENT-DECODED (url.PathUnescape) and then trimmed. A per-signal
//     OTEL_EXPORTER_OTLP_<SIGNAL>_HEADERS, when set, REPLACES the base
//     variable's set wholesale rather than merging into it — the SDK applies
//     the two as successive options and the second assigns the map. An entry
//     with no `=`, a name that is not an HTTP token, or a value that is not
//     valid percent-encoding is DROPPED, as the SDK drops it, and named in
//     [Destination.Dropped] so a caller can say so.
//   - PROTOCOL. OTEL_EXPORTER_OTLP_<SIGNAL>_PROTOCOL, else
//     OTEL_EXPORTER_OTLP_PROTOCOL, else `http/protobuf`, the specification's
//     default.
//   - EXPORTER. OTEL_<SIGNAL>_EXPORTER=none switches the signal off whatever
//     the endpoint says.
//   - COMPRESSION. OTEL_EXPORTER_OTLP_<SIGNAL>_COMPRESSION, else
//     OTEL_EXPORTER_OTLP_COMPRESSION: `gzip`, or `none`, the default.
//   - TLS. The endpoint's SCHEME decides whether an export is encrypted. An
//     https endpoint verifies the collector against the system's roots, or
//     against the PEM file OTEL_EXPORTER_OTLP_<SIGNAL>_CERTIFICATE, else
//     OTEL_EXPORTER_OTLP_CERTIFICATE, names; and presents the client
//     certificate and key the CLIENT_CERTIFICATE and CLIENT_KEY pair name —
//     the signal's own pair when both halves are set, else the base pair.
//     OTEL_EXPORTER_OTLP_INSECURE says nothing the scheme does not already:
//     the specification applies it only to an endpoint written WITHOUT a
//     scheme, which this package refuses, and the SDK the engine's exporters
//     are built on lets the scheme overrule it.
//
// # Where this is stricter than the SDK, and why
//
// The SDK accepts some values it can never honour, and accepts them silently.
// This package refuses them by name, because a telemetry setting that is
// silently ignored is how a deployment comes to export nowhere with a
// configuration that looks complete:
//
//   - An endpoint must be an `http://` or `https://` URL with a host and
//     nothing but a path. The SDK reads the host and the path and DROPS the
//     rest — userinfo, a query, a fragment — while the receiver's plain HTTP
//     client would have sent userinfo as basic auth: the same setting, read two
//     ways again. The specification says an endpoint MUST NOT carry those
//     parts. A credential belongs in the headers variable.
//   - A protocol must be one this build speaks: `http/protobuf` or `grpc`. The
//     SDK reads an unknown value as `http/protobuf`, so `GRPC` aimed at a gRPC
//     port exported nothing. `http/json` is refused too — the engine's
//     exporters have never been built for it. Case is not significant, as the
//     specification asks of every enumerated variable.
//   - An exporter must be `otlp` or `none`. A list, or a name such as
//     `console` or `zipkin`, names an exporter this build does not have.
//   - A compression must be `gzip` or `none`. The SDK reads anything else as
//     `none`, so `GZIP` — or a typo — exported uncompressed with no word.
//   - A certificate or a client key pair must be readable, PEM, and — for a
//     pair — a certificate and the key that matches it. The SDK reports a
//     file it cannot use through its error handler and exports without it:
//     to a collector that then refuses the handshake, or with a client
//     certificate quietly missing. Half a pair is refused too, where the SDK
//     skips it.
//   - Certificate material beside an `http://` endpoint is refused: it can
//     never be used, and the SDK's two exporters disagree about it — the
//     HTTP one refuses to start, the gRPC one encrypts the connection the
//     scheme said was plaintext. So is an OTEL_EXPORTER_OTLP_INSECURE that
//     contradicts the scheme, and one that is neither `true` nor `false`.
//
// The error names the variable to change and never quotes an endpoint's value,
// which is the one place an operator is likely to have put a credential. A
// certificate's PATH is quoted — it is a file name, not a secret, and the
// thing an operator has to go and check.
//
// # Imports nothing from the engine
//
// The standard library only, so every reader can depend on it without depending
// on another reader — the sandbox receiver must not import the tracing package
// to learn where traces go, nor the other way round.
package otlpenv

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
)

// Signal is one OTLP signal: what is exported, and the path segment it is
// exported under.
type Signal string

// The signals OTLP defines, and this engine resolves.
const (
	Traces  Signal = "traces"
	Metrics Signal = "metrics"
	Logs    Signal = "logs"
)

// Signals is every [Signal], in the order the specification lists them.
var Signals = []Signal{Traces, Metrics, Logs}

// Valid reports a signal this package resolves.
//
// A CLOSED SET, because a signal is concatenated into a variable name and into
// an upstream path: the receiver takes one from a request URL, and an
// unchecked segment would let a caller choose part of the address the
// engine's own credential is sent to.
func (s Signal) Valid() bool { return slices.Contains(Signals, s) }

// Path is the path the specification appends to a base endpoint.
func (s Signal) Path() string { return "/v1/" + string(s) }

// EndpointVar is the signal-specific endpoint variable, used verbatim.
func (s Signal) EndpointVar() string { return "OTEL_EXPORTER_OTLP_" + s.upper() + "_ENDPOINT" }

// HeadersVar is the signal-specific headers variable, which replaces the base
// set rather than adding to it.
func (s Signal) HeadersVar() string { return "OTEL_EXPORTER_OTLP_" + s.upper() + "_HEADERS" }

// ProtocolVar is the signal-specific protocol variable.
func (s Signal) ProtocolVar() string { return "OTEL_EXPORTER_OTLP_" + s.upper() + "_PROTOCOL" }

// ExporterVar is the variable that switches the signal off with `none`.
func (s Signal) ExporterVar() string { return "OTEL_" + s.upper() + "_EXPORTER" }

// CompressionVar is the signal-specific compression variable.
func (s Signal) CompressionVar() string { return "OTEL_EXPORTER_OTLP_" + s.upper() + "_COMPRESSION" }

// CertificateVar is the signal-specific trusted-certificate file variable.
func (s Signal) CertificateVar() string { return "OTEL_EXPORTER_OTLP_" + s.upper() + "_CERTIFICATE" }

// ClientCertificateVar is the signal-specific client-certificate file variable.
func (s Signal) ClientCertificateVar() string {
	return "OTEL_EXPORTER_OTLP_" + s.upper() + "_CLIENT_CERTIFICATE"
}

// ClientKeyVar is the signal-specific client-key file variable.
func (s Signal) ClientKeyVar() string { return "OTEL_EXPORTER_OTLP_" + s.upper() + "_CLIENT_KEY" }

// InsecureVar is the signal-specific insecure switch, checked against the
// endpoint's scheme and otherwise without effect.
func (s Signal) InsecureVar() string { return "OTEL_EXPORTER_OTLP_" + s.upper() + "_INSECURE" }

func (s Signal) upper() string { return strings.ToUpper(string(s)) }

// The base variables, which every signal reads unless its own is set.
const (
	// EndpointVar is the collector's BASE URL; the signal path is appended.
	EndpointVar = "OTEL_EXPORTER_OTLP_ENDPOINT"

	// HeadersVar is the backend's ingest credential, in the standard
	// `name=value,name2=value2` form with percent-encoded values.
	HeadersVar = "OTEL_EXPORTER_OTLP_HEADERS"

	// ProtocolVar selects the transport.
	ProtocolVar = "OTEL_EXPORTER_OTLP_PROTOCOL"

	// CompressionVar selects `gzip` or `none` for every signal.
	CompressionVar = "OTEL_EXPORTER_OTLP_COMPRESSION"

	// CertificateVar names the PEM file of the authorities an https
	// collector's certificate is verified against.
	CertificateVar = "OTEL_EXPORTER_OTLP_CERTIFICATE"

	// ClientCertificateVar and ClientKeyVar name the PEM files of the client
	// certificate an https export presents, and its key — a pair.
	ClientCertificateVar = "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"
	ClientKeyVar         = "OTEL_EXPORTER_OTLP_CLIENT_KEY"

	// InsecureVar is the specification's switch for an endpoint written
	// without a scheme; see the package doc.
	InsecureVar = "OTEL_EXPORTER_OTLP_INSECURE"
)

// Compression is how an export's body is compressed on the wire.
type Compression string

// The compressions, and the closed set an unknown value is refused against.
const (
	NoCompression Compression = "none"
	Gzip          Compression = "gzip"
)

// Compressions is every [Compression], the default first.
var Compressions = []Compression{NoCompression, Gzip}

// Valid reports a compression the exporters apply.
func (c Compression) Valid() bool { return slices.Contains(Compressions, c) }

// Protocol is an OTLP transport this build can speak.
type Protocol string

// The protocols, and the closed set an unknown value is refused against.
const (
	HTTPProtobuf Protocol = "http/protobuf"
	GRPC         Protocol = "grpc"
)

// Protocols is every [Protocol], the default first.
var Protocols = []Protocol{HTTPProtobuf, GRPC}

// Valid reports a protocol this build speaks.
func (p Protocol) Valid() bool { return slices.Contains(Protocols, p) }

// The values an OTEL_<SIGNAL>_EXPORTER variable may take. Unset is
// [ExporterOTLP].
const (
	ExporterOTLP = "otlp"
	ExporterNone = "none"
)

// Destination is where one signal's exports go.
type Destination struct {
	// Signal is the signal this resolves.
	Signal Signal

	// Endpoint is the full URL exports are sent to — for gRPC, the URL
	// whose scheme and host are dialled. Empty means no endpoint is
	// configured for this signal.
	Endpoint string

	// EndpointVar names the variable Endpoint came from, so a message about
	// the endpoint can name what to change. Empty when Endpoint is.
	EndpointVar string

	// Headers are sent with every export, decoded. Never nil.
	Headers map[string]string

	// Protocol is the transport, [HTTPProtobuf] when nothing says otherwise.
	Protocol Protocol

	// Declined reports that the signal's exporter variable is `none`: the
	// operator switched this signal off whatever the endpoint says.
	Declined bool

	// Compression is how an export's body is compressed, [NoCompression]
	// when nothing says otherwise.
	Compression Compression

	// TLS is the certificate material an https export uses beyond the
	// system's roots, by the files it names; the zero value is none. Always
	// zero for an http endpoint, which carries no TLS at all.
	TLS TLS

	// Dropped are the header entries the resolution could not use, in the
	// order they were written. The SDK drops the same entries; this records
	// them, because a dropped credential is an export the backend refuses,
	// and a reason given once at boot is better than a 401 on every batch.
	Dropped []Dropped
}

// Exports reports whether this signal is exported anywhere.
func (d Destination) Exports() bool { return d.Endpoint != "" && !d.Declined }

// Secure reports whether exports go over TLS: the endpoint's scheme is https.
func (d Destination) Secure() bool { return strings.HasPrefix(d.Endpoint, "https://") }

// TLS is the certificate material one destination is dialled with, by the
// files that hold it — which keeps a [Destination] comparable, and keeps a
// private key out of every value a test or a log line might print.
type TLS struct {
	// Certificate is the PEM file of the authorities the collector's
	// certificate is verified against, read from CertificateVar. Empty
	// verifies against the system's roots.
	Certificate    string
	CertificateVar string

	// ClientCertificate and ClientKey are the PEM files of the client
	// certificate presented to the collector, and its key, read from the
	// pair ClientCertificateVar names. Both empty presents none.
	ClientCertificate    string
	ClientKey            string
	ClientCertificateVar string
}

// Config builds the TLS configuration an exporter or a forwarder dials this
// destination with, or nil when the destination names no material of its own
// — an http endpoint, or an https one verified against the system's roots,
// which is what every client does with no configuration at all.
//
// The files are READ HERE, at the moment a client is built, which is when the
// SDK reads them too. [Resolve] has already read them once and refused what
// could not be used, so an error here is a file that changed in between.
func (d Destination) Config() (*tls.Config, error) {
	if !d.Secure() || d.TLS == (TLS{}) {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if d.TLS.Certificate != "" {
		pool, err := certPool(d.TLS.CertificateVar, d.TLS.Certificate)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if d.TLS.ClientCertificate != "" {
		pair, err := keyPair(d.TLS.ClientCertificateVar, d.TLS.ClientCertificate, d.TLS.ClientKey)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// Dropped is one header entry that was not used.
//
// It never carries the entry's VALUE, and carries its name only when the name
// was a valid header name: the entry that fails to parse is most often a
// credential typed in the wrong shape, and the place this is reported is a log.
type Dropped struct {
	// Var is the variable the entry was written in.
	Var string

	// Entry is its 1-based position in that variable's comma-separated
	// list, which is how an operator finds it without the value being
	// printed.
	Entry int

	// Header is the entry's name, when it was a valid one. Empty otherwise.
	Header string

	// Reason says what is wrong with it, in words that say what to write.
	Reason string
}

// Resolve reads one signal's destination from env.
//
// env returns a variable's value, or the empty string when it is unset; the
// value is trimmed here, so a caller passes its lookup as it is. An unset
// endpoint is not an error — it is a signal nothing exports — but a value that
// can never work is: an endpoint that is not an absolute http(s) URL with a
// host and nothing but a path, a protocol this build does not speak, or an
// exporter other than `otlp` or `none`. Each error names the variable.
func Resolve(env func(string) string, s Signal) (Destination, error) {
	if !s.Valid() {
		return Destination{}, fmt.Errorf("otlpenv: %q is not an OTLP signal; "+
			"the signals are %s", string(s), joinSignals())
	}
	get := func(name string) string { return strings.TrimSpace(env(name)) }

	d := Destination{Signal: s, Headers: map[string]string{}}

	exporter := get(s.ExporterVar())
	switch strings.ToLower(exporter) {
	case "", ExporterOTLP:
	case ExporterNone:
		d.Declined = true
	default:
		return Destination{}, fmt.Errorf("%s=%q names an exporter this build "+
			"does not have: set it to %s to export %s over OTLP, or %s to "+
			"export none", s.ExporterVar(), exporter, ExporterOTLP, s, ExporterNone)
	}

	endpoint, err := endpointFor(get, s)
	if err != nil {
		return Destination{}, err
	}
	if endpoint != "" {
		d.Endpoint = endpoint
		d.EndpointVar = EndpointVar
		if get(s.EndpointVar()) != "" {
			d.EndpointVar = s.EndpointVar()
		}
	}

	protocolVar, raw := s.ProtocolVar(), get(s.ProtocolVar())
	if raw == "" {
		protocolVar, raw = ProtocolVar, get(ProtocolVar)
	}
	d.Protocol = HTTPProtobuf
	if raw != "" {
		p := Protocol(strings.ToLower(raw))
		if !p.Valid() {
			return Destination{}, fmt.Errorf("%s=%q is not a protocol this "+
				"build speaks: set it to one of %s", protocolVar, raw, joinProtocols())
		}
		d.Protocol = p
	}

	headersVar := s.HeadersVar()
	if get(headersVar) == "" {
		headersVar = HeadersVar
	}
	if raw := get(headersVar); raw != "" {
		d.Headers, d.Dropped = parseHeaders(headersVar, raw)
	}

	if d.Compression, err = compressionFor(get, s); err != nil {
		return Destination{}, err
	}
	if err := d.resolveTLS(get, s); err != nil {
		return Destination{}, err
	}
	return d, nil
}

// compressionFor applies the compression rule: the signal's own variable, else
// the base, else none.
func compressionFor(get func(string) string, s Signal) (Compression, error) {
	name, raw := s.CompressionVar(), get(s.CompressionVar())
	if raw == "" {
		name, raw = CompressionVar, get(CompressionVar)
	}
	if raw == "" {
		return NoCompression, nil
	}
	c := Compression(strings.ToLower(raw))
	if !c.Valid() {
		return "", fmt.Errorf("%s=%q is not a compression the exporter applies: set it "+
			"to %s or %s", name, raw, Gzip, NoCompression)
	}
	return c, nil
}

// resolveTLS applies the TLS rule — see the package doc — and refuses
// material that can never be used.
//
// The SIGNAL'S OWN VARIABLE WINS, as the SDK applies the two in turn and the
// later assignment stands: its certificate over the base certificate, and its
// client pair over the base pair. A pair is taken from one level whole, never
// a certificate from one and a key from the other.
func (d *Destination) resolveTLS(get func(string) string, s Signal) error {
	insecureVar, insecure := s.InsecureVar(), get(s.InsecureVar())
	if insecure == "" {
		insecureVar, insecure = InsecureVar, get(InsecureVar)
	}
	if insecure != "" {
		var plaintext bool
		switch strings.ToLower(insecure) {
		case "true":
			plaintext = true
		case "false":
		default:
			return fmt.Errorf("%s=%q is neither true nor false", insecureVar, insecure)
		}
		if d.Endpoint != "" && plaintext == d.Secure() {
			scheme := "https://"
			if plaintext {
				scheme = "http://"
			}
			return fmt.Errorf("%s=%s contradicts %s's scheme, and the scheme is what "+
				"decides whether an export uses TLS: write the endpoint as %s, or unset %s",
				insecureVar, insecure, d.EndpointVar, scheme, insecureVar)
		}
	}

	tlsCfg := TLS{}
	tlsCfg.CertificateVar, tlsCfg.Certificate = s.CertificateVar(), get(s.CertificateVar())
	if tlsCfg.Certificate == "" {
		tlsCfg.CertificateVar, tlsCfg.Certificate = CertificateVar, get(CertificateVar)
	}
	if tlsCfg.Certificate == "" {
		tlsCfg.CertificateVar = ""
	} else if _, err := certPool(tlsCfg.CertificateVar, tlsCfg.Certificate); err != nil {
		return err
	}

	for _, pair := range [][2]string{
		{s.ClientCertificateVar(), s.ClientKeyVar()},
		{ClientCertificateVar, ClientKeyVar},
	} {
		cert, key := get(pair[0]), get(pair[1])
		switch {
		case cert == "" && key == "":
			continue
		case cert == "":
			return fmt.Errorf("%s is set without %s: a client key is presented with "+
				"its certificate, so set both or neither", pair[1], pair[0])
		case key == "":
			return fmt.Errorf("%s is set without %s: a client certificate is "+
				"presented with its key, so set both or neither", pair[0], pair[1])
		}
		if _, err := keyPair(pair[0], cert, key); err != nil {
			return err
		}
		tlsCfg.ClientCertificateVar, tlsCfg.ClientCertificate, tlsCfg.ClientKey = pair[0], cert, key
		break
	}

	if tlsCfg == (TLS{}) {
		return nil
	}
	if d.Endpoint != "" && !d.Secure() {
		named := tlsCfg.CertificateVar
		if named == "" {
			named = tlsCfg.ClientCertificateVar
		}
		return fmt.Errorf("%s names certificate material for %s, which is an http:// "+
			"endpoint and so carries no TLS to use it with: write the endpoint as "+
			"https://, or unset %s", named, d.EndpointVar, named)
	}
	d.TLS = tlsCfg
	return nil
}

// certPool reads a PEM file of certificate authorities.
func certPool(name, path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s=%q cannot be read: %w", name, path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s=%q holds no PEM certificate: it must name a "+
			"file of one or more -----BEGIN CERTIFICATE----- blocks", name, path)
	}
	return pool, nil
}

// keyPair reads a client certificate and its key, each a PEM file.
//
// The key file's CONTENT is never quoted, and neither is the parser's own
// error, which can echo a fragment of what it failed to read.
func keyPair(name, certPath, keyPath string) (tls.Certificate, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s=%q cannot be read: %w", name, certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("the client key %q beside %s cannot be read: %w",
			keyPath, name, err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s=%q and its key %q are not a PEM "+
			"certificate and the private key that matches it", name, certPath, keyPath)
	}
	return pair, nil
}

// ResolveAll resolves every signal, for a reader that serves all three.
func ResolveAll(env func(string) string) (map[Signal]Destination, error) {
	out := make(map[Signal]Destination, len(Signals))
	for _, s := range Signals {
		d, err := Resolve(env, s)
		if err != nil {
			return nil, err
		}
		out[s] = d
	}
	return out, nil
}

// DroppedOf is every dropped header entry across several destinations, each
// reported once.
//
// Three signals reading one base headers variable each drop the same entry,
// and a reader that resolves all three should say so once rather than three
// times.
func DroppedOf(ds ...Destination) []Dropped {
	var out []Dropped
	for _, d := range ds {
		for _, x := range d.Dropped {
			if !slices.Contains(out, x) {
				out = append(out, x)
			}
		}
	}
	return out
}

// endpointFor applies the endpoint rule: the signal's own variable verbatim,
// else the base with the signal path appended.
//
// # The two variables mean different things, and conflating them doubles the path
//
// The base is a base: the exporter appends the signal path to it, and the
// signal variable is the whole URL. Treating the base as a full URL is what
// put `/v1/traces/v1/traces` in front of a collector when an operator followed
// a guide that set the base to a URL ending in the signal path.
func endpointFor(get func(string) string, s Signal) (string, error) {
	if signal := get(s.EndpointVar()); signal != "" {
		if err := checkEndpoint(s.EndpointVar(), signal); err != nil {
			return "", err
		}
		return signal, nil
	}
	base := get(EndpointVar)
	if base == "" {
		return "", nil
	}
	if err := checkEndpoint(EndpointVar, base); err != nil {
		return "", err
	}
	return strings.TrimRight(base, "/") + s.Path(), nil
}

// checkEndpoint refuses an endpoint the exporters cannot honour as written.
//
// The value is NEVER quoted back: an endpoint is where an operator who has not
// found the headers variable puts a credential, and this error is printed at
// boot and kept in the logs.
func checkEndpoint(name, raw string) error {
	const how = "an OTLP endpoint is an http:// or https:// URL with a host, " +
		"e.g. http://localhost:4318"
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s does not parse as a URL; %s", name, how)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		// `localhost:4318` lands here: url.Parse reads `localhost` as a
		// scheme. The scheme is also what decides TLS, so it is never
		// guessed.
		return fmt.Errorf("%s has no http:// or https:// scheme — the scheme "+
			"is what decides whether the export uses TLS; %s", name, how)
	case u.Host == "":
		return fmt.Errorf("%s names no host; %s", name, how)
	case u.User != nil:
		return fmt.Errorf("%s carries credentials in its userinfo, which the "+
			"OTLP exporter drops without a word: remove them, and send them as "+
			"a header in %s instead (e.g. authorization=Basic%%20<base64>)",
			name, HeadersVar)
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Errorf("%s carries a query string, which the OTLP exporter "+
			"drops without a word: an endpoint is a scheme, a host and a path, "+
			"and a credential belongs in %s", name, HeadersVar)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("%s carries a fragment, which is never sent; an "+
			"endpoint is a scheme, a host and a path", name)
	}
	return nil
}

// parseHeaders reads one headers variable the way the SDK's envconfig does.
//
// Each comma-separated entry is cut at its first `=`; the name is trimmed and
// must be an HTTP token; the value is percent-decoded BEFORE it is trimmed,
// because that is the SDK's order and the order decides what `%20` at either
// end of a value means. A later entry with the same name replaces an earlier
// one. An entry that is blank — a trailing comma — is skipped without being
// reported, because nothing was lost.
func parseHeaders(name, raw string) (map[string]string, []Dropped) {
	out := map[string]string{}
	var dropped []Dropped
	for i, entry := range strings.Split(raw, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		key, value, found := strings.Cut(entry, "=")
		if !found {
			dropped = append(dropped, Dropped{Var: name, Entry: i + 1,
				Reason: "it has no '=': write it as name=value"})
			continue
		}
		key = strings.TrimSpace(key)
		if !isToken(key) {
			dropped = append(dropped, Dropped{Var: name, Entry: i + 1,
				Reason: "its name is not a valid HTTP header name"})
			continue
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			dropped = append(dropped, Dropped{Var: name, Entry: i + 1, Header: key,
				Reason: "its value is not valid percent-encoding: write a " +
					"literal % as %25"})
			continue
		}
		out[key] = strings.TrimSpace(decoded)
	}
	return out, dropped
}

// isToken reports an RFC 9110 token, which is what a header name is.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", c):
		default:
			return false
		}
	}
	return true
}

func joinSignals() string {
	names := make([]string, len(Signals))
	for i, s := range Signals {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

func joinProtocols() string {
	names := make([]string, len(Protocols))
	for i, p := range Protocols {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}
