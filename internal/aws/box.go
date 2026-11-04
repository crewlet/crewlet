package aws

import (
	"encoding/json"
	"errors"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
)

// A seat's box holds two files and no AWS variable: ~/.aws/config, whose
// crewlet profile reads its credentials through credential_process, and the
// credentials file the completion poll rewrites as each session renews.
const (
	// BoxProfile is the named profile the box's AWS CLI signs as; Crewlet
	// never writes the default profile.
	BoxProfile = "crewlet"
	// BoxConfigPath and BoxCredentialsPath are relative to the box's home.
	BoxConfigPath      = ".aws/config"
	BoxCredentialsPath = ".aws/crewlet-credentials.json"
)

// BoxConfig is the box's ~/.aws/config: the crewlet profile in region.
// credential_process runs its command without a shell and expands neither ~
// nor a variable, so a shell reads the credentials file from the box's home,
// which every box sets HOME to.
func BoxConfig(region string) string {
	return "[profile " + BoxProfile + "]\n" +
		"region = " + region + "\n" +
		`credential_process = sh -c 'cat "$HOME/` + BoxCredentialsPath + `"'` + "\n"
}

// CredentialProcess is a seat session as credential_process prints it:
// version 1, with its expiry, so the CLI reads the file again before the
// session ends and so picks up the renewed one.
func CredentialProcess(c awssdk.Credentials) (string, error) {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" || !c.CanExpire {
		return "", errors.New("aws: a seat session needs an access key, its secret, a session token and an expiry")
	}
	b, err := json.Marshal(struct {
		Version         int    `json:"Version"`
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		SessionToken    string `json:"SessionToken"`
		Expiration      string `json:"Expiration"`
	}{1, c.AccessKeyID, c.SecretAccessKey, c.SessionToken, c.Expires.UTC().Format(time.RFC3339)})
	if err != nil {
		return "", err
	}
	return string(b), nil
}
