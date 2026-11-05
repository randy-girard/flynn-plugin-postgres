package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
)

// watchCopyProgress publishes progress when a follower's copy fingerprint
// changes, then stops. An unconfigured dashboard webhook does not start it.
func (h *handler) watchCopyProgress(inst *postgres.Instance) {
	if h == nil || h.store == nil || inst == nil || inst.Role != postgres.RoleFollower || !dashui.LiveConfigured() {
		return
	}
	id := firstNonEmpty(inst.App, inst.ID)
	apps := copyProgressApps(inst.Tenant, inst.App, attachmentApps(inst.Attachments))
	go func() {
		var last string
		deadline := time.Now().Add(postgres.DefaultUpgradeTimeout)
		for time.Now().Before(deadline) {
			p, err := h.store.Progress(id)
			if err != nil {
				return
			}
			fp := p.Phase + ":" + strconv.Itoa(p.Percent) + ":" + strconv.FormatBool(p.Ready)
			if fp != last {
				last = fp
				for _, appID := range apps {
					dashui.NotifyLive("postgres", appID, "progress", p.Follower)
				}
			}
			if p.Ready || p.Phase == postgres.PhaseFailed {
				return
			}
			time.Sleep(time.Second)
		}
	}()
}

func attachmentApps(items []postgres.Attachment) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.App)
	}
	return out
}

func copyProgressApps(tenant, app string, extra []string) []string {
	raw := append([]string{tenant, app}, extra...)
	seen := map[string]bool{}
	var out []string
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
