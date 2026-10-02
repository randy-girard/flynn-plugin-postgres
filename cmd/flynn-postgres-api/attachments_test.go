package main

import (
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

func TestAttachmentInfosForResource(t *testing.T) {
	r := &ct.Resource{
		OwnerApp: "demo",
		Apps:     []string{"demo", "shop"},
		Env: map[string]string{
			"FLYNN_POSTGRES":             "postgresql-basin-42915",
			"DATABASE_URL":               "postgres://db",
			"FLYNN_POSTGRESQL_AMBER_URL": "postgres://db",
		},
	}
	envOf := func(app string) map[string]string {
		if app == "shop" {
			return map[string]string{"FLYNN_POSTGRESQL_CRIMSON_URL": "postgres://db"}
		}
		return map[string]string{
			"DATABASE_URL":               "postgres://db",
			"FLYNN_POSTGRESQL_AMBER_URL": "postgres://db",
		}
	}
	got := attachmentInfosForResource(r, func(id string) string { return id }, envOf)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	byApp := map[string]postgres.AttachmentInfo{}
	for _, a := range got {
		byApp[a.App] = a
	}
	owner := byApp["demo"]
	if !owner.Owner || owner.As != "FLYNN_POSTGRESQL_AMBER_URL" || len(owner.Keys) != 2 {
		t.Fatalf("owner: %+v", owner)
	}
	shop := byApp["shop"]
	if shop.Owner || shop.As != "FLYNN_POSTGRESQL_CRIMSON_URL" {
		t.Fatalf("shop: %+v", shop)
	}
}

func TestHandlerInfoUsesControllerAttachments(t *testing.T) {
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "demo", Tenant: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	h.listAllResources = func() ([]*ct.Resource, error) {
		return []*ct.Resource{{
			ID:       "res-1",
			OwnerApp: "demo",
			Apps:     []string{"demo", "shop"},
			Env: map[string]string{
				"FLYNN_POSTGRES": inst.App,
				"DATABASE_URL":   "postgres://db",
			},
		}}, nil
	}
	h.appDisplayName = func(id string) string { return id }
	h.appReleaseEnv = func(app string) map[string]string {
		if app == "shop" {
			return map[string]string{"FLYNN_POSTGRESQL_CRIMSON_URL": "postgres://db"}
		}
		return map[string]string{"DATABASE_URL": "postgres://db", "FLYNN_POSTGRESQL_AMBER_URL": "postgres://db"}
	}
	views := h.controllerAttachmentViews(inst.ID, inst.App)
	if len(views) != 2 {
		t.Fatalf("views: %+v", views)
	}
	var shop postgres.AttachmentInfo
	for _, v := range views {
		if v.App == "shop" {
			shop = v
		}
	}
	if shop.As != "FLYNN_POSTGRESQL_CRIMSON_URL" || shop.Owner {
		t.Fatalf("shop: %+v", shop)
	}
}

func TestResourceMatchesInstance(t *testing.T) {
	r := &ct.Resource{
		ID:         "res-uuid",
		ExternalID: "ext-1",
		Env:        map[string]string{"FLYNN_POSTGRES": "postgresql-basin-42915"},
	}
	for _, ref := range []string{"res-uuid", "ext-1", "postgresql-basin-42915"} {
		if !resourceMatchesInstance(r, ref) {
			t.Fatalf("expected match %q", ref)
		}
	}
	if resourceMatchesInstance(r, "other") {
		t.Fatal("matched unrelated ref")
	}
	if resourceMatchesInstance(nil, "res-uuid") {
		t.Fatal("matched nil resource")
	}
}
