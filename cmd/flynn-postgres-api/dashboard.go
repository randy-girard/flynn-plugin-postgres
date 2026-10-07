package main

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	ct "github.com/randy-girard/flynn/controller/types"
)

var dashNav = [][2]string{
	{"./", "Overview"},
	{"metrics", "Metrics"},
	{"databases", "Databases"},
	{"users", "Users"},
	{"backup", "Backup"},
	{"replication", "Followers"},
	{"settings", "Settings"},
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
	h.router.GET("/dashboard/metrics", wrap(h.dashMetrics))
	h.router.GET("/dashboard/api/diagnostics", wrap(h.dashDiagnostics))
	h.router.GET("/dashboard/databases", wrap(h.dashDatabases))
	h.router.GET("/dashboard/databases/new", wrap(h.dashDatabases))
	h.router.POST("/dashboard/databases", wrap(h.dashDatabases))
	h.router.GET("/dashboard/users", wrap(h.dashUsers))
	h.router.POST("/dashboard/users", wrap(h.dashUsers))
	h.router.GET("/dashboard/backup", wrap(h.dashBackup))
	h.router.GET("/dashboard/replication", wrap(h.dashReplication))
	h.router.GET("/dashboard/replication/new", wrap(h.dashReplication))
	h.router.POST("/dashboard/replication", wrap(h.dashReplication))
	h.router.POST("/dashboard/replication/new", wrap(h.dashReplication))
	h.router.GET("/dashboard/settings", wrap(h.dashSettings))
	h.router.POST("/dashboard/settings", wrap(h.dashSettings))
	h.router.GET("/dashboard/api/databases", wrap(h.dashListDatabases))
	h.router.POST("/dashboard/api/databases", wrap(h.dashCreateDatabase))
	h.router.GET("/dashboard/api/users", wrap(h.dashListUsers))
	h.router.POST("/dashboard/api/users", wrap(h.dashCreateUser))
	h.router.POST("/dashboard/api/users/drop", wrap(h.dashDropUser))
	h.router.GET("/dashboard/api/dump", wrap(h.dashDump))
	h.router.POST("/dashboard/api/dump", wrap(h.dashDump))
	h.router.POST("/dashboard/api/restore", wrap(h.dashRestore))
	h.router.GET("/dashboard/api/progress", wrap(h.dashProgress))
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
	for _, inst := range out {
		h.enrichFromLive(inst)
	}
	add(h.instancesFromController(sess))
	out = h.pruneGoneIsolatedInstances(out)
	linkFollowerApps(out)
	return out
}

func appGone(err error) bool {
	return err != nil && missingApp(err)
}

// pruneGoneIsolatedInstances drops in-memory instances whose Flynn app was
// already deleted (the other API worker handled deprovision). Otherwise the
// Followers tab keeps showing a replica of a gone primary (upland/basin).
func (h *handler) pruneGoneIsolatedInstances(insts []*postgres.Instance) []*postgres.Instance {
	if h == nil || h.client == nil {
		return insts
	}
	return pruneGoneIsolated(h.forgetInstance, func(name string) error {
		_, err := h.client.GetApp(name)
		return err
	}, insts)
}

func pruneGoneIsolated(forget func(*postgres.Instance), getApp func(string) error, insts []*postgres.Instance) []*postgres.Instance {
	if getApp == nil {
		return insts
	}
	var out []*postgres.Instance
	for _, inst := range insts {
		if inst == nil {
			continue
		}
		if postgres.IsolatedInstanceApp(inst.App) {
			if err := getApp(inst.App); appGone(err) {
				if forget != nil {
					forget(inst)
				}
				continue
			}
		}
		out = append(out, inst)
	}
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
			h.hydrateInstanceAttachments(inst, r)
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
	inst := postgres.InstanceFromEnv(r.ID, name, env)
	if inst == nil {
		return nil
	}
	inst.Tenant = app
	inst.Attachments = []postgres.Attachment{{
		App: app,
	}}
	leader := strings.TrimSpace(env["POSTGRES_LEADER"])
	role := strings.TrimSpace(env["POSTGRES_ROLE"])
	if strings.EqualFold(role, "primary") || strings.EqualFold(role, "standalone") {
		inst.Role = postgres.RolePrimary
		inst.ReadOnly = false
		inst.LeaderID = ""
	} else if strings.EqualFold(role, "deposed") {
		inst.Role = postgres.RoleDeposed
		inst.ReadOnly = true
		if leader != "" {
			inst.LeaderID = leader
		}
	} else if strings.EqualFold(role, "follower") || strings.TrimSpace(env["POSTGRES_PRIMARY_URL"]) != "" || leader != "" {
		inst.Role = postgres.RoleFollower
		inst.ReadOnly = true
		if leader != "" {
			inst.LeaderID = leader
		}
	}
	for _, aid := range r.Apps {
		if aid != "" && aid != app {
			inst.Attachments = append(inst.Attachments, postgres.Attachment{App: aid})
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
	for k, raw := range env {
		if !strings.HasSuffix(k, "_URL") {
			continue
		}
		raw = strings.TrimSpace(raw)
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
		if db != "" && db != "postgres" {
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

func (h *handler) dashMetrics(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	diag := h.diagnosticsFor(sess)
	var b strings.Builder
	if diag.Source == "" && len(h.instancesFor(sess)) == 0 {
		b.WriteString(`<div class="card"><p class="muted">No samples yet. The plugin posts series every 20s to the dashboard plugin-metrics webhook. The isolated postgres job writes flynn-postgres sample# lines to this resource app&rsquo;s logs.</p></div>`)
		writeDash(w, sess, "Metrics", b.String())
		return
	}
	b.WriteString(`<div class="card"><table><tr><th>Series</th><th>Value</th></tr>`)
	for _, name := range postgresMetricSeries {
		fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%v</td></tr>`, html.EscapeString(name), diag.Series[name])
	}
	b.WriteString(`</table><p class="muted">Posted to the dashboard plugin-metrics webhook. Charts and slow queries render in the cluster dashboard Metrics tab.</p></div>`)
	if len(diag.SlowQueries) > 0 {
		b.WriteString(`<div class="card"><h2>Slow queries</h2><table><tr><th>Query</th><th>Calls</th><th>Mean</th><th>Max</th></tr>`)
		for _, q := range diag.SlowQueries {
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%d</td><td>%.1f ms</td><td>%.1f ms</td></tr>`, html.EscapeString(q.Query), q.Calls, q.MeanMS, q.MaxMS)
		}
		b.WriteString(`</table></div>`)
	}
	writeDash(w, sess, "Metrics", b.String())
}

func (h *handler) dashDiagnostics(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	dashui.WriteJSON(w, http.StatusOK, h.diagnosticsFor(sess))
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
		Details:  mergeDetails(map[string]string{"tls": "required", "nodes": "1"}, postgres.EngineCardDetails(insts)),
	})
}

func mergeDetails(base map[string]string, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func dashNewPanel(r *http.Request) bool {
	p := strings.TrimSuffix(strings.ToLower(r.URL.Path), "/")
	return strings.HasSuffix(p, "/new")
}

func dashRedirectQueryNew(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("new")))
	if v != "1" && v != "true" {
		return false
	}
	if dashNewPanel(r) {
		return false
	}
	http.Redirect(w, r, strings.TrimSuffix(r.URL.Path, "/")+"/new", http.StatusFound)
	return true
}

func (h *handler) dashDatabases(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	notice := ""
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		msg, err := h.createDatabaseFromRequest(r, sess)
		if wantsJSON(r) {
			if err != nil {
				writeAPIError(w, err)
				return
			}
			dashui.WriteJSON(w, 200, map[string]string{"status": "ok", "message": msg})
			return
		}
		if err != nil {
			notice = `<p class="banner">` + html.EscapeString(err.Error()) + `</p>`
		} else {
			notice = `<p class="ok">` + html.EscapeString(msg) + `</p>`
		}
	}
	if dashRedirectQueryNew(w, r) {
		return
	}
	insts := h.instancesFor(sess)
	primaries := dashWritablePrimaries(insts)
	var b strings.Builder
	b.WriteString(notice)
	b.WriteString(`<div class="tab-toolbar is-spread"><p class="hint">Logical databases on this Postgres server (<code>CREATE DATABASE</code>), not a new Flynn resource. Followers copy every database on the primary.</p>`)
	if len(primaries) > 0 {
		b.WriteString(`<div class="tab-toolbar-actions"><a class="btn btn-sm" href="databases/new">Create database</a></div>`)
	}
	b.WriteString(`</div><div class="card table-card"><table><tr><th>Database</th><th>Instance</th><th>Role</th><th>Host</th></tr>`)
	rows := 0
	for _, inst := range insts {
		role := string(inst.Role)
		if role == "" {
			role = string(postgres.RolePrimary)
		}
		host := dashInstanceHost(inst)
		dbs := inst.Databases
		if len(dbs) == 0 {
			fmt.Fprintf(&b, `<tr><td class="muted">—</td><td><code>%s</code></td><td>%s</td><td><code>%s</code></td></tr>`, html.EscapeString(inst.App), html.EscapeString(role), html.EscapeString(host))
			rows++
			continue
		}
		for _, db := range dbs {
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td><code>%s</code></td><td>%s</td><td><code>%s</code></td></tr>`, html.EscapeString(db.Name), html.EscapeString(inst.App), html.EscapeString(role), html.EscapeString(host))
			rows++
		}
	}
	if rows == 0 {
		b.WriteString(`<tr><td colspan="4" class="muted">No Postgres instance is attached yet.</td></tr>`)
	}
	b.WriteString(`</table></div><p class="muted">Same as <code>flynn pg create &lt;name&gt;</code>. Create new databases on the primary only.</p>`)
	if dashNewPanel(r) && len(primaries) > 0 {
		b.WriteString(h.dashCreateDatabasePanel(primaries))
	}
	writeDash(w, sess, "Databases", b.String())
}

func dashWritablePrimaries(insts []*postgres.Instance) []*postgres.Instance {
	var primaries []*postgres.Instance
	for _, inst := range insts {
		if inst == nil || inst.Role == postgres.RoleFollower || inst.ReadOnly {
			continue
		}
		primaries = append(primaries, inst)
	}
	return primaries
}

func dashInstanceHost(inst *postgres.Instance) string {
	if inst == nil {
		return "—"
	}
	if host := strings.TrimSpace(inst.ServiceHost); host != "" {
		return host
	}
	return "—"
}

func (h *handler) dashCreateDatabasePanel(primaries []*postgres.Instance) string {
	if len(primaries) == 0 {
		return ""
	}
	return dashFormPanel(
		"postgres-db-panel",
		"Create database",
		"A logical database on this Postgres server, not a new Flynn resource.",
		"databases",
		"postgres-db-form",
		"..",
		"Create",
		h.dashCreateDatabaseFields(primaries),
	)
}

func (h *handler) dashCreateDatabaseFields(primaries []*postgres.Instance) string {
	var b strings.Builder
	if len(primaries) > 1 {
		b.WriteString(`<label>Instance<select name="instance">`)
		for _, inst := range primaries {
			ref := instanceRef(inst)
			label := inst.App
			if label == "" {
				label = ref
			}
			fmt.Fprintf(&b, `<option value="%s">%s</option>`, html.EscapeString(ref), html.EscapeString(label))
		}
		b.WriteString(`</select></label>`)
	} else if len(primaries) == 1 {
		fmt.Fprintf(&b, `<input type="hidden" name="instance" value="%s">`, html.EscapeString(instanceRef(primaries[0])))
	}
	b.WriteString(`<label for="logical-db-name">Database name</label><input id="logical-db-name" name="name" required pattern="[A-Za-z_][A-Za-z0-9_]{0,62}" placeholder="shop_analytics" autocomplete="off">`)
	return b.String()
}

func dashFormPanel(id, title, hint, closeHref, formID, action, submitLabel, fields string) string {
	return fmt.Sprintf(`<div class="side-panel-layer" id="%s" data-close="%s">
<a class="side-panel-backdrop" href="%s" aria-label="Close panel">Close</a>
<div class="side-panel" role="dialog" aria-modal="true" aria-labelledby="%s-title">
<div class="side-panel-head">
<div class="side-panel-title">
<h2 id="%s-title">%s</h2>
<p class="hint">%s</p>
</div>
<a class="btn btn-ghost btn-sm" href="%s">Close</a>
</div>
<div class="side-panel-body">
<form id="%s" class="form-stack" method="post" action="%s">
%s
</form>
</div>
<div class="side-panel-foot">
<a class="btn btn-ghost btn-sm" href="%s">Cancel</a>
<button type="submit" form="%s" class="btn btn-sm primary">%s</button>
</div>
</div>
</div>`,
		html.EscapeString(id), html.EscapeString(closeHref),
		html.EscapeString(closeHref), html.EscapeString(id),
		html.EscapeString(id), html.EscapeString(title), html.EscapeString(hint),
		html.EscapeString(closeHref),
		html.EscapeString(formID), html.EscapeString(action), fields,
		html.EscapeString(closeHref), html.EscapeString(formID), html.EscapeString(submitLabel),
	)
}

func (h *handler) dashUsers(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	notice := ""
	if r.Method == http.MethodPost {
		msg, err := h.createUserFromRequest(r, sess)
		if wantsJSON(r) {
			if err != nil {
				writeAPIError(w, err)
				return
			}
			dashui.WriteJSON(w, 200, map[string]string{"status": "ok", "message": msg})
			return
		}
		if err != nil {
			notice = `<p class="banner">` + html.EscapeString(err.Error()) + `</p>`
		} else {
			notice = `<p class="ok">` + html.EscapeString(msg) + `</p>`
		}
	}
	users := h.collectUsers(sess)
	var b strings.Builder
	b.WriteString(notice)
	b.WriteString(`<div class="card"><h2>Users</h2>`)
	b.WriteString(`<p>Application logins on this Postgres cluster. Followers share the primary's roles, so each user is listed once. The primary PGUSER is listed; replica connection users and the platform superuser are not.</p>`)
	b.WriteString(h.dashCreateUserForm(h.instancesFor(sess)))
	b.WriteString(`<table><tr><th>User</th><th>Database</th><th>Instance</th></tr>`)
	if len(users) == 0 {
		b.WriteString(`<tr><td colspan="3" class="muted">No application users yet.</td></tr>`)
	}
	for _, u := range users {
		fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td><code>%s</code></td><td><code>%s</code></td></tr>`,
			html.EscapeString(u.Name), html.EscapeString(u.Database), html.EscapeString(u.Instance))
	}
	b.WriteString(`</table></div>`)
	writeDash(w, sess, "Users", b.String())
}

func (h *handler) dashCreateUserForm(insts []*postgres.Instance) string {
	var primaries []*postgres.Instance
	for _, inst := range insts {
		if inst == nil || inst.Role == postgres.RoleFollower || inst.ReadOnly {
			continue
		}
		primaries = append(primaries, inst)
	}
	if len(primaries) == 0 {
		return `<p class="muted">Attach a primary Postgres resource before adding a user.</p>`
	}
	var dbs []string
	seen := map[string]bool{}
	for _, inst := range primaries {
		for _, db := range inst.Databases {
			if db.Name == "" || seen[db.Name] {
				continue
			}
			seen[db.Name] = true
			dbs = append(dbs, db.Name)
		}
	}
	var b strings.Builder
	b.WriteString(`<form method="post" action="api/users" class="stack" style="margin:0 0 1rem">`)
	if len(primaries) > 1 {
		b.WriteString(`<label>Instance<select name="instance">`)
		for _, inst := range primaries {
			ref := instanceRef(inst)
			label := inst.App
			if label == "" {
				label = ref
			}
			fmt.Fprintf(&b, `<option value="%s">%s</option>`, html.EscapeString(ref), html.EscapeString(label))
		}
		b.WriteString(`</select></label>`)
	} else {
		fmt.Fprintf(&b, `<input type="hidden" name="instance" value="%s">`, html.EscapeString(instanceRef(primaries[0])))
	}
	b.WriteString(`<label>Username<input name="name" required pattern="[A-Za-z_][A-Za-z0-9_]{0,62}" placeholder="app_reader" autocomplete="off"></label>`)
	b.WriteString(`<label>Password<input name="password" type="password" required autocomplete="new-password"></label>`)
	if len(dbs) > 0 {
		b.WriteString(`<label>Database<select name="database">`)
		for _, db := range dbs {
			fmt.Fprintf(&b, `<option value="%s">%s</option>`, html.EscapeString(db), html.EscapeString(db))
		}
		b.WriteString(`</select></label>`)
	} else {
		b.WriteString(`<label>Database<input name="database" required pattern="[A-Za-z_][A-Za-z0-9_]{0,62}" placeholder="appdb"></label>`)
	}
	b.WriteString(`<button class="primary" type="submit">Create user</button>`)
	b.WriteString(`</form>`)
	return b.String()
}

type userView struct {
	Name     string `json:"name"`
	Database string `json:"database,omitempty"`
	Instance string `json:"instance,omitempty"`
}

func isReplicaInstance(inst *postgres.Instance) bool {
	return inst != nil && (inst.Role == postgres.RoleFollower || inst.ReadOnly)
}

func (h *handler) collectUsers(sess *dashui.Session) []userView {
	insts := append([]*postgres.Instance(nil), h.instancesFor(sess)...)
	sort.SliceStable(insts, func(i, j int) bool {
		ri, rj := isReplicaInstance(insts[i]), isReplicaInstance(insts[j])
		if ri == rj {
			return false
		}
		return !ri && rj
	})
	var out []userView
	seen := map[string]int{}
	add := func(u userView) {
		u.Name = strings.TrimSpace(u.Name)
		if u.Name == "" {
			return
		}
		key := strings.ToLower(u.Name)
		if i, ok := seen[key]; ok {
			if strings.TrimSpace(out[i].Database) == "" && strings.TrimSpace(u.Database) != "" {
				out[i].Database = u.Database
			}
			if strings.TrimSpace(out[i].Instance) == "" && strings.TrimSpace(u.Instance) != "" {
				out[i].Instance = u.Instance
			}
			return
		}
		seen[key] = len(out)
		out = append(out, u)
	}
	for _, inst := range insts {
		if inst == nil {
			continue
		}
		db := ""
		if len(inst.Databases) > 0 {
			db = inst.Databases[0].Name
		}
		ref := instanceRef(inst)
		replica := isReplicaInstance(inst)
		if inst.AppUser != "" && !replica {
			add(userView{Name: inst.AppUser, Database: db, Instance: inst.App})
		}
		if h.live() {
			for _, u := range listUsersOnInstance(inst) {
				if replica && inst.AppUser != "" && strings.EqualFold(u.Name, inst.AppUser) {
					continue
				}
				add(userView{Name: u.Name, Database: u.Database, Instance: inst.App})
			}
		}
		if h.store != nil {
			seenKeys := map[string]bool{}
			for _, key := range []string{ref, inst.App, inst.ID} {
				key = strings.TrimSpace(key)
				if key == "" || seenKeys[key] {
					continue
				}
				seenKeys[key] = true
				stored, err := h.store.Users(key)
				if err != nil {
					continue
				}
				for _, u := range stored {
					if replica && inst.AppUser != "" && strings.EqualFold(u.Name, inst.AppUser) {
						continue
					}
					add(userView{Name: u.Name, Database: firstNonEmpty(u.Database, db), Instance: inst.App})
				}
			}
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (h *handler) dashListUsers(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	out := h.collectUsers(sess)
	if out == nil {
		out = []userView{}
	}
	dashui.WriteJSON(w, 200, out)
}

func (h *handler) dashCreateUser(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	msg, err := h.createUserFromRequest(r, sess)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if wantsJSON(r) {
		dashui.WriteJSON(w, 200, map[string]string{"status": "ok", "message": msg})
		return
	}
	http.Redirect(w, r, "users", http.StatusSeeOther)
}

func (h *handler) dashDropUser(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	name, _, database, id, err := h.readUserBody(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if id == "" {
		id = h.primaryRef(sess)
	}
	if id == "" {
		writeAPIError(w, fmt.Errorf("choose a primary instance"))
		return
	}
	if err := h.dropUserOnInstance(id, name, database); err != nil {
		writeAPIError(w, err)
		return
	}
	if wantsJSON(r) {
		dashui.WriteJSON(w, 200, map[string]string{"status": "ok", "message": "Dropped user " + name})
		return
	}
	http.Redirect(w, r, "users", http.StatusSeeOther)
}

func (h *handler) createUserFromRequest(r *http.Request, sess *dashui.Session) (string, error) {
	name, password, database, id, err := h.readUserBody(r)
	if err != nil {
		return "", err
	}
	if id == "" {
		id = h.primaryRef(sess)
	}
	if id == "" {
		return "", fmt.Errorf("choose a primary instance")
	}
	if err := h.createUserOnInstance(id, name, password, database); err != nil {
		return "", err
	}
	return "Created user " + name, nil
}

func (h *handler) readUserBody(r *http.Request) (name, password, database, instance string, err error) {
	if r != nil && strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") {
		var body struct {
			Name     string `json:"name"`
			Password string `json:"password"`
			Database string `json:"database"`
			Instance string `json:"instance"`
		}
		if err := decode(r, &body); err != nil {
			return "", "", "", "", err
		}
		return strings.TrimSpace(body.Name), body.Password, strings.TrimSpace(body.Database), strings.TrimSpace(body.Instance), nil
	}
	if r != nil {
		_ = r.ParseForm()
		return strings.TrimSpace(r.FormValue("name")), r.FormValue("password"), strings.TrimSpace(r.FormValue("database")), strings.TrimSpace(r.FormValue("instance")), nil
	}
	return "", "", "", "", fmt.Errorf("missing request")
}

func (h *handler) dashBackup(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	insts := h.instancesFor(sess)
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Backup</h2>`)
	b.WriteString(`<p>Download a custom-format dump of this instance, or restore one in place. Restore replaces data in the selected database and cannot be undone. This is not a Flynn cluster backup and does not include other apps.</p>`)
	if len(insts) == 0 {
		b.WriteString(`<p class="muted">Attach a Postgres resource before taking a backup.</p></div>`)
		writeDash(w, sess, "Backup", b.String())
		return
	}
	b.WriteString(`<div class="row" style="align-items:flex-start">`)
	b.WriteString(`<div style="flex:1;min-width:16rem"><h3 style="font-size:.85rem;margin:0 0 .4rem">Download</h3>`)
	b.WriteString(`<p class="muted">pg_dump custom format of the current database. Same as <code>flynn pg dump -f postgres.dump</code>.</p>`)
	b.WriteString(`<div class="row"><button id="dump" class="primary" type="button">Download dump</button></div></div>`)
	b.WriteString(`<div style="flex:1;min-width:16rem"><h3 style="font-size:.85rem;margin:0 0 .4rem">Restore</h3>`)
	b.WriteString(`<p class="muted">Restore a dump taken from this instance. Same as <code>flynn pg restore -f postgres.dump</code>.</p>`)
	b.WriteString(`<label>Dump file<input id="file" type="file" accept=".dump,.backup,.sql"></label>`)
	b.WriteString(`<div class="row"><button id="restore" class="danger" type="button">Restore dump</button></div></div>`)
	b.WriteString(`</div><pre id="out" class="muted" style="margin-top:1rem"></pre>`)
	b.WriteString(`<ul class="muted">`)
	for _, inst := range insts {
		fmt.Fprintf(&b, `<li>Instance <code>%s</code>`, html.EscapeString(inst.App))
		if inst.Volume != "" {
			fmt.Fprintf(&b, ` · volume <code>%s</code>`, html.EscapeString(inst.Volume))
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></div>
<script>
document.getElementById('dump').onclick=async()=>{
  const out=document.getElementById('out'); out.textContent='Preparing dump…';
  const r=await fetch('api/dump');
  if(!r.ok){ out.textContent=await r.text(); return; }
  const blob=await r.blob();
  const a=document.createElement('a'); a.href=URL.createObjectURL(blob); a.download='postgres.dump'; a.click();
  URL.revokeObjectURL(a.href); out.textContent='Dump downloaded.';
};
document.getElementById('restore').onclick=async()=>{
  const f=document.getElementById('file').files[0];
  const out=document.getElementById('out');
  if(!f){ out.textContent='Choose a dump file to restore.'; return; }
  if(!confirm('Restore '+f.name+'? This replaces existing data on this instance.')) return;
  out.textContent='Restoring…';
  const body=new FormData(); body.append('file', f); body.append('dump', f);
  const r=await fetch('api/restore',{method:'POST',body});
  out.textContent=r.ok ? 'Restore complete.' : await r.text();
};
</script>`)
	writeDash(w, sess, "Backup", b.String())
}

func (h *handler) dashReplication(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	notice := ""
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		msg, err := h.dashReplicationResult(r, sess)
		if wantsJSON(r) {
			if err != nil {
				writeAPIError(w, err)
				return
			}
			dashui.WriteJSON(w, 200, map[string]string{"status": "ok", "message": msg})
			return
		}
		if err != nil {
			notice = `<p class="banner">` + html.EscapeString(err.Error()) + `</p>`
		} else {
			notice = `<p class="ok">` + html.EscapeString(msg) + `</p>`
		}
	}
	if dashRedirectQueryNew(w, r) {
		return
	}
	insts := h.instancesFor(sess)
	var b strings.Builder
	b.WriteString(notice)
	b.WriteString(`<div class="tab-toolbar is-spread"><p class="hint">A follower is a separate read-only resource on another node. Wait until lag is zero, then promote it or unfollow.</p>`)
	b.WriteString(`<div class="tab-toolbar-actions">`)
	if followable := h.dashFollowablePrimaries(insts); len(followable) > 0 {
		b.WriteString(`<a class="btn btn-sm" href="replication/new">Add follower</a>`)
	}
	if primaries := dashWritablePrimaries(insts); len(primaries) > 0 {
		b.WriteString(primaryActionForms(instanceRef(primaries[0]), h.store))
	}
	b.WriteString(`</div></div><div class="card table-card"><table><tr><th>Instance</th><th>Role</th><th>Follows</th><th>Auto-failover</th><th>Status</th><th>Host</th><th></th></tr>`)
	rows := 0
	for _, inst := range insts {
		view := h.replicationView(inst)
		if inst == nil {
			continue
		}
		role := view.Role
		if role == "" {
			role = string(postgres.RolePrimary)
		}
		follows := ""
		if strings.EqualFold(role, string(postgres.RoleFollower)) {
			follows = firstNonEmpty(view.Follows, view.Leader)
		}
		auto := "—"
		if view.AutoFailover {
			auto = "yes"
		}
		if view.ReplicaPending {
			auto = "replica pending"
		}
		status := `<span class="muted">—</span>`
		if strings.EqualFold(role, string(postgres.RoleFollower)) {
			status = `<span class="muted">Starting…</span>`
			ref := instanceRef(inst)
			if h.store != nil {
				if p, err := h.store.Progress(ref); err == nil {
					if p.Ready || p.Available {
						status = `<span class="muted">` + html.EscapeString(firstNonEmpty(p.Message, postgres.FormatProgress(p))) + `</span>`
					} else {
						status = fmt.Sprintf(`<div class="replica-progress"><progress max="100" value="%d" aria-label="%s"></progress><span class="replica-progress-label">%s</span></div>`,
							p.Percent, html.EscapeString(p.Message), html.EscapeString(p.Message))
					}
				}
			}
		}
		if view.ReplicaPending {
			status = `<span class="muted">Replica pending: waiting for another node</span>`
		}
		fmt.Fprintf(&b, `<tr data-instance="%s"><td><code>%s</code></td><td>%s</td><td>%s</td><td>%s</td><td class="replica-status">%s</td><td><code>%s</code></td><td>%s</td></tr>`,
			html.EscapeString(instanceRef(inst)),
			html.EscapeString(view.App), html.EscapeString(role), html.EscapeString(follows), html.EscapeString(auto), status, html.EscapeString(dashInstanceHost(inst)), view.Actions)
		rows++
	}
	if rows == 0 {
		b.WriteString(`<tr><td colspan="7" class="muted">This database has no instances yet.</td></tr>`)
	}
	b.WriteString(`</table></div><p class="muted">Same as <code>flynn resource:add postgres --follow &lt;instance&gt;</code> (always streaming) and <code>flynn pg:upgrade</code>. Copy progress updates live on this page and on <code>flynn pg:wait</code>.</p>`)
	if dashNewPanel(r) {
		if followable := h.dashFollowablePrimaries(insts); len(followable) > 0 {
			b.WriteString(h.dashAddFollowerPanel(followable))
		}
	}
	b.WriteString(dashProgressScript())
	writeDash(w, sess, "Followers", b.String())
}

func (h *handler) dashSettings(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	notice := ""
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		msg, err := h.dashSettingsResult(sess, r)
		if wantsJSON(r) {
			if err != nil {
				writeAPIError(w, err)
				return
			}
			dashui.WriteJSON(w, 200, map[string]string{"status": "ok", "message": msg})
			return
		}
		if err != nil {
			notice = `<p class="banner">` + html.EscapeString(err.Error()) + `</p>`
		} else {
			notice = `<p class="ok">` + html.EscapeString(msg) + `</p>`
		}
	}
	insts := h.instancesFor(sess)
	var b strings.Builder
	b.WriteString(notice)
	b.WriteString(`<div class="card"><h2>Settings</h2>`)
	b.WriteString(`<p>Delete this Postgres resource. A follower can always be deleted. A primary can be deleted only when it has no followers.</p>`)
	if len(insts) == 0 {
		b.WriteString(`<p class="muted">No Postgres resource is attached to this app.</p></div>`)
		writeDash(w, sess, "Settings", b.String())
		return
	}
	b.WriteString(`<table><tr><th>Instance</th><th>Role</th><th>Followers</th><th></th></tr>`)
	for _, inst := range insts {
		ref := instanceRef(inst)
		role := string(inst.Role)
		if role == "" {
			role = string(postgres.RolePrimary)
		}
		followers := h.followerApps(inst)
		blocked := postgres.DeleteBlockedBy(inst, followers)
		fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td>`, html.EscapeString(ref), html.EscapeString(role))
		if len(followers) == 0 {
			b.WriteString(`—`)
		} else {
			b.WriteString(`<code>` + html.EscapeString(strings.Join(followers, ", ")) + `</code>`)
		}
		b.WriteString(`</td><td>`)
		if len(blocked) == 0 {
			esc := html.EscapeString(ref)
			b.WriteString(`<form method="post" style="margin:0" onsubmit="return confirm('Delete Postgres resource ` + esc + `? This destroys the instance.')">`)
			b.WriteString(`<input type="hidden" name="action" value="delete"><input type="hidden" name="instance" value="` + esc + `">`)
			b.WriteString(`<button class="danger" type="submit">Delete resource</button></form>`)
		} else {
			b.WriteString(`<button class="danger" type="button" disabled>Delete resource</button>`)
		}
		b.WriteString(`</td></tr>`)
		if len(blocked) > 0 {
			fmt.Fprintf(&b, `<tr><td colspan="4" class="muted">Remove or unfollow %s before deleting this primary.</td></tr>`, html.EscapeString(strings.Join(blocked, ", ")))
		}
	}
	b.WriteString(`</table><p class="muted">Same as <code>flynn resource:remove &lt;instance&gt;</code>.</p></div>`)
	writeDash(w, sess, "Settings", b.String())
}

func (h *handler) dashSettingsResult(sess *dashui.Session, r *http.Request) (string, error) {
	if strings.TrimSpace(r.FormValue("action")) != "delete" {
		return "", fmt.Errorf("unknown settings action")
	}
	inst := h.instanceByRef(sess, r.FormValue("instance"))
	if inst == nil {
		return "", fmt.Errorf("choose a postgres resource to delete")
	}
	if err := h.deleteInstanceResource(sess, inst); err != nil {
		return "", err
	}
	return "Deleted resource " + instanceRef(inst), nil
}

func (h *handler) instanceByRef(sess *dashui.Session, ref string) *postgres.Instance {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	for _, inst := range h.instancesFor(sess) {
		if inst == nil {
			continue
		}
		if strings.EqualFold(instanceRef(inst), ref) || strings.EqualFold(inst.ID, ref) || strings.EqualFold(inst.App, ref) {
			return inst
		}
	}
	if inst := h.instanceFromResourceRef(sess, ref); inst != nil {
		return inst
	}
	if h.store != nil {
		if inst, err := h.store.Get(ref); err == nil {
			return inst
		}
	}
	return nil
}

// instanceFromResourceRef maps a controller resource id onto the isolated
// instance the store already has (instancesFor dedupes by app name, so the
// UUID is otherwise dropped).
func (h *handler) instanceFromResourceRef(sess *dashui.Session, ref string) *postgres.Instance {
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
	if err != nil {
		return nil
	}
	for _, r := range resources {
		if !resourceMatchesInstance(r, ref) {
			continue
		}
		inst := instanceFromResource(r, app)
		if inst == nil {
			continue
		}
		if h.store != nil {
			if existing, err := h.store.Get(firstNonEmptyLocal(inst.App, inst.ID)); err == nil && existing != nil {
				return existing
			}
		}
		return inst
	}
	return nil
}

func (h *handler) deleteInstanceResource(sess *dashui.Session, inst *postgres.Instance) error {
	if err := postgres.CanDeleteResource(inst, h.followerApps(inst)); err != nil {
		return err
	}
	if h.client != nil {
		app := sessApp(sess)
		if sess != nil && strings.TrimSpace(sess.AppID) != "" {
			app = sess.AppID
		}
		p, err := h.client.GetProvider("postgres")
		if err != nil {
			return err
		}
		resources, err := h.appResources(app)
		if err != nil {
			return err
		}
		for _, r := range resources {
			if r == nil {
				continue
			}
			name := ""
			if r.Env != nil {
				name = strings.TrimSpace(r.Env["FLYNN_POSTGRES"])
			}
			if name != inst.App && r.ExternalID != inst.ID && r.ID != inst.ID && name != instanceRef(inst) {
				continue
			}
			_, err = h.client.DeleteResource(p.ID, r.ID)
			h.forgetInstance(inst)
			return err
		}
	}
	h.forgetInstance(inst)
	return nil
}

func (h *handler) forgetInstance(inst *postgres.Instance) {
	if h == nil || h.store == nil || inst == nil {
		return
	}
	h.store.Forget(inst.ID)
	if inst.App != "" && inst.App != inst.ID {
		h.store.Forget(inst.App)
	}
}

func wantsJSON(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json") {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json")
}

func (h *handler) dashReplicationResult(r *http.Request, sess *dashui.Session) (string, error) {
	action := strings.TrimSpace(r.FormValue("action"))
	if action == "" {
		action = "upgrade"
	}
	id := strings.TrimSpace(r.FormValue("instance"))
	if id == "" {
		switch action {
		case "follow":
			return "", fmt.Errorf("choose a primary to follow")
		case "promote", "unfollow", "wait":
			return "", fmt.Errorf("choose a follower")
		default:
			return "", fmt.Errorf("choose a primary to upgrade")
		}
	}
	switch action {
	case "follow":
		if err := h.dashFollow(sess, id, r.FormValue("runtime"), r.FormValue("auto_failover") == "1" || r.FormValue("auto_failover") == "on"); err != nil {
			return "", err
		}
		return "Follower started with streaming replication. It is read-only until you promote or unfollow it.", nil
	case "promote":
		res, err := h.store.Promote(id)
		if err != nil {
			return "", err
		}
		if res != nil {
			if err := h.stampIsolatedRole(res.Promoted); err != nil {
				return "", err
			}
			h.syncResourceEnv(sess, res.Promoted)
			if res.PreviousLeader != nil && res.PreviousLeader.Role == postgres.RoleDeposed {
				_ = h.stampIsolatedRole(res.PreviousLeader)
				_ = h.fenceIsolatedApp(res.PreviousLeader.App)
				h.syncResourceEnv(sess, res.PreviousLeader)
				_ = h.ensureAutoFailoverReplica(res.Promoted)
				return "Follower promoted. It is now the primary. The previous leader is fenced and a replacement replica is created when another node is available.", nil
			}
		}
		return "Follower promoted. It is now the primary. The previous leader remains as its own resource.", nil
	case "unfollow":
		inst, err := h.store.Unfollow(id)
		if err != nil {
			return "", err
		}
		if err := h.stampIsolatedRole(inst); err != nil {
			return "", err
		}
		h.syncResourceEnv(sess, inst)
		return "Replication stopped. This instance is a standalone writable copy.", nil
	case "wait":
		ctx, cancel := timeoutCtx(r)
		defer cancel()
		if err := h.store.Wait(ctx, id); err != nil {
			return "", err
		}
		return "Follower lag is zero.", nil
	default:
		if _, err := h.startUpgrade(id, h.imageRefreshOptions(nil)); err != nil {
			return "", err
		}
		return "Upgrade started. A follower on the current plugin image is promoted, attachments are rewritten, and the old primary is removed.", nil
	}
}

func (h *handler) dashFollow(sess *dashui.Session, leader, runtime string, autoFailover bool) error {
	app := sessApp(sess)
	appRef := app
	if sess != nil && sess.AppID != "" {
		appRef = sess.AppID
	}
	inst := h.resolveFollowLeader(sess, leader)
	if inst == nil {
		return postgres.ErrNotFound
	}
	if inst.Role == postgres.RoleFollower {
		return postgres.ErrFollowFollower
	}
	if inst.Role == postgres.RoleDeposed {
		return fmt.Errorf("cannot add a follower of a deposed primary")
	}
	follow := instanceRef(inst)
	if follow == "" {
		return postgres.ErrNotFound
	}
	_, err := h.provisionFollow(app, appRef, follow, runtime, autoFailover)
	return err
}

// resolveFollowLeader maps a dashboard instance field (app name, plugin id, or
// controller resource id) onto a primary the store can Follow.
func (h *handler) resolveFollowLeader(sess *dashui.Session, ref string) *postgres.Instance {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	inst := h.instanceByRef(sess, ref)
	if inst == nil && h.store != nil {
		inst, _ = h.store.Get(ref)
	}
	if inst == nil {
		return nil
	}
	if h.store != nil {
		if adopted := h.store.Adopt(inst); adopted != nil {
			inst = adopted
		}
	}
	return inst
}

func (h *handler) syncResourceEnv(sess *dashui.Session, inst *postgres.Instance) {
	if h == nil || inst == nil || strings.TrimSpace(inst.App) == "" || h.client == nil {
		return
	}
	app := sessApp(sess)
	if sess != nil && strings.TrimSpace(sess.AppID) != "" {
		app = sess.AppID
	}
	if app == "" {
		app = strings.TrimSpace(inst.Tenant)
	}
	if app == "" && len(inst.Attachments) > 0 {
		app = strings.TrimSpace(inst.Attachments[0].App)
	}
	if app == "" {
		return
	}
	resources, err := h.appResources(app)
	if err != nil {
		return
	}
	for _, r := range resources {
		if r == nil || r.Env == nil {
			continue
		}
		if strings.TrimSpace(r.Env["FLYNN_POSTGRES"]) != inst.App {
			continue
		}
		applyPostgresResourceEnv(inst, r.Env, nil)
		stripTenantPostgresCredentials(r.Env)
		_ = h.client.PutResource(r)
		return
	}
}

func sessApp(sess *dashui.Session) string {
	if sess == nil {
		return ""
	}
	if strings.TrimSpace(sess.AppName) != "" {
		return sess.AppName
	}
	return sess.AppID
}

type replicationView struct {
	App            string
	Role           string
	Leader         string
	Follows        string
	Followers      []string
	Lag            int64
	Runtime        string
	Mode           string
	AutoFailover   bool
	ReplicaPending bool
	Actions        string
}

func (h *handler) replicationView(inst *postgres.Instance) replicationView {
	v := replicationView{
		App:       inst.App,
		Role:      string(inst.Role),
		Leader:    inst.LeaderID,
		Followers: inst.Followers,
		Lag:       inst.LagBytes,
		Runtime:   inst.Runtime,
		Mode:      string(inst.Mode),
	}
	if v.Role == "" {
		v.Role = string(postgres.RolePrimary)
	}
	ref := instanceRef(inst)
	if h.store != nil {
		if info, err := h.store.Info(ref); err == nil {
			v.Role = string(info.Role)
			v.Leader = info.LeaderID
			v.Follows = info.Follows
			v.Followers = info.Followers
			v.Lag = info.LagBytes
			v.Runtime = info.Runtime
			v.Mode = string(info.Mode)
			v.AutoFailover = info.AutoFailover
			v.ReplicaPending = info.ReplicaPending
			if info.App != "" {
				v.App = info.App
			}
		}
	}
	follower := strings.EqualFold(v.Role, string(postgres.RoleFollower))
	if follower {
		v.Actions = followerActionForms(ref, v.AutoFailover)
		return v
	}
	v.Actions = primaryActionForms(ref, h.store)
	return v
}

func instanceRef(inst *postgres.Instance) string {
	if inst == nil {
		return ""
	}
	if inst.App != "" {
		return inst.App
	}
	return inst.ID
}

func primaryActionForms(ref string, store *postgres.Store) string {
	if store != nil {
		if task := store.LatestUpgrade(ref); task != nil && task.Status != postgres.TaskDone && task.Status != postgres.TaskFailed {
			msg := html.EscapeString(task.Status)
			if task.Progress != nil {
				msg = html.EscapeString(formatWaitLine(task.Status, *task.Progress))
			}
			bar := ""
			if task.Progress != nil && !task.Progress.Ready {
				bar = fmt.Sprintf(`<div class="replica-progress"><progress max="100" value="%d" aria-label="%s"></progress></div>`, task.Progress.Percent, msg)
			}
			return `<div class="replica-progress">` + bar + `<span class="pill">` + msg + `</span></div>`
		}
	}
	esc := html.EscapeString(ref)
	return `<form method="post" style="margin:0"><input type="hidden" name="action" value="upgrade"><input type="hidden" name="instance" value="` + esc + `"><button class="primary" type="submit">Upgrade</button></form>`
}

func followerActionForms(ref string, autoFailover bool) string {
	esc := html.EscapeString(ref)
	confirm := "Promote this follower to primary? The previous leader stays as its own resource."
	if autoFailover {
		confirm = "Promote this auto-failover follower to primary? The previous leader is fenced and a replacement replica is created."
	}
	return `<div class="row" style="display:flex;gap:.75rem;flex-wrap:wrap;align-items:center">` +
		`<form method="post" style="margin:0"><input type="hidden" name="action" value="promote"><input type="hidden" name="instance" value="` + esc + `"><button class="primary" type="submit" onclick="return confirm('` + html.EscapeString(confirm) + `')">Promote</button></form>` +
		`<form method="post" style="margin:0"><input type="hidden" name="action" value="unfollow"><input type="hidden" name="instance" value="` + esc + `"><button class="danger" type="submit" onclick="return confirm('Stop replication and leave a standalone writable copy?')">Unfollow</button></form>` +
		`</div>`
}

func (h *handler) dashFollowablePrimaries(insts []*postgres.Instance) []*postgres.Instance {
	var primaries []*postgres.Instance
	for _, inst := range insts {
		if inst == nil {
			continue
		}
		role := inst.Role
		if h.store != nil {
			if info, err := h.store.Info(instanceRef(inst)); err == nil {
				role = info.Role
			}
		}
		if role == postgres.RoleFollower || role == postgres.RoleDeposed {
			continue
		}
		primaries = append(primaries, inst)
	}
	return primaries
}

func (h *handler) dashAddFollowerPanel(primaries []*postgres.Instance) string {
	if len(primaries) == 0 {
		return ""
	}
	return dashFormPanel(
		"postgres-follow-panel",
		"Add follower",
		"A separate read-only replica. Choose options, then add it.",
		"replication",
		"add-follower-form",
		"..",
		"Add follower",
		dashAddFollowerFields(primaries),
	)
}

func dashAddFollowerFields(primaries []*postgres.Instance) string {
	var b strings.Builder
	b.WriteString(`<input type="hidden" name="action" value="follow">`)
	if len(primaries) == 1 {
		fmt.Fprintf(&b, `<input type="hidden" name="instance" value="%s">`, html.EscapeString(instanceRef(primaries[0])))
	} else {
		b.WriteString(`<label for="follow-primary">Primary</label><select id="follow-primary" name="instance">`)
		for _, inst := range primaries {
			ref := instanceRef(inst)
			label := inst.App
			if label == "" {
				label = ref
			}
			fmt.Fprintf(&b, `<option value="%s">%s</option>`, html.EscapeString(ref), html.EscapeString(label))
		}
		b.WriteString(`</select>`)
	}
	b.WriteString(`<label><input type="checkbox" name="auto_failover" value="1"> Automatic failover</label>`)
	b.WriteString(`<p class="hint">Place the replica on another host and promote it if the primary job is lost. Needs two or more live nodes. A replacement replica is then created to keep the pair.</p>`)
	return b.String()
}

func dashProgressScript() string {
	return `<script>
(function(){
  async function tick(){
    try {
      const r = await fetch('api/progress', {headers:{Accept:'application/json'}, credentials:'same-origin'});
      if(!r.ok) return;
      const data = await r.json();
      const items = data.followers || [];
      document.querySelectorAll('tr[data-instance]').forEach(row => {
        const inst = row.getAttribute('data-instance');
        const cell = row.querySelector('.replica-status');
        if(!cell || !inst) return;
        const p = items.find(x => (x.follower||'').toLowerCase() === inst.toLowerCase());
        if(!p) return;
        if(p.ready){
          cell.innerHTML = '<span class="muted">'+(p.message||'ready')+'</span>';
          return;
        }
        const msg = p.message || (p.percent+'%');
        cell.innerHTML = '<div class="replica-progress"><progress max="100" value="'+(p.percent||0)+'" aria-label="'+msg.replace(/"/g,'')+'"></progress><span class="replica-progress-label">'+msg.replace(/</g,'')+'</span></div>';
      });
    } catch(e) {}
  }
  tick();
  setInterval(tick, 1000);
})();
</script>`
}

type logicalDatabaseView struct {
	Name     string `json:"name"`
	Instance string `json:"instance,omitempty"`
	Role     string `json:"role,omitempty"`
}

func (h *handler) dashListDatabases(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	var out []logicalDatabaseView
	for _, inst := range h.instancesFor(sess) {
		if inst == nil {
			continue
		}
		role := string(inst.Role)
		if role == "" {
			role = string(postgres.RolePrimary)
		}
		for _, name := range h.databasesFor(inst) {
			out = append(out, logicalDatabaseView{Name: name, Instance: inst.App, Role: role})
		}
	}
	if out == nil {
		out = []logicalDatabaseView{}
	}
	dashui.WriteJSON(w, 200, out)
}

func (h *handler) databasesFor(inst *postgres.Instance) []string {
	if inst == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	if h.live() {
		for _, name := range listDatabasesOnInstance(inst) {
			add(name)
		}
	}
	for _, db := range inst.Databases {
		add(db.Name)
	}
	if h.store != nil {
		if stored, err := h.store.Get(instanceRef(inst)); err == nil {
			for _, db := range stored.Databases {
				add(db.Name)
			}
		}
	}
	return out
}

func (h *handler) dashCreateDatabase(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	msg, err := h.createDatabaseFromRequest(r, sess)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if wantsJSON(r) {
		dashui.WriteJSON(w, 200, map[string]string{"status": "ok", "message": msg})
		return
	}
	http.Redirect(w, r, "databases", http.StatusSeeOther)
}

func (h *handler) createDatabaseFromRequest(r *http.Request, sess *dashui.Session) (string, error) {
	name := strings.TrimSpace(r.FormValue("name"))
	id := strings.TrimSpace(r.FormValue("instance"))
	if name == "" && strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") {
		var body struct {
			Name     string `json:"name"`
			Instance string `json:"instance"`
		}
		if err := decode(r, &body); err != nil {
			return "", err
		}
		name = strings.TrimSpace(body.Name)
		if id == "" {
			id = strings.TrimSpace(body.Instance)
		}
	}
	if id == "" {
		id = h.primaryRef(sess)
	}
	if id == "" {
		return "", fmt.Errorf("choose a primary instance")
	}
	if err := h.createLogicalDatabase(id, name); err != nil {
		return "", err
	}
	return "Created database " + name, nil
}

func (h *handler) primaryRef(sess *dashui.Session) string {
	for _, inst := range h.instancesFor(sess) {
		if inst == nil || inst.Role == postgres.RoleFollower || inst.ReadOnly {
			continue
		}
		return instanceRef(inst)
	}
	return ""
}

func (h *handler) dumpTarget(sess *dashui.Session) (*postgres.Instance, error) {
	var primary *postgres.Instance
	for _, inst := range h.instancesFor(sess) {
		if inst == nil {
			continue
		}
		if inst.Role != postgres.RoleFollower && !inst.ReadOnly {
			primary = inst
			break
		}
		if primary == nil {
			primary = inst
		}
	}
	if primary == nil {
		return nil, fmt.Errorf("attach a Postgres resource before taking a backup")
	}
	if h.store != nil {
		if got, err := h.store.Get(instanceRef(primary)); err == nil {
			return got, nil
		}
	}
	return primary, nil
}

func (h *handler) dashDump(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	inst, err := h.dumpTarget(sess)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="postgres.dump"`)
	if h.live() {
		if err := runDump(inst.ConnectionURL(), w); err != nil {
			writeAPIError(w, err)
		}
		return
	}
	fmt.Fprintf(w, "-- flynn postgres dump\n-- instance %s\n", inst.App)
	for _, db := range inst.Databases {
		fmt.Fprintf(w, "-- database %s\n", db.Name)
	}
}

func (h *handler) dashRestore(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	inst, err := h.dumpTarget(sess)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	file, _, err := r.FormFile("dump")
	if err != nil {
		file, _, err = r.FormFile("file")
	}
	if err != nil {
		writeAPIError(w, fmt.Errorf("choose a dump file to restore"))
		return
	}
	defer file.Close()
	if h.live() {
		if err := runRestore(inst.ConnectionURL(), file); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	if wantsJSON(r) {
		dashui.WriteJSON(w, 200, map[string]string{"status": "ok", "message": "Restore complete"})
		return
	}
	w.WriteHeader(http.StatusOK)
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
			label := attachmentDisplay(att)
			if label == "" {
				continue
			}
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td><code>%s</code></td></tr>`, html.EscapeString(label), html.EscapeString(maskURL(att.URL)))
		}
		b.WriteString(`</table><p>Attached apps</p><table><tr><th>App</th><th>Attachment</th></tr>`)
		if len(inst.Attachments) == 0 {
			b.WriteString(`<tr><td colspan="2" class="muted">none</td></tr>`)
		}
		for _, att := range inst.Attachments {
			label := attachmentDisplay(att)
			if label == "" {
				label = "—"
			}
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td><code>%s</code></td></tr>`, html.EscapeString(att.App), html.EscapeString(label))
		}
		b.WriteString(`</table><p class="muted">Same as <code>flynn pg:info</code>. Detach with <code>flynn resource:detach postgres</code> from that app, or from the dashboard overview.</p></div>`)
	}
	return b.String()
}

func attachmentDisplay(att postgres.Attachment) string {
	keys := postgres.AttachmentURLKeys(att.Env, att.Env)
	if len(keys) > 0 {
		return strings.Join(keys, ", ")
	}
	as := strings.TrimSpace(att.As)
	if as == "" {
		return ""
	}
	if strings.HasSuffix(as, "_URL") {
		return as
	}
	return as + "_URL"
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
