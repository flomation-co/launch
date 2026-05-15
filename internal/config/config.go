package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"flomation.app/automate/launch/internal/mtls"
)

type HttpListenConfig struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type DatabaseConfig struct {
	Hostname           string `json:"hostname"`
	Port               int    `json:"port"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	Database           string `json:"database"`
	EncryptionKey      string `json:"encryption_key"`
	MaxIdleConnections int    `json:"max_idle_connections"`
	MaxOpenConnections int    `json:"max_open_connections"`
	SSLModeOverride    string `json:"ssl_mode"`
}

type ServiceConfig struct {
	URL         string  `json:"url"`
	InternalURL string  `json:"internal_url,omitempty"`
	Key         *string `json:"key"`
}

type SecurityConfig struct {
	IdentityService string `json:"identity_service"`
}

type GoogleConfig struct {
	ClientID        string  `json:"client_id"`
	ClientSecret    string  `json:"client_secret"`
	CredentialsFile *string `json:"credentials_file"`
}

type EmbeddingConfig struct {
	Enabled     bool   `json:"enabled"`
	Region      string `json:"region"`
	ModelID     string `json:"model_id"`
	Dimensions  int    `json:"dimensions"`
	TopK        int    `json:"top_k"`
	AccessKeyID string `json:"access_key_id,omitempty"`
	SecretKey   string `json:"secret_access_key,omitempty"`
}

// MetricsConfig controls the Prometheus /metrics endpoint.
type MetricsConfig struct {
	Enabled    bool     `json:"enabled"`
	AllowedIPs []string `json:"allowed_ips"`
}

type Config struct {
	Database         DatabaseConfig   `json:"database"`
	HttpListenConfig HttpListenConfig `json:"http"`
	Automate         ServiceConfig    `json:"automate"`
	Security         SecurityConfig   `json:"security"`
	Google           *GoogleConfig    `json:"google"`
	Embedding        *EmbeddingConfig `json:"embedding"`
	PublicURL        string           `json:"public_url"` // e.g. "https://launch.flomation.app"
	TLS              *mtls.TLSConfig  `json:"tls,omitempty"`
	Metrics          MetricsConfig    `json:"metrics"`
}

// InternalAPIURL returns the internal mTLS URL if configured,
// otherwise falls back to the public API URL.
func (c *Config) InternalAPIURL() string {
	if c.Automate.InternalURL != "" {
		return c.Automate.InternalURL
	}
	return c.Automate.URL
}

func LoadConfig(path string) (*Config, error) {
	filePath := filepath.Join(".", filepath.Clean(path))
	b, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var config Config

	err = json.Unmarshal(b, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}
