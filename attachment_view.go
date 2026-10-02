package postgres

import (
	"sort"
	"strings"

	"github.com/randy-girard/flynn/pkg/resname"
)

// AttachmentURLKeys are the *_URL names an app uses for this resource. Color
// and --as keys come first; DATABASE_URL is last when it still matches.
func AttachmentURLKeys(release, resource map[string]string) []string {
	unset := resname.UnsetAttachment(release, resource)
	keys := make([]string, 0, len(unset))
	for k := range unset {
		if strings.HasSuffix(k, "_URL") {
			keys = append(keys, k)
		}
	}
	sortAttachmentURLKeys(keys)
	return keys
}

// AttachmentName is the primary env name (color or --as), not DATABASE_URL.
func AttachmentName(keys []string) string {
	for _, k := range keys {
		if k != "DATABASE_URL" {
			return k
		}
	}
	if len(keys) > 0 {
		return keys[0]
	}
	return ""
}

func urlKeysFromMap(env map[string]string) []string {
	if env == nil {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if strings.HasSuffix(k, "_URL") {
			keys = append(keys, k)
		}
	}
	sortAttachmentURLKeys(keys)
	return keys
}

func sortAttachmentURLKeys(keys []string) {
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := attachmentKeyRank(keys[i]), attachmentKeyRank(keys[j])
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
}

func attachmentKeyRank(k string) int {
	if k == "DATABASE_URL" {
		return 1
	}
	return 0
}

func attachmentInfos(inst *Instance) []AttachmentInfo {
	if inst == nil || len(inst.Attachments) == 0 {
		return nil
	}
	out := make([]AttachmentInfo, 0, len(inst.Attachments))
	for _, a := range inst.Attachments {
		keys := urlKeysFromMap(a.Env)
		as := strings.TrimSpace(a.As)
		if as != "" && !strings.HasSuffix(as, "_URL") {
			as += "_URL"
		}
		if as == "" {
			as = AttachmentName(keys)
		} else if !containsString(keys, as) {
			keys = append([]string{as}, keys...)
			sortAttachmentURLKeys(keys)
		}
		out = append(out, AttachmentInfo{
			App:   a.App,
			ID:    a.App,
			As:    as,
			Keys:  keys,
			Owner: inst.Tenant != "" && a.App == inst.Tenant,
		})
	}
	return out
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
