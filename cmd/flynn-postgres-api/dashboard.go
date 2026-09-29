package main

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	ct "github.com/randy-girard/flynn/controller/types"
)

var dashNav = [][2]string{
	{"./", "Overview"},
	{"databases", "Databases"},
	{"users", "Users"},
	{"backup", "Backup"},
	{"replication", "Followers"},
}

func (h *handler) mountDashboard() {
	wrap := func(fn func(http.ResponseWriter, *http.Request, *dashui.Session)) httprouter.Handle {
		return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
			dashui.Require(fn).ServeHTTP(w, r)
		}
	}
	h.router.GET("/dashboard", wrap(h.dashOverview))
	h.router.GET("/dashboard/", wrap(h.dashOverview))
	h.router.GET("/dashboard/card", wrap(h.dashCard))
	h.router.GET("/dashboard/databases", wrap(h.dashDatabases))
	h.router.GET("/dashboard/users", wrap(h.dashUsers))
	h.router.GET("/dashboard/backup", wrap(h.dashBackup))
	h.router.GET("/dashboard/replication", wrap(h.dashReplication))
}

func (h *handler) instancesFor(sess *dashui.Session) []*postgres.Instance {
	if sess == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []*postgres.Instance
	add := func(list []*postgres.Instance) {
		for _, inst := range list {
			if inst == nil {
				continue
			}
			key := inst.App
			if key == "" {
				key = inst.ID
			}
			if key == "" {
				continue
			}
			if seen[key] {
				continue
			}
			if inst.ID != "" {
				seen[inst.ID] = true
			}
			seen[key] = true
			out = append(out, inst)
		}
	}
	if h.store != nil {
		add(h.store.ForApp(sess.AppID))
		if sess.AppName != "" && sess.AppName != sess.AppID {
			add(h.store.ForApp(sess.AppName))
		}
	}
	add(h.instancesFromController(sess))
	linkFollowerApps(out)
	return out
}

func (h *handler) instancesFromController(sess *dashui.Session) []*postgres.Instance {
	if h == nil || sess == nil {
		return nil
	}
	app := strings.TrimSpace(sess.AppID)
	if app == "" {
		app = strings.TrimSpace(sess.AppName)
	}
	if app == "" {
		return nil
	}
	resources, err := h.appResources(app)
	if err != nil && sess.AppName != "" && sess.AppName != app {
		resources, err = h.appResources(sess.AppName)
	}
	if err != nil || len(resources) == 0 {
		return nil
	}
	var out []*postgres.Instance
	for _, r := range resources {
		if inst := instanceFromResource(r, app); inst != nil {
			h.enrichFromLive(inst)
			out = append(out, inst)
		}
	}
	return out
}

func (h *handler) enrichFromLive(inst *postgres.Instance) {
	if h == nil || inst == nil || strings.TrimSpace(inst.App) == "" || h.client == nil {
		return
	}
	live := loadLivePostgres(h.client, inst.App)
	if live == nil {
		return
	}
	if live.Role != "" {
		inst.Role = live.Role
		inst.ReadOnly = live.ReadOnly
	}
	if inst.LeaderID == "" && live.LeaderID != "" {
		inst.LeaderID = live.LeaderID
	}
	if len(inst.Databases) == 0 && len(live.Databases) > 0 {
		inst.Databases = append([]postgres.Database(nil), live.Databases...)
	}
}

func linkFollowerApps(insts []*postgres.Instance) {
	byApp := map[string]*postgres.Instance{}
	for _, inst := range insts {
		if inst != nil && inst.App != "" {
			byApp[inst.App] = inst
		}
	}
	for _, inst := range insts {
		if inst == nil || inst.Role != postgres.RoleFollower {
			continue
		}
		leader := byApp[inst.LeaderID]
		if leader == nil {
			continue
		}
		id := inst.App
		if id == "" {
			id = inst.ID
		}
		found := false
		for _, f := range leader.Followers {
			if f == id {
				found = true
				break
			}
		}
		if !found && id != "" {
			leader.Followers = append(leader.Followers, id)
		}
	}
}

func (h *handler) appResources(app string) ([]*ct.Resource, error) {
	if h != nil && h.listResources != nil {
		return h.listResources(app)
	}
	if h == nil || h.client == nil {
		return nil, nil
	}
	return h.client.AppResourceList(app)
}

func instanceFromResource(r *ct.Resource, app string) *postgres.Instance {
	if r == nil {
		return nil
	}
	env := r.Env
	if env == nil {
		env = map[string]string{}
	}
	if !postgresResourceEnv(env) && !strings.EqualFold(r.ProviderID, "postgres") {
		return nil
	}
	name := strings.TrimSpace(env["FLYNN_POSTGRES"])
	db := databaseNameFromEnv(env)
	role := postgres.RolePrimary
	leader := strings.TrimSpace(env["POSTGRES_LEADER"])
	if strings.EqualFold(strings.TrimSpace(env["POSTGRES_ROLE"]), "follower") || strings.TrimSpace(env["POSTGRES_PRIMARY_URL"]) != "" || leader != "" {
		role = postgres.RoleFollower
	}
	inst := &postgres.Instance{
		ID:       r.ID,
		App:      name,
		Role:     role,
		LeaderID: leader,
		ReadOnly: role == postgres.RoleFollower,
		Attachments: []postgres.Attachment{{
			App: app,
			Env: env,
		}},
	}
	if db != "" {
		inst.Databases = []postgres.Database{{Name: db}}
	}
	for _, aid := range r.Apps {
		if aid != "" && aid != app {
			inst.Attachments = append(inst.Attachments, postgres.Attachment{App: aid, Env: env})
		}
	}
	return inst
}

func postgresResourceEnv(env map[string]string) bool {
	if env == nil {
		return false
	}
	return env["FLYNN_POSTGRES"] != "" || env["POSTGRES_URL"] != "" || env["PGDATABASE"] != "" || env["POSTGRES_DB"] != ""
}

func databaseNameFromEnv(env map[string]string) string {
	if env == nil {
		return ""
	}
	for _, k := range []string{"PGDATABASE", "POSTGRES_DB"} {
		if v := strings.TrimSpace(env[k]); v != "" {
			return v
		}
	}
	for _, k := range []string{"POSTGRES_URL", "DATABASE_URL"} {
		raw := strings.TrimSpace(env[k])
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if k == "DATABASE_URL" && !strings.HasPrefix(strings.ToLower(u.Scheme), "postgres") {
			continue
		}
		db := strings.Trim(u.Path, "/")
		if i := strings.IndexByte(db, '/'); i >= 0 {
			db = db[:i]
		}
		if db != "" {
			return db
		}
	}
	return ""
}

func writeDash(w http.ResponseWriter, sess *dashui.Session, title, body string) {
	dashui.WriteHTML(w, sess, title, dashui.Nav(sess, dashNav...)+body)
}

func (h *handler) dashOverview(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	writeDash(w, sess, "Postgres", overviewHTML(h.instancesFor(sess)))
}

func (h *handler) dashCard(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	insts := h.instancesFor(sess)
	status := "empty"
	summary := "0 postgres instance(s)"
	if len(insts) > 0 {
		status = "ok"
		summary = fmt.Sprintf("%d postgres instance(s)", len(insts))
		if len(insts) == 1 {
			if insts[0].App != "" {
				summary = insts[0].App
			} else if len(insts[0].Databases) > 0 && insts[0].Databases[0].Name != "" {
				summary = insts[0].Databases[0].Name
			}
		}
	}
	dashui.WriteJSON(w, 200, dashui.Card{
		Status:   status,
		Attached: len(insts) > 0,
		Summary:  summary,
		EnvCount: envCount(insts, sess.AppID, sess.AppName),
		Details:  map[string]string{"tls": "required", "nodes": "1"},
	})
}

func (h *handler) dashDatabases(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Databases</h2><table><tr><th>Instance</th><th>Database</th><th>Role</th></tr>`)
	for _, inst := range h.instancesFor(sess) {
		role := string(inst.Role)
		if role == "" {
			role = string(postgres.RolePrimary)
		}
		if len(inst.Databases) == 0 {
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td class="muted">—</td><td>%s</td></tr>`, html.EscapeString(inst.App), html.EscapeString(role))
			continue
		}
		for _, db := range inst.Databases {
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td><code>%s</code></td><td>%s</td></tr>`, html.EscapeString(inst.App), html.EscapeString(db.Name), html.EscapeString(role))
		}
	}
	b.WriteString(`</table><p class="muted">Each attached resource is one instance. A --follow replica is listed even when it copies the leader database.</p></div>`)
	writeDash(w, sess, "Databases", b.String())
}

func (h *handler) dashUsers(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Users</h2><table><tr><th>Instance</th><th>User</th></tr>`)
	for _, inst := range h.instancesFor(sess) {
		for _, u := range inst.Users {
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td><code>%s</code></td></tr>`, html.EscapeString(inst.App), html.EscapeString(u.Name))
		}
	}
	b.WriteString(`</table><p class="muted">Users exist only in that instance. The platform appliance superuser is not listed.</p></div>`)
	writeDash(w, sess, "Users", b.String())
}

func (h *handler) dashBackup(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Backup</h2><p>Dump this instance only. There is no in-place upgrade. To move or resize, follow, wait, then promote.</p><ul>`)
	for _, inst := range h.instancesFor(sess) {
		fmt.Fprintf(&b, `<li><code>%s</code> volume <code>%s</code> rows %d</li>`, html.EscapeString(inst.App), html.EscapeString(inst.Volume), len(inst.Rows))
	}
	b.WriteString(`</ul></div>`)
	writeDash(w, sess, "Backup", b.String())
}

func (h *handler) dashReplication(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Follow</h2><p>A follower is a separate resource with one node. It is read-only until promote or unfollow. Streaming copies the same major version. Logical replication is the major-upgrade path.</p>`)
	b.WriteString(`<table><tr><th>App</th><th>Role</th><th>Leader</th><th>Followers</th><th>Lag</th><th>Runtime</th><th>Mode</th></tr>`)
	for _, inst := range h.instancesFor(sess) {
		role := string(inst.Role)
		if role == "" {
			role = string(postgres.RolePrimary)
		}
		leader := inst.LeaderID
		followers := inst.Followers
		lag := inst.LagBytes
		runtime := inst.Runtime
		mode := string(inst.Mode)
		if h.store != nil {
			if info, err := h.store.Info(inst.ID); err == nil {
				role = string(info.Role)
				leader = info.LeaderID
				followers = info.Followers
				lag = info.LagBytes
				runtime = info.Runtime
				mode = string(info.Mode)
			}
		}
		fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td><code>%s</code></td><td>%s</td><td>%d</td><td>%s</td><td>%s</td></tr>`,
			html.EscapeString(inst.App), html.EscapeString(role), html.EscapeString(leader),
			html.EscapeString(strings.Join(followers, ", ")), lag, html.EscapeString(runtime), html.EscapeString(mode))
	}
	b.WriteString(`</table><p class="muted">pg:follow creates the follower. pg:wait blocks until lag is zero. pg:promote rewrites the primary *_URL and leaves the old leader in place. pg:unfollow keeps a writable copy and stops receiving leader writes.</p></div>`)
	writeDash(w, sess, "Followers", b.String())
}

func overviewHTML(insts []*postgres.Instance) string {
	var b strings.Builder
	if len(insts) == 0 {
		b.WriteString(`<div class="card"><p class="muted">No Postgres resource is attached to this app.</p></div>`)
		return b.String()
	}
	for _, inst := range insts {
		health := "ok"
		if inst.Nodes != 1 {
			health = "unexpected node count"
		}
		fmt.Fprintf(&b, `<div class="card"><p class="ok">Health: %s</p>`, html.EscapeString(health))
		fmt.Fprintf(&b, `<p>TLS: <code>required</code> (<code>sslmode=require</code>)</p>`)
		fmt.Fprintf(&b, `<p>Nodes: <code>%d</code> · volume <code>%s</code> · app <code>%s</code></p>`, inst.Nodes, html.EscapeString(inst.Volume), html.EscapeString(inst.App))
		b.WriteString(`<table><tr><th>Env</th><th>Value</th></tr>`)
		for _, att := range inst.Attachments {
			fmt.Fprintf(&b, `<tr><td><code>%s_URL</code></td><td><code>%s</code></td></tr>`, html.EscapeString(att.As), html.EscapeString(maskURL(att.URL)))
		}
		b.WriteString(`</table><p>Attached apps</p><ul>`)
		if len(inst.Attachments) == 0 {
			b.WriteString(`<li class="muted">none</li>`)
		}
		for _, att := range inst.Attachments {
			fmt.Fprintf(&b, `<li><code>%s</code> as <code>%s</code></li>`, html.EscapeString(att.App), html.EscapeString(att.As))
		}
		b.WriteString(`</ul></div>`)
	}
	return b.String()
}

func envCount(insts []*postgres.Instance, apps ...string) int {
	refs := map[string]bool{}
	for _, app := range apps {
		if strings.TrimSpace(app) != "" {
			refs[app] = true
		}
	}
	n := 0
	for _, inst := range insts {
		matched := false
		for _, att := range inst.Attachments {
			if len(refs) == 0 || refs[att.App] {
				if len(att.Env) > 0 {
					n += len(att.Env)
				} else {
					n++
				}
				matched = true
				break
			}
		}
		if !matched && len(inst.Attachments) > 0 && len(refs) > 0 {
			n++
		}
	}
	return n
}

func maskURL(raw string) string {
	if i := strings.Index(raw, ":"); i >= 0 {
		if at := strings.Index(raw, "@"); at > i {
			return raw[:i+3] + ":***" + raw[at:]
		}
	}
	return raw
}
