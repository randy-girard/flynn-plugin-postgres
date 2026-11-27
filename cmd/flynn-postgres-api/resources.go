package main

import (
	"strings"

	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/host/resource"
	"github.com/randy-girard/flynn/pkg/dbruntime"
)

// instanceResources is the CPU and memory limits for a new instance's job:
// the provider config cpu/memory or the published runtime's
// (dbruntime.ProcessResourcesFor, which refuses a custom size unless
// allow-custom is on and a value below the engine minimum). A follower,
// upgrade copy or replacement with an unpublished runtime (a custom leader)
// and no cpu/memory of its own gets the leader's limits. Nil keeps the
// controller default.
func instanceResources(c any, engine, runtime string, cpu, memory int64, leaderApp string) (resource.Resources, error) {
	type cataloguer interface {
		ListDBRuntimes() (*dbruntime.Catalog, error)
	}
	var cat *dbruntime.Catalog
	if g, ok := c.(cataloguer); ok {
		var err error
		if cat, err = g.ListDBRuntimes(); err != nil {
			return nil, err
		}
	}
	r, err := dbruntime.ProcessResourcesFor(cat, engine, runtime, cpu, memory)
	if err != nil || r != nil || strings.TrimSpace(leaderApp) == "" {
		return r, err
	}
	type releaser interface {
		GetAppRelease(string) (*ct.Release, error)
	}
	if g, ok := c.(releaser); ok {
		if rel, err := g.GetAppRelease(leaderApp); err == nil && rel != nil {
			return releaseResources(rel), nil
		}
	}
	return nil, nil
}

// releaseResources is a copy of the CPU and memory limits of the first
// process in rel that has them.
func releaseResources(rel *ct.Release) resource.Resources {
	if rel == nil {
		return nil
	}
	for _, p := range rel.Processes {
		cpu, mem := p.Resources[resource.TypeCPU], p.Resources[resource.TypeMemory]
		if cpu.Limit == nil || mem.Limit == nil {
			continue
		}
		out := make(resource.Resources)
		out[resource.TypeCPU] = cpu
		out[resource.TypeMemory] = mem
		return out
	}
	return nil
}
