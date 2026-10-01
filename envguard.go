package postgres

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrAttachedEnv is returned when env:set would overwrite an attached env var.
var ErrAttachedEnv = errors.New("cannot env:set an attached env var while the resource is attached")

// RejectAttachedURLSet is the pure check the plugin API and a CLI can share.
// attached maps env names currently injected by an attachment.
// Updates to those keys (including unset) are blocked until detach.
func RejectAttachedURLSet(attached map[string]string, updates map[string]*string) error {
	var blocked []string
	for k := range updates {
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
