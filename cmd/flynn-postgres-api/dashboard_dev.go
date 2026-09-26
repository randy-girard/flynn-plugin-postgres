package main

import (
	"log"
	"net/http"
	"os"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
)

func runDashboardDev() {
	_ = os.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	if _, _, err := store.Provision(postgres.ProvisionRequest{App: "demo", Tenant: "demo"}); err != nil {
		log.Fatal(err)
	}
	addr := dashui.DevAddr()
	log.Printf("postgres dashboard-dev listening on %s — open http://127.0.0.1%s/dashboard/?app_id=demo", addr, addr)
	log.Fatal(http.ListenAndServe(addr, dashui.DevHandler(newHandler(store))))
}
