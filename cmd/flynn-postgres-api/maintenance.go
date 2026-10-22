package main

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/pkg/httphelper"
)

// maintenanceAuto is the config switch for upgrading resource containers when
// the plugin API starts. Plugin updates restart this process; leaving the
// variable unset keeps running instances on their current release until
// flynn-host pg:maintenance.
func maintenanceAuto() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MAINTENANCE_AUTO"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// parseMaintenanceTargets accepts --all or a comma-separated instance list.
// An empty selection is an error so a bare command cannot roll every database.
func parseMaintenanceTargets(all bool, raw string) (only map[string]struct{}, everything bool, err error) {
	raw = strings.TrimSpace(raw)
	if all || strings.EqualFold(raw, "all") {
		return nil, true, nil
	}
	only = map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		only[name] = struct{}{}
	}
	if len(only) == 0 {
		return nil, false, fmt.Errorf("pass --all or a comma-separated list of instance names")
	}
	return only, false, nil
}

func maintenanceSelected(only map[string]struct{}, names ...string) bool {
	if only == nil {
		return true
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := only[name]; ok {
			return true
		}
	}
	return false
}

func maintenanceMissing(only, seen map[string]struct{}) []string {
	if len(only) == 0 {
		return nil
	}
	var missing []string
	for name := range only {
		if _, ok := seen[name]; ok {
			continue
		}
		missing = append(missing, name+": not a running instance")
	}
	sort.Strings(missing)
	return missing
}

func (h *handler) maintain(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var body struct {
		All       bool   `json:"all"`
		Instances string `json:"instances"`
	}
	_ = decode(r, &body)
	only, everything, err := parseMaintenanceTargets(body.All, body.Instances)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	var filter map[string]struct{}
	if !everything {
		filter = only
	}
	h.refreshIsolatedImagesOnly(filter)
	started, skipped := h.beginClusterUpgradesOnly(filter)
	httphelper.JSON(w, http.StatusAccepted, map[string]any{
		"tasks":   started,
		"skipped": skipped,
	})
}

type nameAdopter interface {
	GetApp(string) (*ct.App, error)
	UpdateApp(*ct.App) error
	GetAppRelease(string) (*ct.Release, error)
	CreateRelease(string, *ct.Release) error
	SetAppRelease(string, string) error
	ScaleAppRelease(string, string, ct.ScaleOptions) error
}

// adoptInstanceName moves the replacement app onto the original resource name.
// The replacement comes up under a temporary name. At the switch the original
// app is renamed aside, the replacement takes the original name, and its
// release service and env follow. Attachment URLs keep that name; a host
// change is applied later by cutover.
func (h *handler) adoptInstanceName(previous, next *postgres.Instance) (string, error) {
	if h == nil || h.client == nil || previous == nil || next == nil {
		return "", nil
	}
	return adoptAppName(h.client, previous, next)
}

func adoptAppName(c nameAdopter, previous, next *postgres.Instance) (string, error) {
	if c == nil || previous == nil || next == nil {
		return "", nil
	}
	original := strings.TrimSpace(previous.App)
	temp := strings.TrimSpace(next.App)
	if original == "" || temp == "" || original == temp {
		return "", nil
	}
	oldApp, err := c.GetApp(original)
	if err != nil || oldApp == nil {
		if err == nil {
			err = fmt.Errorf("app %s not found", original)
		}
		return "", err
	}
	tempApp, err := c.GetApp(temp)
	if err != nil || tempApp == nil {
		if err == nil {
			err = fmt.Errorf("app %s not found", temp)
		}
		return "", err
	}
	running, err := c.GetAppRelease(tempApp.ID)
	if err != nil || running == nil {
		if err == nil {
			err = fmt.Errorf("app %s has no release", temp)
		}
		return "", err
	}
	retired := original + "-retired1"
	if existing, lookupErr := c.GetApp(retired); lookupErr == nil && existing != nil {
		retired = fmt.Sprintf("%s-retired1-%d", original, time.Now().Unix()%100000)
	}
	stop := 60 * time.Second
	if oldRel, relErr := c.GetAppRelease(oldApp.ID); relErr == nil && oldRel != nil && oldRel.ID != "" {
		zeros := map[string]int{}
		for name := range oldRel.Processes {
			zeros[name] = 0
		}
		if len(zeros) == 0 {
			zeros[postgres.ProcessName] = 0
		}
		if err := c.ScaleAppRelease(oldApp.ID, oldRel.ID, ct.ScaleOptions{Processes: zeros, Timeout: &stop}); err != nil {
			return "", err
		}
	}
	tempZeros := map[string]int{}
	for name := range running.Processes {
		tempZeros[name] = 0
	}
	if len(tempZeros) == 0 {
		tempZeros[postgres.ProcessName] = 0
	}
	if err := c.ScaleAppRelease(tempApp.ID, running.ID, ct.ScaleOptions{Processes: tempZeros, Timeout: &stop}); err != nil {
		return "", err
	}
	oldApp.Name = retired
	if err := c.UpdateApp(oldApp); err != nil {
		return "", err
	}
	rewritten := rewriteReleaseIdentity(running, temp, original)
	if err := c.CreateRelease(tempApp.ID, rewritten); err != nil {
		return "", err
	}
	if err := c.SetAppRelease(tempApp.ID, rewritten.ID); err != nil {
		return "", err
	}
	tempApp.Name = original
	if err := c.UpdateApp(tempApp); err != nil {
		return "", err
	}
	previous.App = retired
	next.App = original
	if strings.TrimSpace(next.ServiceHost) == "" {
		next.ServiceHost = "leader." + original + ".discoverd"
	} else {
		next.ServiceHost = strings.ReplaceAll(next.ServiceHost, temp, original)
	}
	return retired, nil
}

func rewriteReleaseIdentity(rel *ct.Release, from, to string) *ct.Release {
	next := *rel
	next.ID = ""
	next.Env = make(map[string]string, len(rel.Env))
	for k, v := range rel.Env {
		next.Env[k] = strings.ReplaceAll(v, from, to)
	}
	next.Processes = make(map[string]ct.ProcessType, len(rel.Processes))
	for name, proc := range rel.Processes {
		if proc.Service == from {
			proc.Service = to
		}
		if len(proc.Args) > 0 {
			args := append([]string(nil), proc.Args...)
			for i := range args {
				args[i] = strings.ReplaceAll(args[i], from, to)
			}
			proc.Args = args
		}
		next.Processes[name] = proc
	}
	return &next
}
