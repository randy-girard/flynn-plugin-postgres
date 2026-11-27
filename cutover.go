package postgres

import (
	"net/url"
	"strings"
)

// RewriteAttachmentEnv points tenant *_URL values at the promoted primary
// and records FLYNN_POSTGRES as that app. Controller PutResource uses this
// after a follower swap; tenant jobs pick the new URL on their next deploy.
// Only URLs on the previous instance's host (its own or the stable name it
// answered for) change.
func RewriteAttachmentEnv(env map[string]string, previous, promoted *Instance) {
	if env == nil || promoted == nil {
		return
	}
	newHost := instanceServiceHost(promoted)
	if promoted.App != "" {
		env["FLYNN_POSTGRES"] = promoted.App
	}
	if newHost == "" {
		return
	}
	var oldHosts []string
	for _, h := range []string{instanceServiceHost(previous), previous.StableServiceHost()} {
		if h != "" && h != newHost {
			oldHosts = append(oldHosts, h)
		}
	}
	for k, v := range env {
		if k == "DATABASE_URL" || strings.HasSuffix(k, "_URL") {
			for _, old := range oldHosts {
				v = strings.ReplaceAll(v, old+":", newHost+":")
				v = strings.ReplaceAll(v, old+"/", newHost+"/")
				if strings.HasSuffix(v, old) {
					v = strings.TrimSuffix(v, old) + newHost
				}
			}
			env[k] = v
		}
	}
}

// RewriteReaderLogin replaces inst's own read-only follower login in *_URL
// values with its instance login once it no longer follows (a switchover
// made it the primary): apps attached to it can write. Other URLs, and URLs
// on other hosts, are left alone.
func RewriteReaderLogin(env map[string]string, inst *Instance) {
	if env == nil || inst == nil || inst.Role == RoleFollower {
		return
	}
	reader := strings.TrimSpace(inst.ReaderUser)
	if reader == "" || strings.TrimSpace(inst.AppUser) == "" {
		return
	}
	host := hostOf(instanceServiceHost(inst))
	for k, v := range env {
		if k != "DATABASE_URL" && !strings.HasSuffix(k, "_URL") {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.User == nil || u.User.Username() != reader || hostOf(u.Host) != host {
			continue
		}
		u.User = url.UserPassword(inst.AppUser, inst.AppPassword)
		env[k] = u.String()
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
