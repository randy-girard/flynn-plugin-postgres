package dashui

import (
	"os"
	"strings"
	"testing"
)

func TestLiveWebhookEnvInjected(t *testing.T) {
	raw, err := os.ReadFile("../../flynn-plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"DASHBOARD_METRICS_URL", "DASHBOARD_METRICS_SECRET"} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Fatalf("flynn-plugin.json inject_env missing %s", key)
		}
	}
}
