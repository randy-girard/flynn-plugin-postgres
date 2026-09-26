package main

import (
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
)

var dashNav = [][2]string{
	{"./", "Overview"},
	{"databases", "Databases"},
	{"users", "Users"},
	{"backup", "Backup"},
	{"replication", "Follow"},
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
	return h.store.ForApp(sess.AppID)
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
	if len(insts) > 0 {
		status = "ok"
	}
	dashui.WriteJSON(w, 200, dashui.Card{
		Status:   status,
		Attached: len(insts) > 0,
		Summary:  fmt.Sprintf("%d postgres instance(s)", len(insts)),
		EnvCount: envCount(insts, sess.AppID),
		Details:  map[string]string{"tls": "required", "nodes": "1"},
	})
}

func (h *handler) dashDatabases(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>Databases</h2><table><tr><th>Instance</th><th>Database</th></tr>`)
	for _, inst := range h.instancesFor(sess) {
		for _, db := range inst.Databases {
			fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td><code>%s</code></td></tr>`, html.EscapeString(inst.App), html.EscapeString(db.Name))
		}
	}
	b.WriteString(`</table><p class="muted">Databases belong to this app's instance only.</p></div>`)
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
		info, err := h.store.Info(inst.ID)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, `<tr><td><code>%s</code></td><td>%s</td><td><code>%s</code></td><td>%s</td><td>%d</td><td>%s</td><td>%s</td></tr>`,
			html.EscapeString(inst.App), html.EscapeString(string(info.Role)), html.EscapeString(info.LeaderID),
			html.EscapeString(strings.Join(info.Followers, ", ")), info.LagBytes, html.EscapeString(info.Runtime), html.EscapeString(string(info.Mode)))
	}
	b.WriteString(`</table><p class="muted">pg:follow creates the follower. pg:wait blocks until lag is zero. pg:promote rewrites the primary *_URL and leaves the old leader in place. pg:unfollow keeps a writable copy and stops receiving leader writes.</p></div>`)
	writeDash(w, sess, "Follow", b.String())
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

func envCount(insts []*postgres.Instance, app string) int {
	n := 0
	for _, inst := range insts {
		for _, att := range inst.Attachments {
			if att.App == app {
				n++
			}
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
