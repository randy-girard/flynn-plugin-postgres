package main

import (
	"errors"
	"net/http"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	ct "github.com/randy-girard/flynn/controller/types"
)

// Dashboard Destroy and Detach on the Settings page (DATA-66 for postgres).
// The rules and the controller calls are the shared dashres.go (DATA-08, the
// same file as the mysql, mongodb, kafka, clickhouse and redis plugins). This
// file maps them onto the postgres dashboard: the session app's resource for
// an instance and the follower guard before a destroy. Writes go through the
// controller with the dashboard user's token; the plugin's CONTROLLER_KEY
// client is used only for lookups.

var (
	errNoPostgresResource = errors.New("this instance has no controller resource on this app, so there is nothing to detach")
	errDashNoResource     = errors.New("this instance has no controller resource for this app, so the dashboard cannot destroy it; use flynn addons:destroy")
)

// userClient is the controller client acting as the session's user.
func userClient(sess *dashui.Session) (dashResourceClient, error) {
	if sess == nil {
		return nil, errDashNoToken
	}
	return dashUserController(sess.ControllerToken)
}

// dashLifecycleStatus is the HTTP status for a lifecycle error, or 0 when err
// is not one of them.
func dashLifecycleStatus(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errDashNoToken):
		return http.StatusUnauthorized
	case errors.Is(err, errDashNeedsManage), errors.Is(err, errDashNeedsAdmin), errors.Is(err, errDashNotOwner):
		return http.StatusForbidden
	case errors.Is(err, errNotYourInstance), errors.Is(err, errNoPostgresResource):
		return http.StatusNotFound
	case errors.Is(err, errDashConfirm):
		return http.StatusBadRequest
	case errors.Is(err, errDashOwnerDetach), errors.Is(err, errDashNoResource), errors.Is(err, postgres.ErrHasFollowers):
		return http.StatusConflict
	case errors.Is(err, errDashNoClient):
		return http.StatusServiceUnavailable
	}
	return 0
}

// hasController is true when the handler can see controller resources.
func (h *handler) hasController() bool {
	return h != nil && (h.client != nil || h.listResources != nil)
}

// sessionResources are the session app's controller resources, read with the
// plugin's client (a lookup).
func (h *handler) sessionResources(sess *dashui.Session) []*ct.Resource {
	if sess == nil {
		return nil
	}
	resources, err := h.appResources(firstNonEmpty(sess.AppID, sess.AppName))
	if err != nil {
		return nil
	}
	return resources
}

// resourceForInstance is the resource among resources that points at inst.
func resourceForInstance(resources []*ct.Resource, inst *postgres.Instance) *ct.Resource {
	if inst == nil {
		return nil
	}
	for _, res := range resources {
		if res == nil {
			continue
		}
		for _, ref := range []string{inst.App, inst.ID, instanceRef(inst)} {
			if resourceMatchesInstance(res, ref) {
				return res
			}
		}
	}
	return nil
}

// sessionInstanceResource resolves ref among the session app's own instances
// and returns the instance and its resource on the session app (nil if none).
func (h *handler) sessionInstanceResource(sess *dashui.Session, ref string) (*postgres.Instance, *ct.Resource, error) {
	inst := h.ownedInstance(sess, ref)
	if inst == nil {
		return nil, nil, errNotYourInstance
	}
	if !h.hasController() {
		return inst, nil, nil
	}
	resources, err := h.appResources(firstNonEmpty(sess.AppID, sess.AppName))
	if err != nil {
		return nil, nil, err
	}
	return inst, resourceForInstance(resources, inst), nil
}

// detachFromSettings unbinds the session app from a resource it is attached
// to but does not own and removes its env, like flynn addons:detach. It needs
// Manage or Admin; the owner gets errDashOwnerDetach.
func (h *handler) detachFromSettings(sess *dashui.Session, ref string) (string, error) {
	if !dashCanManage(sess.Permissions) {
		return "", errDashNeedsManage
	}
	c, err := userClient(sess)
	if err != nil {
		return "", err
	}
	inst, res, err := h.sessionInstanceResource(sess, ref)
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", errNoPostgresResource
	}
	if err := dashDetach(c, firstNonEmpty(res.ProviderID, "postgres"), res, dashApp(sess.AppID, sess.AppName)); err != nil {
		return "", err
	}
	return instanceRef(inst), nil
}

// destroyFromSettings deletes the session app's own resource through the
// controller, like flynn addons:destroy. It needs Admin, the owning app and
// confirm set to the instance name; a primary with followers is refused. The
// checks run before anything changes.
func (h *handler) destroyFromSettings(sess *dashui.Session, ref, confirm string) (string, error) {
	if !dashCanAdmin(sess.Permissions) {
		return "", errDashNeedsAdmin
	}
	c, err := userClient(sess)
	if err != nil {
		return "", err
	}
	inst, res, err := h.sessionInstanceResource(sess, ref)
	if err != nil {
		return "", err
	}
	app := dashApp(sess.AppID, sess.AppName)
	if res == nil {
		// With a controller every destroyable instance has a resource row;
		// the plugin key never deletes one for the dashboard. Without one
		// (local dev) the session app's own instance is only dropped from
		// memory.
		if h.hasController() {
			return "", errDashNoResource
		}
		if !dashConfirmed(confirm, inst.App, inst.ID) {
			return "", errDashConfirm
		}
		if err := postgres.CanDeleteResource(inst, h.followerApps(inst)); err != nil {
			return "", err
		}
		h.forgetInstance(inst)
		return instanceRef(inst), nil
	}
	if !res.OwnedByApp(app) {
		return "", errDashNotOwner
	}
	names := []string{res.Env["FLYNN_POSTGRES"], inst.App, inst.ID}
	if !dashConfirmed(confirm, append([]string{res.ID}, names...)...) {
		return "", errDashConfirm
	}
	if err := postgres.CanDeleteResource(inst, h.followerApps(inst)); err != nil {
		return "", err
	}
	if err := dashDestroy(c, firstNonEmpty(res.ProviderID, "postgres"), res, app, confirm, names...); err != nil {
		return "", err
	}
	h.forgetInstance(inst)
	return instanceRef(inst), nil
}
