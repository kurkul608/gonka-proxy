package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Document is the normalized, serializable form of a Config. It uses the same
// keys as the configuration file, so a Document encoded as YAML or JSON can be
// parsed back with Parse (unless its API keys were redacted).
type Document struct {
	Server                     DocumentServer           `json:"server" yaml:"server"`
	Cooldown                   string                   `json:"cooldown" yaml:"cooldown"`
	RecoveryWait               string                   `json:"recovery_wait" yaml:"recovery_wait"`
	ResponseHeaderTimeout      string                   `json:"response_header_timeout" yaml:"response_header_timeout"`
	MaxRoutingTime             string                   `json:"max_routing_time" yaml:"max_routing_time"`
	FailoverOn404              bool                     `json:"failover_on_404" yaml:"failover_on_404"`
	FailoverOnAuthError        bool                     `json:"failover_on_auth_error" yaml:"failover_on_auth_error"`
	LogLevel                   string                   `json:"log_level" yaml:"log_level"`
	ReasoningEffort            *string                  `json:"reasoning_effort" yaml:"reasoning_effort"`
	AllowClientReasoningEffort bool                     `json:"allow_client_reasoning_effort" yaml:"allow_client_reasoning_effort"`
	Models                     map[string]DocumentModel `json:"models" yaml:"models"`
	Providers                  []DocumentProvider       `json:"providers" yaml:"providers"`
}

// DocumentServer is the server block of a Document.
type DocumentServer struct {
	ListenAddress string `json:"listen_address" yaml:"listen_address"`
}

// DocumentModel is one Virtual Model's settings in a Document.
type DocumentModel struct {
	ReasoningEffort            *EffortSetting `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty"`
	AllowClientReasoningEffort *bool          `json:"allow_client_reasoning_effort,omitempty" yaml:"allow_client_reasoning_effort,omitempty"`
}

// DocumentProvider is one Provider in a Document.
type DocumentProvider struct {
	Name            string            `json:"name" yaml:"name"`
	BaseURL         string            `json:"base_url" yaml:"base_url"`
	APIKey          string            `json:"api_key" yaml:"api_key"`
	ModelAlias      string            `json:"model_alias,omitempty" yaml:"model_alias,omitempty"`
	Models          map[string]string `json:"models,omitempty" yaml:"models,omitempty"`
	Priority        int               `json:"priority" yaml:"priority"`
	Enabled         bool              `json:"enabled" yaml:"enabled"`
	ReasoningEffort *EffortSetting    `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty"`
}

// EffortSetting is an explicit optional reasoning_effort: a value, or null
// (Strip) which removes the field upstream. A nil *EffortSetting is "absent".
type EffortSetting struct {
	Value ReasoningEffort
	Strip bool
}

// MarshalJSON encodes a strip as null and a value as a string.
func (e EffortSetting) MarshalJSON() ([]byte, error) {
	if e.Strip {
		return []byte("null"), nil
	}
	return json.Marshal(string(e.Value))
}

// UnmarshalJSON decodes a string value. (encoding/json leaves a nil
// *EffortSetting for null, so a decoded Document cannot tell null from absent;
// Parse, which reads the raw document, can.)
func (e *EffortSetting) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*e = EffortSetting{Strip: true}
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*e = EffortSetting{Value: ReasoningEffort(value)}
	return nil
}

// MarshalYAML encodes a strip as null and a value as a string.
func (e EffortSetting) MarshalYAML() (any, error) {
	if e.Strip {
		return nil, nil
	}
	return string(e.Value), nil
}

func effortSetting(value *ReasoningEffort, strip bool) *EffortSetting {
	if strip {
		return &EffortSetting{Strip: true}
	}
	if value != nil {
		return &EffortSetting{Value: value.Normalize()}
	}
	return nil
}

// Document returns the normalized form of the Config with secrets intact.
func (c Config) Document() Document {
	doc := Document{
		Server:                     DocumentServer{ListenAddress: c.ListenAddress},
		Cooldown:                   c.Cooldown.String(),
		RecoveryWait:               c.RecoveryWait.String(),
		ResponseHeaderTimeout:      c.ResponseHeaderTimeout.String(),
		MaxRoutingTime:             c.MaxRoutingTime.String(),
		FailoverOn404:              c.FailoverOn404,
		FailoverOnAuthError:        c.FailoverOnAuthError,
		LogLevel:                   string(c.LogLevel),
		AllowClientReasoningEffort: c.AllowClientReasoningEffort,
		Models:                     make(map[string]DocumentModel, len(c.Models)),
		Providers:                  make([]DocumentProvider, 0, len(c.Providers)),
	}
	if doc.LogLevel == "" {
		doc.LogLevel = DefaultLogLevel
	}
	if c.ReasoningEffort != nil {
		value := string(c.ReasoningEffort.Normalize())
		doc.ReasoningEffort = &value
	}
	for name, settings := range c.Models {
		doc.Models[name] = DocumentModel{
			ReasoningEffort:            effortSetting(settings.ReasoningEffort, settings.StripReasoningEffort),
			AllowClientReasoningEffort: settings.AllowClientReasoningEffort,
		}
	}
	for _, provider := range c.Providers {
		entry := DocumentProvider{
			Name:            provider.Name,
			BaseURL:         provider.BaseURL,
			APIKey:          provider.APIKey,
			ModelAlias:      provider.ModelAlias,
			Priority:        provider.Priority,
			Enabled:         !provider.Disabled,
			ReasoningEffort: effortSetting(provider.ReasoningEffort, provider.StripReasoningEffort),
		}
		if len(provider.Models) > 0 {
			entry.Models = make(map[string]string, len(provider.Models))
			for virtual, upstream := range provider.Models {
				entry.Models[virtual] = upstream
			}
		}
		doc.Providers = append(doc.Providers, entry)
	}
	return doc
}

// Redacted returns a copy of the Document whose API keys show only their
// last four characters.
func (d Document) Redacted() Document {
	providers := make([]DocumentProvider, len(d.Providers))
	copy(providers, d.Providers)
	for index := range providers {
		providers[index].APIKey = RedactSecret(providers[index].APIKey)
	}
	d.Providers = providers
	return d
}

// RedactSecret masks a secret as "****" plus its last four characters (or
// just "****" when the secret is too short to reveal any of it safely).
func RedactSecret(secret string) string {
	if len(secret) < 12 {
		return "****"
	}
	return "****" + secret[len(secret)-4:]
}

// Version is a stable fingerprint of the normalized configuration: the hex
// SHA-256 of its canonical JSON encoding (secrets included, so a rotated key
// changes the version). Equivalent documents in YAML or JSON, with any key
// order or formatting, share one Version.
func (c Config) Version() string {
	encoded, err := json.Marshal(c.Document())
	if err != nil {
		// Document contains only strings, numbers, bools and maps; this cannot fail.
		panic(fmt.Sprintf("encode config document: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// MarshalFile renders the Config as a YAML configuration file (secrets
// included). Comments from the original file are not preserved.
func (c Config) MarshalFile() ([]byte, error) {
	return yaml.Marshal(c.Document())
}

// WriteFileAtomic replaces path with data via a temporary file in the same
// directory and a rename, so readers never see a partial file. The file is
// created with mode 0600.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary config file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temporary config file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temporary config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temporary config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temporary config file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("replace config file: %w", err)
	}
	return nil
}
