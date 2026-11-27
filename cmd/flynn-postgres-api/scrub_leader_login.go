package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

// scrubFollowerAttachmentsFn is the maintenance step; tests replace it.
var scrubFollowerAttachmentsFn = (*handler).scrubFollowerAttachments

// primaryURLKeys are the plugin's replication URL keys. Only the follower's
// own instance release needs them; in a tenant release they hand out the
// leader's app role login.
var primaryURLKeys = []string{"FLYNN_POSTGRES_PRIMARY_URL", "POSTGRES_PRIMARY_URL"}

// scrubFollowerAttachments is the NEW-52 one-time repair, run by
// pg:maintenance (and at plugin start with MAINTENANCE_AUTO). For each
// postgres follower resource row (filtered like maintenance):
//
//   - a follower made before NEW-06 (no login of its own) gets its own
//     read-only login, created on the leader so replication copies it;
//   - FLYNN_POSTGRES_PRIMARY_URL / POSTGRES_PRIMARY_URL is removed from the
//     row and from each attached tenant release, but only when the value
//     points at the leader (a value the tenant set to another host stays);
//   - the row's and each tenant's *_URL that log in as the leader on the
//     follower's host switch to the follower's own login;
//   - each changed tenant app is deployed and waited for.
//
// The follower's own instance release keeps its replication URL. Running it
// again changes nothing. It returns one line per change or problem.
func (h *handler) scrubFollowerAttachments(only map[string]struct{}) []string {
	if h == nil || !h.live() || h.store == nil {
		return nil
	}
	var list []*ct.Resource
	var err error
	if h.listAllResources != nil {
		list, err = h.listAllResources()
	} else {
		list, err = h.client.ResourceListAll()
	}
	if err != nil {
		return []string{"list resources: " + err.Error()}
	}
	var report []string
	note := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		report = append(report, line)
		if h.log != nil {
			h.log.Info("follower attachment scrub", "change", line)
		}
	}
	for _, r := range list {
		if r == nil || r.Env == nil {
			continue
		}
		app := strings.TrimSpace(r.Env["FLYNN_POSTGRES"])
		if !postgres.IsolatedInstanceApp(app) || !maintenanceSelected(only, app) {
			continue
		}
		fol := h.knownInstance(app)
		rowFollower := strings.EqualFold(strings.TrimSpace(r.Env["FLYNN_POSTGRES_ROLE"]), "follower")
		if (fol == nil || fol.Role != postgres.RoleFollower) && !rowFollower && !hasAnyKey(r.Env, primaryURLKeys) {
			continue
		}
		var leader *postgres.Instance
		if fol != nil && fol.LeaderID != "" {
			leader = h.knownInstance(fol.LeaderID)
		}
		if leader == nil {
			if l := strings.TrimSpace(r.Env["FLYNN_POSTGRES_LEADER"]); l != "" {
				leader = h.knownInstance(l)
			}
		}
		if fol != nil && fol.Role == postgres.RoleFollower && !fol.PlatformReplica && leader != nil && strings.TrimSpace(fol.ReaderUser) == "" {
			if err := h.giveFollowerOwnLogin(fol, leader); err != nil {
				note("%s: could not create its own login on %s: %v (its apps keep their URL)", fol.App, leader.App, err)
			} else {
				fol, _ = h.store.Get(fol.ID)
				note("%s: created its own read-only login", fol.App)
			}
		}
		leaderHosts := map[string]bool{}
		if leader != nil {
			leaderHosts[hostOnly(leader.ServiceHost)] = true
			leaderHosts[hostOnly(leader.StableServiceHost())] = true
		}
		if l := strings.TrimSpace(r.Env["FLYNN_POSTGRES_LEADER"]); l != "" {
			leaderHosts["leader."+l+".discoverd"] = true
		}
		for _, k := range primaryURLKeys {
			if v := strings.TrimSpace(r.Env[k]); v != "" {
				leaderHosts[urlHost(v)] = true
			}
		}
		delete(leaderHosts, "")
		fix := func(env map[string]string) {
			scrubLeaderLogin(env, fol, leader, leaderHosts)
		}
		before := cloneEnv(r.Env)
		fix(r.Env)
		if !envEqual(before, r.Env) {
			if err := h.client.PutResource(r); err != nil {
				note("resource %s: %v", r.ID, err)
				continue
			}
			note("resource %s (%s): removed the leader's login", r.ID, app)
		}
		for _, appID := range resourceAppIDs(r) {
			name := h.resolveAppName(appID)
			changed, err := h.deployScrubbedTenant(appID, fix)
			switch {
			case err != nil:
				note("app %s: %v", name, err)
			case changed:
				note("app %s: removed the leader's login of %s and deployed", name, app)
			}
		}
	}
	return report
}

// knownInstance is the store's instance, else the one its release
// describes (adopted: this process may have started with an empty store).
func (h *handler) knownInstance(ref string) *postgres.Instance {
	if inst, err := h.store.Get(ref); err == nil && inst != nil {
		return inst
	}
	inst := loadLivePostgres(h.client, ref)
	if inst == nil {
		return nil
	}
	return h.store.Adopt(inst)
}

// scrubLeaderLogin removes the plugin's replication URL keys when they point
// at the leader, and moves *_URL values that log in as the leader on the
// follower's host onto the follower's own login.
func scrubLeaderLogin(env map[string]string, fol, leader *postgres.Instance, leaderHosts map[string]bool) {
	for _, k := range primaryURLKeys {
		if v, ok := env[k]; ok && leaderHosts[urlHost(v)] {
			delete(env, k)
		}
	}
	if fol == nil || leader == nil || strings.TrimSpace(fol.ReaderUser) == "" || strings.TrimSpace(fol.ReaderPassword) == "" {
		return
	}
	host := hostOnly(fol.ServiceHost)
	for k, v := range env {
		if k != "DATABASE_URL" && !strings.HasSuffix(k, "_URL") {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.User == nil || u.User.Username() != leader.AppUser || hostOnly(u.Host) != host {
			continue
		}
		u.User = url.UserPassword(fol.ReaderUser, fol.ReaderPassword)
		env[k] = u.String()
	}
}

// giveFollowerOwnLogin creates fol's read-only login on its leader (the
// same as a new follower, NEW-06) and records it in the store and in fol's
// own release, where the API reads it back after a restart.
func (h *handler) giveFollowerOwnLogin(fol, leader *postgres.Instance) error {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	next := *fol
	next.ReaderUser = "ro_" + strings.ToLower(strings.TrimSpace(fol.ID))
	next.ReaderPassword = "ropw_" + hex.EncodeToString(b)
	if err := createFollowerReader(leader, &next); err != nil {
		return err
	}
	if err := setInstanceReleaseEnv(h.client, fol.App, map[string]string{
		postgres.EnvReaderUser:     next.ReaderUser,
		postgres.EnvReaderPassword: next.ReaderPassword,
	}); err != nil {
		return err
	}
	return h.store.SetReaderLogin(fol.ID, next.ReaderUser, next.ReaderPassword)
}

// setInstanceReleaseEnv adds keys to an instance app's release (no restart:
// only the plugin reads them).
func setInstanceReleaseEnv(c isolatedReleaseClient, app string, set map[string]string) error {
	if !postgres.IsolatedInstanceApp(app) {
		return fmt.Errorf("postgres plugin must not rewrite tenant app %q", app)
	}
	a, err := c.GetApp(app)
	if err != nil || a == nil {
		if err == nil {
			err = fmt.Errorf("app %s not found", app)
		}
		return err
	}
	rel, err := c.GetAppRelease(a.ID)
	if err != nil || rel == nil {
		if err == nil {
			err = fmt.Errorf("app %s has no release", app)
		}
		return err
	}
	env := cloneEnv(rel.Env)
	for k, v := range set {
		env[k] = v
	}
	if envEqual(rel.Env, env) {
		return nil
	}
	next := *rel
	next.ID = ""
	next.Env = env
	if err := c.CreateRelease(a.ID, &next); err != nil {
		return err
	}
	return c.SetAppRelease(a.ID, next.ID)
}

// deployScrubbedTenant applies fix to a tenant app's head release and, if
// that changed anything, deploys it and waits. Instance apps are skipped.
func (h *handler) deployScrubbedTenant(appRef string, fix func(map[string]string)) (bool, error) {
	app, err := h.client.GetApp(appRef)
	if err != nil || app == nil {
		return false, err
	}
	if skipTenantPostgresDeploy(app, nil, nil) {
		return false, nil
	}
	rel, err := tenantHeadRelease(h.client, app.ID)
	if err != nil || rel == nil {
		return false, err
	}
	next := *rel
	next.ID = ""
	next.Env = cloneEnv(rel.Env)
	next.Processes = cloneProcesses(rel.Processes)
	fix(next.Env)
	for name, p := range next.Processes {
		if p.Env != nil {
			fix(p.Env)
			next.Processes[name] = p
		}
	}
	if envEqual(rel.Env, next.Env) && processesEnvEqual(rel.Processes, next.Processes) {
		return false, nil
	}
	stampSystemDeployMeta(&next, "System deployment: Flynn removed the Postgres leader's login from a follower attachment (FLYNN_POSTGRES_PRIMARY_URL). This is not a git push.")
	if err := h.client.CreateRelease(app.ID, &next); err != nil {
		return false, err
	}
	stop := make(chan struct{})
	timer := time.AfterFunc(instanceReadyTimeout, func() { close(stop) })
	defer timer.Stop()
	if err := h.client.DeployAppRelease(app.ID, next.ID, stop); err != nil {
		return false, fmt.Errorf("deploy: %w", err)
	}
	return true, nil
}

func hasAnyKey(env map[string]string, keys []string) bool {
	for _, k := range keys {
		if _, ok := env[k]; ok {
			return true
		}
	}
	return false
}

func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func hostOnly(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if i := strings.IndexByte(hostport, ':'); i >= 0 {
		return hostport[:i]
	}
	return hostport
}
