package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/randy-girard/flynn/pkg/certgen"
)

// Release env keys for an instance's TLS bundle. Each instance gets its own
// private CA; the CA key is discarded after the server certificate is issued.
const (
	EnvTLSCACert     = "POSTGRES_TLS_CA_CERT"
	EnvTLSServerCert = "POSTGRES_TLS_SERVER_CERT"
	EnvTLSServerKey  = "POSTGRES_TLS_SERVER_KEY"
)

// TLSBundle is PEM material for one instance's TLS listener on 5432.
type TLSBundle struct {
	CACert     string
	ServerCert string
	ServerKey  string
}

// GenerateTLSBundle creates a private CA and a server certificate for hosts.
func GenerateTLSBundle(hosts []string) (*TLSBundle, error) {
	if len(hosts) == 0 {
		hosts = []string{"localhost"}
	}
	ca, err := certgen.Generate(certgen.Params{IsCA: true})
	if err != nil {
		return nil, fmt.Errorf("generate ca: %s", err)
	}
	server, err := certgen.Generate(certgen.Params{Hosts: hosts, CA: ca})
	if err != nil {
		return nil, fmt.Errorf("generate server cert: %s", err)
	}
	return &TLSBundle{CACert: ca.PEM, ServerCert: server.PEM, ServerKey: server.KeyPEM}, nil
}

// TLSHosts are the certificate names for an instance app.
func TLSHosts(serviceName string, extra ...string) []string {
	hosts := []string{
		"leader." + serviceName + ".discoverd",
		serviceName + ".discoverd",
		"localhost",
		"127.0.0.1",
	}
	if d := strings.TrimSpace(os.Getenv("CLUSTER_DOMAIN")); d != "" {
		hosts = append(hosts, serviceName+"."+d, "*."+d)
	}
	return append(hosts, extra...)
}

// InstallTLSFiles writes the release bundle (POSTGRES_TLS_SERVER_CERT and
// POSTGRES_TLS_SERVER_KEY) into dir as server.crt and server.key, replacing
// what is there (a follower's pg_basebackup copies the primary's files). chown
// hands each file to the postgres user. It reports false and touches nothing
// when the release has no bundle (an instance created before RES-14).
func InstallTLSFiles(dir string, getenv func(string) string, chown func(string) error) (bool, error) {
	cert := strings.TrimSpace(getenv(EnvTLSServerCert))
	key := strings.TrimSpace(getenv(EnvTLSServerKey))
	if cert == "" || key == "" {
		return false, nil
	}
	for name, body := range map[string]string{"server.crt": cert, "server.key": key} {
		path := filepath.Join(dir, name)
		tmp := path + ".flynn"
		if err := os.WriteFile(tmp, []byte(body+"\n"), 0600); err != nil {
			return false, err
		}
		if err := os.Chmod(tmp, 0600); err != nil {
			return false, err
		}
		if chown != nil {
			if err := chown(tmp); err != nil {
				os.Remove(tmp)
				return false, err
			}
		}
		if err := os.Rename(tmp, path); err != nil {
			return false, err
		}
	}
	return true, nil
}

// caFileDir holds the CA files the plugin's libpq clients read.
var caFileDir = filepath.Join(os.TempDir(), "flynn-postgres-ca")

// CAFile writes caPEM to a 0600 file named by its digest and returns the
// path, for libpq's sslrootcert.
func CAFile(caPEM string) (string, error) {
	caPEM = strings.TrimSpace(caPEM)
	if caPEM == "" {
		return "", fmt.Errorf("empty CA")
	}
	sum := sha256.Sum256([]byte(caPEM))
	path := filepath.Join(caFileDir, hex.EncodeToString(sum[:])+".pem")
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == caPEM {
		return path, nil
	}
	if err := os.MkdirAll(caFileDir, 0700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(caFileDir, "ca-*.tmp")
	if err != nil {
		return "", err
	}
	if _, err := tmp.WriteString(caPEM + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}

// unavailableCAFile is used when the CA file cannot be written, so libpq
// fails instead of connecting unverified.
const unavailableCAFile = "/nonexistent/flynn-postgres-ca-unavailable.pem"

// VerifyFullURL turns a libpq URL into sslmode=verify-full with caPEM as
// sslrootcert: the server certificate must chain to that instance's CA and
// name the URL host. With no CA (an instance created before its release
// carried POSTGRES_TLS_CA_CERT) the URL is returned unchanged
// (sslmode=require). Whether a CA exists comes from the controller release,
// never from the network.
func VerifyFullURL(raw, caPEM string) string {
	if strings.TrimSpace(caPEM) == "" || strings.TrimSpace(raw) == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	path, err := CAFile(caPEM)
	if err != nil {
		path = unavailableCAFile
	}
	q := u.Query()
	q.Set("sslmode", "verify-full")
	q.Set("sslrootcert", path)
	u.RawQuery = q.Encode()
	return u.String()
}
