package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

type tenantDeployClient interface {
	GetApp(string) (*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
	CreateRelease(string, *ct.Release) error
	DeployAppRelease(string, string, <-chan struct{}) error
}

type tenantHeadReleaseClient interface {
	DeploymentList(string) ([]*ct.Deployment, error)
	GetRelease(string) (*ct.Release, error)
}

// deployTenantPostgresEnv forks the attached app's release with URLs pointing
// at the promoted primary, CreateDeployment, and waits until that deploy
// finishes. Isolated datastore apps are skipped. The old primary stays up
// until this returns, then DropPrevious can delete it.
func deployTenantPostgresEnv(c tenantDeployClient, appRef string, previous, promoted *postgres.Instance) error {
	if c == nil || promoted == nil || strings.TrimSpace(appRef) == "" {
		return nil
	}
	app, err := c.GetApp(appRef)
	if err != nil || app == nil {
		return err
	}
	if skipTenantPostgresDeploy(app, previous, promoted) {
		return nil
	}
	rel, err := tenantHeadRelease(c, app.ID)
	if err != nil || rel == nil {
		return err
	}
	next := *rel
	next.ID = ""
	next.Env = cloneEnv(rel.Env)
	next.Processes = cloneProcesses(rel.Processes)
	rewriteReleasePostgresURLs(&next, previous, promoted)
	if envEqual(rel.Env, next.Env) && processesEnvEqual(rel.Processes, next.Processes) {
		return nil
	}
	stampSystemDeployMeta(&next, postgresCutoverNote(previous, promoted))
	if err := c.CreateRelease(app.ID, &next); err != nil {
		return err
	}
	stop := make(chan struct{})
	timer := time.AfterFunc(instanceReadyTimeout, func() { close(stop) })
	defer timer.Stop()
	if err := c.DeployAppRelease(app.ID, next.ID, stop); err != nil {
		return fmt.Errorf("deploy %s onto %s: %w", app.Name, promoted.App, err)
	}
	return nil
}

const (
	metaSystemDeploy     = "flynn-system-deploy"
	metaSystemDeployNote = "flynn-system-deploy.note"
)

func stampSystemDeployMeta(rel *ct.Release, note string) {
	if rel == nil {
		return
	}
	rel.Meta = cloneEnv(rel.Meta)
	rel.Meta[metaSystemDeploy] = "true"
	if n := strings.TrimSpace(note); n != "" {
		rel.Meta[metaSystemDeployNote] = n
	}
}

func postgresCutoverNote(previous, promoted *postgres.Instance) string {
	to, from := "", ""
	if promoted != nil {
		to = strings.TrimSpace(promoted.App)
	}
	if previous != nil {
		from = strings.TrimSpace(previous.App)
	}
	switch {
	case to != "" && from != "":
		return fmt.Sprintf("System deployment: Flynn moved Postgres from %s to %s and updated connection URLs. This is not a git push.", from, to)
	case to != "":
		return fmt.Sprintf("System deployment: Flynn moved Postgres to %s and updated connection URLs. This is not a git push.", to)
	default:
		return "System deployment: Flynn updated this app's Postgres connection URLs after a primary cutover. This is not a git push."
	}
}

func skipTenantPostgresDeploy(app *ct.App, previous, promoted *postgres.Instance) bool {
	if app == nil {
		return true
	}
	name := strings.TrimSpace(app.Name)
	if postgres.IsolatedInstanceApp(name) {
		return true
	}
	if app.Meta["flynn-datastore"] == "true" {
		return true
	}
	if promoted != nil && (name == promoted.App || app.ID == promoted.App) {
		return true
	}
	if previous != nil && (name == previous.App || app.ID == previous.App) {
		return true
	}
	return false
}

func tenantHeadRelease(c tenantDeployClient, appID string) (*ct.Release, error) {
	if h, ok := c.(tenantHeadReleaseClient); ok {
		if list, err := h.DeploymentList(appID); err == nil {
			live, _ := c.GetAppRelease(appID)
			liveID := ""
			if live != nil {
				liveID = live.ID
			}
			if id := headReleaseID(liveID, list); id != "" && id != liveID {
				if rel, err := h.GetRelease(id); err == nil && rel != nil {
					return rel, nil
				}
			}
		}
	}
	return c.GetAppRelease(appID)
}

func headReleaseID(liveID string, deps []*ct.Deployment) string {
	for _, d := range deps {
		if d == nil || d.FinishedAt != nil {
			continue
		}
		if id := strings.TrimSpace(d.NewReleaseID); id != "" {
			return id
		}
	}
	return strings.TrimSpace(liveID)
}

func rewriteReleasePostgresURLs(rel *ct.Release, previous, promoted *postgres.Instance) {
	if rel == nil {
		return
	}
	if rel.Env == nil {
		rel.Env = map[string]string{}
	}
	postgres.RewriteAttachmentEnv(rel.Env, previous, promoted)
	applyPostgresResourceEnv(promoted, rel.Env, nil)
	stripTenantPostgresCredentials(rel.Env)
	for name, p := range rel.Processes {
		if p.Env == nil {
			continue
		}
		postgres.RewriteAttachmentEnv(p.Env, previous, promoted)
		rel.Processes[name] = p
	}
}

func cloneEnv(env map[string]string) map[string]string {
	if env == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

func cloneProcesses(in map[string]ct.ProcessType) map[string]ct.ProcessType {
	if in == nil {
		return nil
	}
	out := make(map[string]ct.ProcessType, len(in))
	for k, v := range in {
		p := v
		if v.Env != nil {
			p.Env = cloneEnv(v.Env)
		}
		out[k] = p
	}
	return out
}

func processesEnvEqual(a, b map[string]ct.ProcessType) bool {
	if len(a) != len(b) {
		return false
	}
	for k, pa := range a {
		pb, ok := b[k]
		if !ok || !envEqual(pa.Env, pb.Env) {
			return false
		}
	}
	return true
}
