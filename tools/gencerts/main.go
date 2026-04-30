// Command gencerts generates a self-signed CA and per-service TLS
// certificates for development and staging environments.
//
// Output structure:
//
//	<output-dir>/
//	  ca.pem              Platform CA certificate
//	  ca-key.pem          CA private key (never deployed to prod)
//	  api/cert.pem        API service certificate + key
//	  launch/cert.pem     Launch service certificate + key
//	  runner/cert.pem     Runner service certificate + key
//
// Usage:
//
//	go run tools/gencerts/main.go [flags]
//
// Flags:
//
//	-out              Output directory (default: certs/dev)
//	-ca-cn            CA common name (default: Flomation Dev CA)
//	-ca-org           CA organisation (default: Flomation)
//	-ca-validity      CA validity in days (default: 3650)
//	-cert-validity    Service cert validity in days (default: 365)
//	-service          Service name:CN pair, repeatable (default: api,launch,runner with CN=name)
//	-san-dns          Additional DNS SAN to add to all certs, repeatable
//	-san-ip           Additional IP SAN to add to all certs, repeatable
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ", ") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type serviceSpec struct {
	Name string
	CN   string
}

func main() {
	var (
		outDir       = flag.String("out", "certs/dev", "output directory")
		caCertFile   = flag.String("ca-cert", "", "path to existing CA certificate PEM (skips CA generation)")
		caKeyFile    = flag.String("ca-key", "", "path to existing CA private key PEM (required with -ca-cert)")
		caCN         = flag.String("ca-cn", "Flomation Dev CA", "CA common name (ignored when -ca-cert is set)")
		caOrg        = flag.String("ca-org", "Flomation", "CA organisation (ignored when -ca-cert is set)")
		caValidity   = flag.Int("ca-validity", 3650, "CA certificate validity in days (ignored when -ca-cert is set)")
		certValidity = flag.Int("cert-validity", 365, "service certificate validity in days")
		services     stringSlice
		extraDNS     stringSlice
		extraIPs     stringSlice
	)

	flag.Var(&services, "service", "service name[:CN] pair (repeatable, default: api,launch,runner)")
	flag.Var(&extraDNS, "san-dns", "additional DNS SAN for all service certs (repeatable)")
	flag.Var(&extraIPs, "san-ip", "additional IP SAN for all service certs (repeatable)")
	flag.Parse()

	if (*caCertFile == "") != (*caKeyFile == "") {
		fmt.Fprintln(os.Stderr, "error: -ca-cert and -ca-key must be provided together")
		os.Exit(1)
	}

	// Default services if none specified.
	if len(services) == 0 {
		services = stringSlice{"api", "launch", "runner"}
	}

	// Parse service specs.
	specs := make([]serviceSpec, 0, len(services))
	for _, s := range services {
		parts := strings.SplitN(s, ":", 2)
		name := parts[0]
		cn := name
		if len(parts) == 2 {
			cn = parts[1]
		}
		specs = append(specs, serviceSpec{Name: name, CN: cn})
	}

	// Parse extra IPs.
	var parsedIPs []net.IP
	for _, ip := range extraIPs {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			fmt.Fprintf(os.Stderr, "error: invalid IP address: %s\n", ip)
			os.Exit(1)
		}
		parsedIPs = append(parsedIPs, parsed)
	}

	opts := genOpts{
		OutDir:       *outDir,
		CACertFile:   *caCertFile,
		CAKeyFile:    *caKeyFile,
		CACN:         *caCN,
		CAOrg:        *caOrg,
		CAValidity:   time.Duration(*caValidity) * 24 * time.Hour,
		CertValidity: time.Duration(*certValidity) * 24 * time.Hour,
		Services:     specs,
		ExtraDNS:     extraDNS,
		ExtraIPs:     parsedIPs,
	}

	if err := run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

type genOpts struct {
	OutDir       string
	CACertFile   string // existing CA cert PEM (empty = generate new)
	CAKeyFile    string // existing CA key PEM (empty = generate new)
	CACN         string
	CAOrg        string
	CAValidity   time.Duration
	CertValidity time.Duration
	Services     []serviceSpec
	ExtraDNS     []string
	ExtraIPs     []net.IP
}

func run(opts genOpts) error {
	if err := os.MkdirAll(opts.OutDir, 0o750); err != nil {
		return err
	}

	var (
		caCert *x509.Certificate
		caKey  *ecdsa.PrivateKey
	)

	if opts.CACertFile != "" {
		// Load existing CA.
		var err error
		caCert, caKey, err = loadCA(opts.CACertFile, opts.CAKeyFile)
		if err != nil {
			return fmt.Errorf("load CA: %w", err)
		}
		fmt.Printf("CA: loaded from %s (CN=%s)\n", opts.CACertFile, caCert.Subject.CommonName)
	} else {
		// Generate new CA.
		var err error
		caCert, caKey, err = generateCA(opts)
		if err != nil {
			return err
		}
	}

	// Generate per-service certificates.
	for _, svc := range opts.Services {
		if err := generateServiceCert(opts, svc, caCert, caKey); err != nil {
			return fmt.Errorf("generate %s cert: %w", svc.Name, err)
		}
		fmt.Printf("  %s (CN=%s) written to %s/%s/\n", svc.Name, svc.CN, opts.OutDir, svc.Name)
	}

	fmt.Printf("\nDone. Service certificates valid for %d days.\n", int(opts.CertValidity.Hours()/24))
	return nil
}

func generateCA(opts genOpts) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}

	caSerial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	caTemplate := &x509.Certificate{
		SerialNumber: caSerial,
		Subject: pkix.Name{
			Organization:       []string{opts.CAOrg},
			OrganizationalUnit: []string{"Platform"},
			CommonName:         opts.CACN,
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(opts.CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	// Write CA files to output directory.
	if err := writePEM(filepath.Join(opts.OutDir, "ca.pem"), "CERTIFICATE", caCertDER); err != nil {
		return nil, nil, err
	}

	caKeyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(filepath.Join(opts.OutDir, "ca-key.pem"), "EC PRIVATE KEY", caKeyDER); err != nil {
		return nil, nil, err
	}

	fmt.Printf("CA: %s (%s)\n", opts.CACN, opts.CAOrg)
	fmt.Printf("  Written to %s/ca.pem (validity: %d days)\n", opts.OutDir, int(opts.CAValidity.Hours()/24))

	return caCert, caKey, nil
}

func loadCA(certFile, keyFile string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA cert: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("no PEM block found in %s", certFile)
	}

	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("no PEM block found in %s", keyFile)
	}

	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}

	return caCert, caKey, nil
}

func generateServiceCert(opts genOpts, svc serviceSpec, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	serial, err := randomSerial()
	if err != nil {
		return err
	}

	// Base SANs: localhost + service-specific names.
	dnsNames := []string{
		"localhost",
		svc.CN,
		svc.Name + ".flomation.local",
	}
	// Add CN as DNS SAN if it differs from the service name.
	if svc.CN != svc.Name {
		dnsNames = append(dnsNames, svc.Name)
	}
	dnsNames = append(dnsNames, opts.ExtraDNS...)

	ips := []net.IP{
		net.ParseIP("127.0.0.1"),
		net.ParseIP("::1"),
	}
	ips = append(ips, opts.ExtraIPs...)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization:       []string{opts.CAOrg},
			OrganizationalUnit: []string{svc.Name},
			CommonName:         svc.CN,
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(opts.CertValidity),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		DNSNames:    dnsNames,
		IPAddresses: ips,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}

	svcDir := filepath.Join(opts.OutDir, svc.Name)
	if err := os.MkdirAll(svcDir, 0o750); err != nil {
		return err
	}

	if err := writePEM(filepath.Join(svcDir, "cert.pem"), "CERTIFICATE", certDER); err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(filepath.Join(svcDir, "key.pem"), "EC PRIVATE KEY", keyDER)
}

func writePEM(path, blockType string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304
	if err != nil {
		return err
	}
	defer f.Close()

	return pem.Encode(f, &pem.Block{
		Type:  blockType,
		Bytes: data,
	})
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}