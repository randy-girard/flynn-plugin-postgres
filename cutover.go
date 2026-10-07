package postgres

import "strings"

// RewriteAttachmentEnv points tenant *_URL values at the promoted primary
// and records FLYNN_POSTGRES as that app. Controller PutResource uses this
// after a follower swap; tenant jobs pick the new URL on their next deploy.
func RewriteAttachmentEnv(env map[string]string, previous, promoted *Instance) {
	if env == nil || promoted == nil {
		return
	}
	oldHost := instanceServiceHost(previous)
	newHost := instanceServiceHost(promoted)
	if promoted.App != "" {
		env["FLYNN_POSTGRES"] = promoted.App
	}
	if oldHost == "" || newHost == "" || oldHost == newHost {
		return
	}
	for k, v := range env {
		if k == "DATABASE_URL" || strings.HasSuffix(k, "_URL") {
			env[k] = strings.ReplaceAll(v, oldHost, newHost)
		}
	}
}

func instanceServiceHost(inst *Instance) string {
	if inst == nil {
		return ""
	}
	if host := strings.TrimSpace(inst.ServiceHost); host != "" {
		return host
	}
	if inst.App != "" {
		return "leader." + inst.App + ".discoverd"
	}
	return ""
}
