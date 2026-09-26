package postgres

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrAttachedEnv is returned when env:set would overwrite an attached *_URL.
var ErrAttachedEnv = errors.New("cannot env:set an attached URL while the resource is attached")

// RejectAttachedURLSet is the pure check the plugin API and a CLI can share.
// attached maps env names currently injected by an attachment to their URLs.
// A nil update value is env:unset and is allowed. After detach, attached is
// empty and the same key can be set again.
func RejectAttachedURLSet(attached map[string]string, updates map[string]*string) error {
	var blocked []string
	for k, v := range updates {
		if v == nil || !strings.HasSuffix(k, "_URL") {
			continue
		}
		if _, ok := attached[k]; ok {
			blocked = append(blocked, k)
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	sort.Strings(blocked)
	return fmt.Errorf("%w: %s", ErrAttachedEnv, strings.Join(blocked, ", "))
}

// AttachmentEnv is the single env var a resource attachment injects.
// The default name DATABASE becomes DATABASE_URL. ANALYTICS becomes ANALYTICS_URL.
func AttachmentEnv(as, rawURL string) map[string]string {
	name := attachmentName(as)
	return map[string]string{name + "_URL": rawURL}
}

func attachmentName(as string) string {
	as = strings.TrimSpace(as)
	if as == "" {
		return "DATABASE"
	}
	return strings.ToUpper(as)
}
