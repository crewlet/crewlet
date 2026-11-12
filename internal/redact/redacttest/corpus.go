// Package redacttest is the corpus every canary test plants: one credential
// of each shape the redaction pass knows, each assembled at run time so no
// source file holds a string a secret scanner reads as a live credential.
//
// A surface that must hold no credential (the atlas, whose values are typed
// and refuse a credential-shaped one) proves it by planting every shape in
// every field a vendor's answer or a person's input can carry, and asserting
// that no piece of any shape reaches its records. The corpus is held to the
// pass by its own test: a shape the pass stops recognising fails here first.
package redacttest

import (
	"strings"
)

// Shape is one credential shape, named for the marker the pass writes for it.
type Shape struct {
	Name   string
	Sample string
}

// body is n characters of a token's alphabet, deterministic and obviously
// filler.
func body(n int) string {
	const alphabet = "Ab3dE6gH9jK2mN5pQ8sT1vW4yZ7bC0eF"
	var b strings.Builder
	for i := range n {
		b.WriteByte(alphabet[(i*7+3)%len(alphabet)])
	}
	return b.String()
}

// hex is n lowercase hexadecimal digits.
func hex(n int) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for i := range n {
		b.WriteByte(digits[(i*5+1)%len(digits)])
	}
	return b.String()
}

// upper is n uppercase letters and digits.
func upper(n int) string { return strings.ToUpper(body(n)) }

// keyLine is one line of a key's body at PEM's width.
const keyLine = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"

// Shapes is every shape, each sample a credential the pass recognises.
func Shapes() []Shape {
	return []Shape{
		{"run-token", "crewlet_rt1." + body(8) + ".seat-1." + "1700000000" + "." + body(43)},
		{"google-token", "ya29." + body(30)},
		{"jwt", "eyJ" + body(12) + ".eyJ" + body(12) + "." + body(16)},
		{"openai-project-key", "sk-proj-" + body(32)},
		{"anthropic-key", "sk-ant-api03-" + body(40)},
		{"openai-key", "sk-" + body(32)},
		{"slack-token", "xoxb-" + "123456789012-1234567890123-" + body(24)},
		{"aws-key", "AKIA" + upper(16)},
		{"aws-session-key", "ASIA" + upper(16)},
		{"github-token", "ghp_" + body(36)},
		{"github-fine-grained-token", "github_pat_" + "11" + body(20) + "_" + body(59)},
		{"gitlab-token", "glpat-" + body(20)},
		{"gitlab-runner-token", "glrt-" + body(20)},
		{"digitalocean-token", "dop_v1_" + hex(64)},
		{"ssh-certificate", "ssh-ed25519-cert-v01@openssh.com " + body(48) + "=="},
		{"aws-secret-access-key", "aws_secret_access_key = " + body(40)},
		{"aws-session-token", "AWS_SESSION_TOKEN=" + body(120)},
		{"password", "password: " + body(16)},
		{"private-key", "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat(keyLine+"\n", 3) +
			"-----END PRIVATE KEY-----"},
	}
}
