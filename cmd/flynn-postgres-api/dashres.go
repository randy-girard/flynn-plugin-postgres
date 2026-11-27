package main

import (
	"errors"
	"fmt"
	"strings"

	controller "github.com/randy-girard/flynn/controller/client"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/pkg/resname"
)

// Dashboard resource lifecycle (DATA-08). Provision, Detach and Destroy go
// through the controller like flynn addons:create, addons:detach and
// addons:destroy, so every instance has a resource row (provider, owner app,
// owner account, quota, events) and its env on the app.
//
// The controller calls are made with the dashboard user's own controller
// token (dashUserController), never the plugin's CONTROLLER_KEY, so the
// controller applies its own checks to the user: Manage on the app to
// provision, attach env or unbind, admin of the owner account to delete. The
// plugin also checks the session first, for a clear answer and so a session
// grant never widens what the user token allows:
//   - Provision and Detach need Manage or Admin on the app.
//   - Destroy needs Admin on the app, the owning app, and a typed
//     confirmation.
//   - Detach only unbinds an app attached to another app's resource; the
//     owning app cannot detach, it can only destroy.

// dashResourceClient is the controller client the dashboard lifecycle uses.
type dashResourceClient interface {
	ProvisionResource(req *ct.ResourceReq) (*ct.Resource, error)
	DeleteResource(providerID, resourceID string) (*ct.Resource, error)
	DeleteResourceApp(providerID, resourceID, appID string) (*ct.Resource, error)
	GetAppRelease(appID string) (*ct.Release, error)
	CreateRelease(appID string, release *ct.Release) error
	SetAppRelease(appID, releaseID string) error
	CreateDeployment(appID, releaseID string) (*ct.Deployment, error)
}

var (
	errDashNeedsManage = errors.New("permission denied: Provision and Detach need Manage or Admin on this app")
	errDashNeedsAdmin  = errors.New("permission denied: Destroy needs Admin on this app")
	errDashOwnerDetach = errors.New("this app owns the resource, so Detach does not apply; Destroy deletes it for every attached app")
	errDashNotOwner    = errors.New("this app is attached to the resource but does not own it; Detach unbinds it, only the owning app can Destroy it")
	errDashConfirm     = errors.New("Destroy needs confirm set to the resource name")
	errDashNoClient    = errors.New("controller client is not configured")
	errDashNoToken     = errors.New("the dashboard session has no controller token; reload the dashboard and sign in again")
)

// dashControllerURL is the controller the user's client calls; empty means
// http://controller.discoverd.
var dashControllerURL = ""

// dashUserController is the controller client acting as the dashboard user
// whose token this is. Tests replace it.
var dashUserController = func(token string) (dashResourceClient, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errDashNoToken
	}
	return controller.NewClientWithToken(dashControllerURL, token)
}

// dashCanManage mirrors the controller's requireAppManage on session grants.
func dashCanManage(perms []string) bool {
	for _, p := range perms {
		switch strings.TrimSpace(p) {
		case "*", "cluster:admin", "app:admin", "app:write":
			return true
		}
	}
	return false
}

// dashCanAdmin is Admin on the app (or cluster admin).
func dashCanAdmin(perms []string) bool {
	for _, p := range perms {
		switch strings.TrimSpace(p) {
		case "*", "cluster:admin", "app:admin":
			return true
		}
	}
	return false
}

func dashApp(id, name string) *ct.App {
	return &ct.App{ID: strings.TrimSpace(id), Name: strings.TrimSpace(name)}
}

func dashAppRef(app *ct.App) string {
	if app == nil {
		return ""
	}
	if app.ID != "" {
		return app.ID
	}
	return app.Name
}

// dashProvision creates a resource for appID through the controller (which
// calls this plugin's provider API and stores the resource) and sets its
// attachment env on the app, as flynn addons:create does.
func dashProvision(c dashResourceClient, provider, appID string) (*ct.Resource, error) {
	if c == nil {
		return nil, errDashNoClient
	}
	res, err := c.ProvisionResource(&ct.ResourceReq{ProviderID: provider, Apps: []string{appID}})
	if err != nil {
		return nil, err
	}
	err = dashApplyEnv(c, appID, func(cur map[string]string) map[string]*string {
		out := map[string]*string{}
		for k, v := range resname.MergeAttachment(cur, res.Env, "", true) {
			v := v
			out[k] = &v
		}
		return out
	})
	if err != nil {
		return res, fmt.Errorf("created resource %s, but setting its env on the app failed: %w", res.ID, err)
	}
	return res, nil
}

// dashDetach unbinds app from a resource it is attached to but does not own,
// and removes the attachment env from the app. The resource stays.
func dashDetach(c dashResourceClient, provider string, res *ct.Resource, app *ct.App) error {
	if c == nil {
		return errDashNoClient
	}
	if res.OwnedByApp(app) {
		return errDashOwnerDetach
	}
	if _, err := c.DeleteResourceApp(provider, res.ID, dashAppRef(app)); err != nil {
		return err
	}
	return dashApplyEnv(c, dashAppRef(app), func(cur map[string]string) map[string]*string {
		return dashUnsetAttachment(cur, res.Env)
	})
}

// dashDestroy deletes the resource app owns through the controller (which
// deprovisions it and drops the resource row) once confirm names it, then
// removes the attachment env from the app.
func dashDestroy(c dashResourceClient, provider string, res *ct.Resource, app *ct.App, confirm string, names ...string) error {
	if c == nil {
		return errDashNoClient
	}
	if !res.OwnedByApp(app) {
		return errDashNotOwner
	}
	if !dashConfirmed(confirm, append([]string{res.ID}, names...)...) {
		return errDashConfirm
	}
	if _, err := c.DeleteResource(provider, res.ID); err != nil {
		return err
	}
	return dashApplyEnv(c, dashAppRef(app), func(cur map[string]string) map[string]*string {
		return dashUnsetAttachment(cur, res.Env)
	})
}

// dashUnsetAttachment is resname.UnsetAttachment that also matches the
// trimmed values MergeAttachment copied onto the app (a PEM loses its final
// newline), so FLYNN_<ENGINE>_TRUSTED_CERT does not stay behind.
func dashUnsetAttachment(release, resource map[string]string) map[string]*string {
	out := resname.UnsetAttachment(release, resource)
	trimmed := make(map[string]string, len(resource))
	for k, v := range resource {
		trimmed[k] = strings.TrimSpace(v)
	}
	for k, v := range resname.UnsetAttachment(release, trimmed) {
		out[k] = v
	}
	return out
}

// dashConfirmed is true when confirm equals one of names (case-insensitive).
func dashConfirmed(confirm string, names ...string) bool {
	confirm = strings.TrimSpace(confirm)
	if confirm == "" {
		return false
	}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" && strings.EqualFold(n, confirm) {
			return true
		}
	}
	return false
}

// dashApplyEnv mirrors flynn env:set: a new release with change applied,
// made current with SetAppRelease when it has no artifacts (no jobs start),
// deployed otherwise. An empty change does nothing.
func dashApplyEnv(c dashResourceClient, appID string, change func(map[string]string) map[string]*string) error {
	rel, err := c.GetAppRelease(appID)
	if err != nil && !errors.Is(err, controller.ErrNotFound) {
		return err
	}
	if rel == nil {
		rel = &ct.Release{}
	}
	diff := change(rel.Env)
	if len(diff) == 0 {
		return nil
	}
	next := *rel
	next.ID = ""
	next.Env = make(map[string]string, len(rel.Env)+len(diff))
	for k, v := range rel.Env {
		next.Env[k] = v
	}
	for k, v := range diff {
		if v == nil {
			delete(next.Env, k)
		} else {
			next.Env[k] = *v
		}
	}
	if err := c.CreateRelease(appID, &next); err != nil {
		return err
	}
	if len(next.ArtifactIDs) == 0 {
		return c.SetAppRelease(appID, next.ID)
	}
	_, err = c.CreateDeployment(appID, next.ID)
	return err
}
