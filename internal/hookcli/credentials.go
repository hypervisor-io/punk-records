package hookcli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Credentials is what a machine needs to reach one punk server: the URL
// and, when the server has API keys enabled, a bearer token. Stored at
// CredentialsPath with mode 0600 so no key ever has to sit in a shared
// settings file or a shell profile.
type Credentials struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key,omitempty"`
}

const defaultServerURL = "http://localhost:9090"

// CredentialsPath is $PUNK_CREDENTIALS or ~/.punk/credentials.json.
func CredentialsPath() string {
	if v := os.Getenv("PUNK_CREDENTIALS"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".punk", "credentials.json")
	}
	return filepath.Join(home, ".punk", "credentials.json")
}

// LoadCredentials reads path; a missing file is (zero, false, nil).
func LoadCredentials(path string) (Credentials, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, false, nil
	}
	if err != nil {
		return Credentials{}, false, err
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return Credentials{}, false, err
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c, true, nil
}

// SaveCredentials writes path atomically with mode 0600 (parent 0700).
func SaveCredentials(path string, c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	c.URL = strings.TrimRight(c.URL, "/")
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(raw, '\n'), 0o600)
}

// ResolveServer picks the server URL and API key for a CLI invocation:
// explicit flag, then PUNK_URL / PUNK_API_KEY, then the credentials
// file, then the local default with no key.
func ResolveServer(flagURL string) (url, apiKey string) {
	creds, _, _ := LoadCredentials(CredentialsPath())
	url = strings.TrimRight(flagURL, "/")
	if url == "" {
		url = strings.TrimRight(os.Getenv("PUNK_URL"), "/")
	}
	if url == "" {
		url = creds.URL
	}
	if url == "" {
		url = defaultServerURL
	}
	apiKey = os.Getenv("PUNK_API_KEY")
	if apiKey == "" {
		apiKey = creds.APIKey
	}
	return url, apiKey
}
