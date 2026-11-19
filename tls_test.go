package postgres

import (
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func verifyCert(t *testing.T, caPEM, certPEM, host string) error {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		t.Fatal("bad CA PEM")
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("bad cert PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: host})
	return err
}

// RES-14: the plugin's own connections verify the instance CA and host name
// (verify-full with that instance's CA as sslrootcert).
func TestMaintenanceURLVerifiesInstanceCA(t *testing.T) {
	bundle, err := GenerateTLSBundle(TLSHosts("postgresql-oak-10000"))
	if err != nil {
		t.Fatal(err)
	}
	inst := &Instance{App: "postgresql-oak-10000", AppUser: "u", AppPassword: "p", ServiceHost: "leader.postgresql-oak-10000.discoverd", TLSCA: bundle.CACert}
	u, err := url.Parse(inst.MaintenanceURL())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("sslmode") != "verify-full" {
		t.Fatalf("sslmode=%q, want verify-full: %s", q.Get("sslmode"), u.Redacted())
	}
	raw, err := os.ReadFile(q.Get("sslrootcert"))
	if err != nil {
		t.Fatalf("sslrootcert %q: %v", q.Get("sslrootcert"), err)
	}
	if strings.TrimSpace(string(raw)) != strings.TrimSpace(bundle.CACert) {
		t.Fatal("sslrootcert is not this instance's CA")
	}
	if err := verifyCert(t, string(raw), bundle.ServerCert, u.Hostname()); err != nil {
		t.Fatalf("instance certificate does not verify for %s: %v", u.Hostname(), err)
	}
	// The tenant connection string is unchanged (apps do not have the file).
	if c := inst.ConnectionURL(); !strings.Contains(c, "sslmode=require") || strings.Contains(c, "sslrootcert") {
		t.Fatalf("tenant URL changed: %s", c)
	}
	// An instance whose release has no CA keeps sslmode=require.
	legacy := &Instance{App: "postgresql-oak-10000", AppUser: "u", AppPassword: "p", ServiceHost: "leader.postgresql-oak-10000.discoverd"}
	if m := legacy.MaintenanceURL(); !strings.Contains(m, "sslmode=require") {
		t.Fatalf("legacy maintenance URL %s", m)
	}
}

func TestInstanceFromEnvCarriesTLSCA(t *testing.T) {
	inst := InstanceFromEnv("id", "postgresql-oak-10000", map[string]string{
		"FLYNN_POSTGRES":    "postgresql-oak-10000",
		"POSTGRES_USER":     "u",
		"POSTGRES_PASSWORD": "p",
		"POSTGRES_DB":       "db",
		EnvTLSCACert:        "CA-PEM",
	})
	if inst == nil || inst.TLSCA != "CA-PEM" {
		t.Fatalf("InstanceFromEnv must carry the instance CA: %+v", inst)
	}
}

// start.sh installs the release bundle so the server presents the
// certificate the plugin verifies.
func TestInstallTLSFilesWritesReleaseBundle(t *testing.T) {
	dir := t.TempDir()
	// A follower's pg_basebackup copies the primary's files; the release
	// bundle must replace them.
	_ = os.WriteFile(filepath.Join(dir, "server.crt"), []byte("primary cert"), 0600)
	env := map[string]string{EnvTLSServerCert: "CERT", EnvTLSServerKey: "KEY"}
	var owned []string
	ok, err := InstallTLSFiles(dir, func(k string) string { return env[k] }, func(p string) error { owned = append(owned, p); return nil })
	if err != nil || !ok {
		t.Fatalf("installed=%v err=%v", ok, err)
	}
	crt, _ := os.ReadFile(filepath.Join(dir, "server.crt"))
	key, _ := os.ReadFile(filepath.Join(dir, "server.key"))
	if string(crt) != "CERT\n" || string(key) != "KEY\n" {
		t.Fatalf("crt=%q key=%q", crt, key)
	}
	if info, _ := os.Stat(filepath.Join(dir, "server.key")); info.Mode().Perm() != 0600 {
		t.Fatalf("key mode %o", info.Mode().Perm())
	}
	if len(owned) != 2 {
		t.Fatalf("chown calls %v", owned)
	}
	// No bundle in env: leave the volume alone (the start script then keeps
	// or generates its own certificate).
	ok, err = InstallTLSFiles(t.TempDir(), func(string) string { return "" }, func(string) error { return nil })
	if err != nil || ok {
		t.Fatalf("installed=%v err=%v without a bundle", ok, err)
	}
}
