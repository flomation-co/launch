package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type HttpListenConfig struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type DatabaseConfig struct {
	Hostname           string  `json:"hostname"`
	Port               int     `json:"port"`
	Username           string  `json:"username"`
	Password           string  `json:"password"`
	Database           string  `json:"database"`
	EncryptionKey      string  `json:"encryption_key"`
	MaxIdleConnections int     `json:"max_idle_connections"`
	MaxOpenConnections int     `json:"max_open_connections"`
	SSLModeOverride    *string `json:"ssl_mode_override"`
}

type ServiceConfig struct {
	URL string  `json:"url"`
	Key *string `json:"key"`
}

type GoogleConfig struct {
	ClientID        string  `json:"client_id"`
	ClientSecret    string  `json:"client_secret"`
	CredentialsFile *string `json:"credentials_file"`
}

type Config struct {
	Database         DatabaseConfig   `json:"database"`
	HttpListenConfig HttpListenConfig `json:"http"`
	Automate         ServiceConfig    `json:"automate"`
	Google           *GoogleConfig    `json:"google"`
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
