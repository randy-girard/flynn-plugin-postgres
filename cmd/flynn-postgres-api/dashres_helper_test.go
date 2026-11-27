package main

import (
	"testing"

	ct "github.com/randy-girard/flynn/controller/types"
)

// helperCtl is the smallest controller for the shared dashres.go helpers.
type helperCtl struct {
	release *ct.Release
	created []*ct.Release
}

func (h *helperCtl) ProvisionResource(*ct.ResourceReq) (*ct.Resource, error) { return nil, nil }
func (h *helperCtl) DeleteResource(string, string) (*ct.Resource, error)     { return nil, nil }
func (h *helperCtl) DeleteResourceApp(string, string, string) (*ct.Resource, error) {
	return nil, nil
}
func (h *helperCtl) GetAppRelease(string) (*ct.Release, error) { return h.release, nil }
func (h *helperCtl) CreateRelease(_ string, r *ct.Release) error {
	r.ID = "next"
	h.created = append(h.created, r)
	return nil
}
func (h *helperCtl) SetAppRelease(string, string) error {
	h.release = h.created[len(h.created)-1]
	return nil
}
func (h *helperCtl) CreateDeployment(string, string) (*ct.Deployment, error) {
	h.release = h.created[len(h.created)-1]
	return &ct.Deployment{}, nil
}

// MergeAttachment copies attachment values trimmed (a PEM loses its final
// newline), so the unset after Detach or Destroy must match the trimmed copy
// too, or FLYNN_POSTGRES_TRUSTED_CERT stays on the app.
func TestDashDetachUnsetsTrimmedAttachmentEnv(t *testing.T) {
	cert := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	res := &ct.Resource{ID: "res-1", OwnerApp: "owner", Apps: []string{"app", "owner"}, Env: map[string]string{
		"FLYNN_POSTGRES": "pg-x", "FLYNN_POSTGRES_TRUSTED_CERT": cert, "FLYNN_POSTGRES_TLS_ENABLED": "true",
		"FLYNN_POSTGRES_RED_URL": "postgres://u:p@leader.pg-x.discoverd:5432/d",
	}}
	c := &helperCtl{release: &ct.Release{ID: "cur", Env: map[string]string{
		"KEEP": "1", "FLYNN_POSTGRES_TRUSTED_CERT": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		"FLYNN_POSTGRES_TLS_ENABLED": "true", "FLYNN_POSTGRES_RED_URL": "postgres://u:p@leader.pg-x.discoverd:5432/d",
	}}}
	if err := dashDetach(c, "postgres", res, &ct.App{ID: "app"}); err != nil {
		t.Fatal(err)
	}
	if len(c.release.Env) != 1 || c.release.Env["KEEP"] != "1" {
		t.Fatalf("env after detach: %v", c.release.Env)
	}
}
