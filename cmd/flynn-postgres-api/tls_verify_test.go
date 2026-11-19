package main

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

func certNames(t *testing.T, certPEM, host string) bool {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("no PEM block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf.VerifyHostname(host) == nil
}

// RES-14: each new instance gets its own CA and a certificate for its
// discoverd name, and the plugin remembers the CA.
func TestStartIsolatedInstanceIssuesTLSBundle(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	inst := &postgres.Instance{App: "postgresql-oak-10000", AppUser: "u", AppPassword: "p", Databases: []postgres.Database{{Name: "db"}}}
	if err := startIsolatedInstance(ctrl, "img-1", inst, nil, nil, func(string, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	env := ctrl.releases[0].Env
	if env[postgres.EnvTLSCACert] == "" || env[postgres.EnvTLSServerCert] == "" || env[postgres.EnvTLSServerKey] == "" {
		t.Fatalf("release has no TLS bundle: %v", keys(env))
	}
	if inst.TLSCA != env[postgres.EnvTLSCACert] {
		t.Fatal("instance does not carry its CA")
	}
	if !certNames(t, env[postgres.EnvTLSServerCert], "leader.postgresql-oak-10000.discoverd") {
		t.Fatal("certificate does not name the instance host")
	}
}

var errTestSQL = errors.New("test: no postgres")

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A scheduled backup verifies the instance CA from its release env.
func TestBackupConnURLVerifiesInstanceCA(t *testing.T) {
	env := map[string]string{
		"POSTGRES_USER":       "u",
		"POSTGRES_PASSWORD":   "p",
		"POSTGRES_DB":         "db",
		"FLYNN_POSTGRES":      "postgresql-oak-10000",
		postgres.EnvTLSCACert: "CA-PEM",
	}
	u, err := url.Parse(backupConnURL(env))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("sslmode") != "verify-full" || u.Query().Get("sslrootcert") == "" {
		t.Fatalf("backup url skips verification: %s", u.Redacted())
	}
	delete(env, postgres.EnvTLSCACert)
	if raw := backupConnURL(env); !strings.Contains(raw, "sslmode=require") {
		t.Fatalf("legacy backup url %s", raw)
	}
}

// Replica checks (failover, follower replace, progress) use the verified URL.
func TestReplicaProgressUsesVerifiedURL(t *testing.T) {
	var urls []string
	old := runSQL
	runSQL = func(connURL, query string) (string, error) {
		urls = append(urls, connURL)
		return "", errTestSQL
	}
	t.Cleanup(func() { runSQL = old })
	fol := &postgres.Instance{App: "postgresql-oak-10001", AppUser: "u", AppPassword: "p", ServiceHost: "leader.postgresql-oak-10001.discoverd", TLSCA: "CA-FOL", Role: postgres.RoleFollower}
	leader := &postgres.Instance{App: "postgresql-oak-10000", AppUser: "u", AppPassword: "p", ServiceHost: "leader.postgresql-oak-10000.discoverd", TLSCA: "CA-LEAD"}
	replicaCopyProgress(fol, leader)
	if len(urls) == 0 {
		t.Fatal("no replica query ran")
	}
	for _, raw := range urls {
		if !strings.Contains(raw, "sslmode=verify-full") {
			t.Fatalf("replica check skips verification: %s", raw)
		}
	}
}

// After a replacement adopts the original name, its certificate must name the
// host the plugin now dials.
func TestRewriteReleaseIdentityReissuesCertForAdoptedName(t *testing.T) {
	bundle, err := postgres.GenerateTLSBundle(postgres.TLSHosts("postgresql-temp-22222"))
	if err != nil {
		t.Fatal(err)
	}
	rel := &ct.Release{Env: map[string]string{
		"FLYNN_POSTGRES":          "postgresql-temp-22222",
		postgres.EnvTLSCACert:     bundle.CACert,
		postgres.EnvTLSServerCert: bundle.ServerCert,
		postgres.EnvTLSServerKey:  bundle.ServerKey,
	}}
	next, err := rewriteReleaseIdentity(rel, "postgresql-temp-22222", "postgresql-orig-33333")
	if err != nil {
		t.Fatal(err)
	}
	if next.Env[postgres.EnvTLSCACert] == bundle.CACert {
		t.Fatal("the adopted release must carry a certificate issued for the new name")
	}
	if !certNames(t, next.Env[postgres.EnvTLSServerCert], "leader.postgresql-orig-33333.discoverd") {
		t.Fatal("reissued certificate does not name the adopted host")
	}
	plain, err := rewriteReleaseIdentity(&ct.Release{Env: map[string]string{"FLYNN_POSTGRES": "postgresql-temp-22222"}}, "postgresql-temp-22222", "postgresql-orig-33333")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.Env[postgres.EnvTLSServerCert]; ok {
		t.Fatal("no TLS material must be added to a release without it")
	}
}
