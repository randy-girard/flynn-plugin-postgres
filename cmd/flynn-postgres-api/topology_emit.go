package main

import (
	"strings"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
)

func topologyMessage(from, to, extra string) string {
	var parts []string
	if from = strings.TrimSpace(from); from != "" {
		parts = append(parts, "from="+from)
	}
	if to = strings.TrimSpace(to); to != "" {
		parts = append(parts, "to="+to)
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, " ")
}

func (h *handler) emitTopology(code string, source *postgres.Instance, fromApp, toApp, extra string) {
	if h == nil {
		return
	}
	h.refreshDashboardMetricsEnv()
	srcName := "postgres"
	if source != nil {
		srcName = firstNonEmptyLocal(source.App, source.ID, "postgres")
	}
	msg := topologyMessage(fromApp, toApp, extra)
	line := postgres.FormatTopologyLine(srcName, postgres.TopologyEventName(code), msg)
	desc := postgres.TopologyDescription(code, fromApp, toApp, extra)
	note := postgres.FormatTopologyLine(srcName, "note", desc)
	if h.log != nil {
		h.log.Info(line)
		if desc != "" {
			h.log.Info(desc)
		}
	}
	for _, name := range topologyResourceApps(source, fromApp, toApp) {
		writeResourceLog(h, name, line)
		if desc != "" {
			writeResourceLog(h, name, note)
		}
	}
	apps := []string{}
	if source != nil {
		apps = instanceMetricApps(source)
	}
	apps = append(apps, fromApp, toApp)
	host := "postgres-plugin"
	if source != nil && strings.TrimSpace(source.App) != "" {
		if hid, err := runningAppHost(h.client, source.App); err == nil && hid != "" {
			host = hid
		}
	}
	for _, appID := range h.topologyAppIDs(apps) {
		dashui.PostFlynnEvent(dashui.FlynnEvent{
			HostID:      host,
			Code:        code,
			Description: desc,
			Severity:    postgres.TopologySeverity(code),
			AppID:       appID,
			ProcessType: postgres.ProcessName,
			Metadata: map[string]string{
				"source": srcName,
				"from":   strings.TrimSpace(fromApp),
				"to":     strings.TrimSpace(toApp),
			},
		})
	}
}

func (h *handler) topologyAppIDs(names []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	if h == nil || h.client == nil {
		for _, n := range names {
			add(n)
		}
		return out
	}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		app, err := h.client.GetApp(n)
		if err != nil || app == nil || strings.TrimSpace(app.ID) == "" {
			add(n)
			continue
		}
		add(app.ID)
	}
	return out
}
