package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/pkg/dbruntime"
)

func controllerBase() string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("CONTROLLER_URL")), "/")
	if base == "" {
		base = "http://controller.discoverd"
	}
	return base
}

func fetchPostgresAvailability() (string, error) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := controllerGET("/cluster/postgres-availability", &body); err != nil {
		return "", err
	}
	switch body.Mode {
	case postgres.AvailabilitySystem, postgres.AvailabilityManual:
		return body.Mode, nil
	default:
		return "", fmt.Errorf("postgres availability %q", body.Mode)
	}
}

func controllerGET(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, controllerBase()+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("", strings.TrimSpace(os.Getenv("CONTROLLER_KEY")))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, res.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (h *handler) runtimeNeed(name string) (uint64, uint64) {
	if h == nil || h.client == nil || strings.TrimSpace(name) == "" {
		return 0, 0
	}
	cat, err := h.client.ListDBRuntimes()
	if err != nil || cat == nil {
		return 0, 0
	}
	rt, ok := cat.Find(dbruntime.EnginePostgres, name)
	if !ok || rt.Memory <= 0 || rt.Disk <= 0 {
		return 0, 0
	}
	return uint64(rt.Memory), uint64(rt.Disk)
}

func (h *handler) loadHostFits() ([]postgres.HostFit, error) {
	if h == nil || h.client == nil {
		return nil, fmt.Errorf("missing controller")
	}
	var listed []struct {
		ID           string            `json:"id"`
		Tags         map[string]string `json:"tags"`
		MaxProcesses *int              `json:"max_processes"`
	}
	if err := controllerGET("/hosts", &listed); err != nil {
		return nil, err
	}
	jobs, err := h.client.JobListActive()
	if err != nil {
		return nil, err
	}
	procs := map[string]int{}
	singletons := map[string]int{}
	for _, j := range jobs {
		if j == nil || j.HostID == "" || !jobOccupies(j.State) {
			continue
		}
		procs[j.HostID]++
		if len(j.VolumeIDs) > 0 {
			singletons[j.HostID]++
		}
	}
	out := make([]postgres.HostFit, 0, len(listed))
	for _, host := range listed {
		if strings.TrimSpace(host.ID) == "" {
			continue
		}
		fit := postgres.HostFit{
			ID:             host.ID,
			ProcessCount:   procs[host.ID],
			SingletonCount: singletons[host.ID],
		}
		if host.MaxProcesses != nil && *host.MaxProcesses > 0 {
			fit.MaxProcesses = *host.MaxProcesses
		} else if n, ok := ct.ParseHostMaxProcesses(host.Tags); ok && n > 0 {
			fit.MaxProcesses = n
		}
		if n := persistentSingletonCap(host.Tags); n > 0 {
			fit.MaxSingletons = n
		}
		var stats struct {
			MemoryAvailableBytes uint64 `json:"memory_available_bytes"`
			DiskFreeBytes        uint64 `json:"disk_free_bytes"`
		}
		if err := controllerGET("/hosts/"+host.ID+"/stats", &stats); err != nil {
			out = append(out, fit)
			continue
		}
		fit.StatsKnown = true
		fit.MemoryAvailable = stats.MemoryAvailableBytes
		fit.DiskFree = stats.DiskFreeBytes
		out = append(out, fit)
	}
	return out, nil
}

func persistentSingletonCap(tags map[string]string) int {
	raw := strings.ToLower(strings.TrimSpace(tags["flynn-max-persistent-singletons"]))
	switch raw {
	case "", "0", "unlimited", "off", "none", "auto":
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
