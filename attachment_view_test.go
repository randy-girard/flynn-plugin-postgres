package postgres

import "testing"

func TestAttachmentURLKeysOrdersColorBeforeDatabaseURL(t *testing.T) {
	keys := AttachmentURLKeys(
		map[string]string{
			"DATABASE_URL":               "postgres://db",
			"FLYNN_POSTGRESQL_AMBER_URL": "postgres://db",
			"KEEP":                       "yes",
		},
		map[string]string{
			"FLYNN_POSTGRES":             "postgresql-basin-42915",
			"DATABASE_URL":               "postgres://db",
			"FLYNN_POSTGRESQL_AMBER_URL": "postgres://db",
		},
	)
	if len(keys) != 2 || keys[0] != "FLYNN_POSTGRESQL_AMBER_URL" || keys[1] != "DATABASE_URL" {
		t.Fatalf("keys: %#v", keys)
	}
	if AttachmentName(keys) != "FLYNN_POSTGRESQL_AMBER_URL" {
		t.Fatalf("name: %q", AttachmentName(keys))
	}
}

func TestAttachmentURLKeysUsesAsName(t *testing.T) {
	keys := AttachmentURLKeys(
		map[string]string{"ANALYTICS_URL": "postgres://db", "KEEP": "yes"},
		map[string]string{"FLYNN_POSTGRES": "postgresql-basin-42915", "DATABASE_URL": "postgres://db"},
	)
	if len(keys) != 1 || keys[0] != "ANALYTICS_URL" {
		t.Fatalf("keys: %#v", keys)
	}
}

func TestInfoIncludesAttachments(t *testing.T) {
	s := NewStore()
	inst, _, err := s.Provision(ProvisionRequest{App: "demo", Tenant: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attach(inst.ID, "shop", "analytics"); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Attachments) != 2 {
		t.Fatalf("attachments: %+v", info.Attachments)
	}
	byApp := map[string]AttachmentInfo{}
	for _, a := range info.Attachments {
		byApp[a.App] = a
	}
	owner := byApp["demo"]
	if !owner.Owner || owner.As == "" {
		t.Fatalf("owner: %+v", owner)
	}
	shop := byApp["shop"]
	if shop.Owner || shop.As != "ANALYTICS_URL" {
		t.Fatalf("shop: %+v", shop)
	}
}
