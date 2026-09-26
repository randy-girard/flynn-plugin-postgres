package main

import (
	"fmt"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/discoverd/client"
)

func (h *handler) live() bool {
	return h != nil && h.client != nil && h.imageID != ""
}

// startInstance creates one app, one volume, and one postgres process.
// Followers stay on the in-memory path until base-backup streaming exists.
func (h *handler) startInstance(inst *postgres.Instance) error {
	if inst == nil {
		return fmt.Errorf("missing instance")
	}
	db := "postgres"
	if len(inst.Databases) > 0 && inst.Databases[0].Name != "" {
		db = inst.Databases[0].Name
	}
	service := inst.App
	env := map[string]string{
		"FLYNN_POSTGRES":    service,
		"POSTGRES_USER":     inst.AppUser,
		"POSTGRES_PASSWORD": inst.AppPassword,
		"POSTGRES_DB":       db,
		"POSTGRES_URL":      inst.ConnectionURL(),
	}
	release := &ct.Release{
		ArtifactIDs: []string{h.imageID},
		Meta:        map[string]string{},
		Env:         env,
		Processes: map[string]ct.ProcessType{
			postgres.ProcessName: {
				Args:    []string{"/bin/start-flynn-postgres", "postgres"},
				Service: service,
				Ports:   []ct.Port{{Port: 5432, Proto: "tcp"}},
				Volumes: []ct.VolumeReq{{Path: postgres.VolumePath}},
			},
		},
	}
	app := &ct.App{
		Name:     service,
		Strategy: "one-down-one-up",
		Meta: map[string]string{
			"flynn-system-app":  "true",
			"flynn-datastore":   "true",
			"flynn-expose-port": "5432",
			"flynn-expose-tls":  "required",
		},
	}
	if err := h.client.CreateApp(app); err != nil {
		return err
	}
	if err := h.client.CreateRelease(app.ID, release); err != nil {
		_, _ = h.client.DeleteApp(app.ID)
		return err
	}
	timeout := 5 * time.Minute
	if err := h.client.ScaleAppRelease(app.ID, release.ID, ct.ScaleOptions{
		Processes: map[string]int{postgres.ProcessName: postgres.DefaultNodes},
		Timeout:   &timeout,
	}); err != nil {
		_, _ = h.client.DeleteApp(app.ID)
		return err
	}
	if err := h.client.SetAppRelease(app.ID, release.ID); err != nil {
		_, _ = h.client.DeleteApp(app.ID)
		return err
	}
	if _, err := discoverd.GetInstances(service, 5*time.Minute); err != nil {
		_, _ = h.client.DeleteApp(app.ID)
		return err
	}
	return nil
}
