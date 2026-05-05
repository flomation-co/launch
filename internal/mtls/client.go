// Package mtls provides a TLS-configured HTTP client factory for
// mutual TLS (mTLS) service-to-service communication.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// TLSConfig holds the certificate paths and settings for mTLS.
type TLSConfig struct {
	Enabled      bool   `json:"enabled"`
	CACertFile   string `json:"ca_cert"`
	CertFile     string `json:"cert"`
	KeyFile      string `json:"key"`
	InternalPort int    `json:"internal_port"`
}

// NewClient creates an HTTP client configured with mutual TLS.
// The client presents its own certificate and verifies the server's
// certificate against the CA bundle. TLS 1.3 is the minimum version.
func NewClient(cfg *TLSConfig, timeout time.Duration) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load key pair: %w", err)
	}

	caCert, err := os.ReadFile(cfg.CACertFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA cert: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("mtls: failed to parse CA certificate")
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caPool,
				MinVersion:   tls.VersionTLS13,
			},
		},
	}, nil
}

// NewServerTLSConfig creates a tls.Config suitable for an internal
// mTLS listener that requires and verifies client certificates.
func NewServerTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	caCert, err := os.ReadFile(cfg.CACertFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA cert: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("mtls: failed to parse CA certificate")
	}

	return &tls.Config{
		ClientCAs:  caPool,
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13,
	}, nil
}

// ClientOrDefault returns an mTLS client if cfg is non-nil and enabled,
// otherwise returns a plain HTTP client with the given timeout.
// This allows callers to transparently support both mTLS and plain modes.
func ClientOrDefault(cfg *TLSConfig, timeout time.Duration) (*http.Client, error) {
	if cfg == nil || !cfg.Enabled {
		return &http.Client{Timeout: timeout}, nil
	}
	return NewClient(cfg, timeout)
}
