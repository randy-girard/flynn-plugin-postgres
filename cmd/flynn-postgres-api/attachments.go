package main

import (
	"fmt"
	"strings"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

func (h *handler) controllerAttachmentViews(refs ...string) []postgres.AttachmentInfo {
	r := h.findControllerResource(refs...)
	if r == nil {
		return nil
	}
	return attachmentInfosForResource(r, h.resolveAppName, h.resolveReleaseEnv)
}

func (h *handler) findControllerResource(refs ...string) *ct.Resource {
	if h == nil {
		return nil
	}
	var list []*ct.Resource
	var err error
	if h.listAllResources != nil {
		list, err = h.listAllResources()
	} else if h.client != nil {
		list, err = h.client.ResourceListAll()
	}
	if err != nil || len(list) == 0 {
		return nil
	}
	for _, r := range list {
		for _, ref := range refs {
			if resourceMatchesInstance(r, ref) {
				return r
			}
		}
	}
	return nil
}

func resourceMatchesInstance(r *ct.Resource, ref string) bool {
	ref = strings.TrimSpace(ref)
	if r == nil || ref == "" {
		return false
	}
	if strings.EqualFold(r.ID, ref) || strings.EqualFold(strings.TrimSpace(r.ExternalID), ref) {
		return true
	}
	if r.Env == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Env["FLYNN_POSTGRES"]), ref) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Env[postgres.ResourceIDEnv]), ref)
}

func resourceAppIDs(r *ct.Resource) []string {
	if r == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range r.Apps {
		add(id)
	}
	add(r.OwnerApp)
	return out
}

func attachmentInfosForResource(r *ct.Resource, nameOf func(string) string, envOf func(string) map[string]string) []postgres.AttachmentInfo {
	if r == nil {
		return nil
	}
	if nameOf == nil {
		nameOf = func(id string) string { return id }
	}
	if envOf == nil {
		envOf = func(string) map[string]string { return nil }
	}
	ids := resourceAppIDs(r)
	out := make([]postgres.AttachmentInfo, 0, len(ids))
	for _, id := range ids {
		name := strings.TrimSpace(nameOf(id))
		if name == "" {
			name = id
		}
		keys := postgres.AttachmentURLKeys(envOf(id), r.Env)
		out = append(out, postgres.AttachmentInfo{
			App:   name,
			ID:    id,
			As:    postgres.AttachmentName(keys),
			Keys:  keys,
			Owner: r.OwnedByApp(&ct.App{ID: id, Name: name}),
		})
	}
	return out
}

func (h *handler) resolveAppName(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if h != nil && h.appDisplayName != nil {
		if name := strings.TrimSpace(h.appDisplayName(id)); name != "" {
			return name
		}
	}
	if h == nil || h.client == nil {
		return id
	}
	app, err := h.client.GetApp(id)
	if err != nil || app == nil || strings.TrimSpace(app.Name) == "" {
		return id
	}
	return app.Name
}

func (h *handler) resolveReleaseEnv(id string) map[string]string {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	if h != nil && h.appReleaseEnv != nil {
		return h.appReleaseEnv(id)
	}
	if h == nil || h.client == nil {
		return nil
	}
	rel, err := h.client.GetAppRelease(id)
	if err != nil || rel == nil {
		return nil
	}
	return rel.Env
}

func (h *handler) cutoverResourceEnv(previous, promoted *postgres.Instance) {
	_ = h.cutoverAttachedApps(previous, promoted)
}

func (h *handler) cutoverAttachedApps(previous, promoted *postgres.Instance) error {
	if h == nil || promoted == nil || strings.TrimSpace(promoted.App) == "" {
		return nil
	}
	var list []*ct.Resource
	var err error
	if h.listAllResources != nil {
		list, err = h.listAllResources()
	} else if h.client != nil {
		list, err = h.client.ResourceListAll()
	}
	if err != nil {
		return err
	}
	refs := []string{promoted.App, promoted.ID}
	if previous != nil {
		refs = append(refs, previous.App, previous.ID)
	}
	seen := map[string]bool{}
	matched := 0
	for _, r := range list {
		if r == nil || r.Env == nil {
			continue
		}
		match := false
		for _, ref := range refs {
			if resourceMatchesInstance(r, ref) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		matched++
		postgres.RewriteAttachmentEnv(r.Env, previous, promoted)
		var leader *postgres.Instance
		if promoted.Role == postgres.RoleFollower && h.store != nil && promoted.LeaderID != "" {
			leader, _ = h.store.Get(promoted.LeaderID)
		}
		applyPostgresResourceEnv(promoted, r.Env, leader)
		stripTenantPostgresCredentials(r.Env)
		if h.client != nil {
			if err := h.client.PutResource(r); err != nil {
				return fmt.Errorf("put resource %s: %w", r.ID, err)
			}
		}
		for _, appID := range resourceAppIDs(r) {
			if seen[appID] {
				continue
			}
			seen[appID] = true
			if err := deployTenantPostgresEnv(h.client, appID, previous, promoted); err != nil {
				return err
			}
		}
	}
	if h.log != nil {
		prev := ""
		if previous != nil {
			prev = previous.App
		}
		h.log.Info("cutover attached apps", "previous", prev, "promoted", promoted.App, "matched", matched, "deployed", len(seen))
	}
	return nil
}

func (h *handler) hydrateInstanceAttachments(inst *postgres.Instance, r *ct.Resource) {
	if inst == nil || r == nil {
		return
	}
	views := attachmentInfosForResource(r, h.resolveAppName, h.resolveReleaseEnv)
	if len(views) == 0 {
		return
	}
	atts := make([]postgres.Attachment, 0, len(views))
	for _, v := range views {
		env := map[string]string{}
		src := h.resolveReleaseEnv(v.ID)
		for _, k := range v.Keys {
			if src != nil && src[k] != "" {
				env[k] = src[k]
			}
		}
		as := strings.TrimSuffix(v.As, "_URL")
		atts = append(atts, postgres.Attachment{
			App: v.App,
			As:  as,
			Env: env,
		})
	}
	inst.Attachments = atts
}
