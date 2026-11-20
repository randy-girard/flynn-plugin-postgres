package main

import (
	"strings"

	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/pkg/dbruntime"
)

// instanceVolumeSize is the data volume size for a new instance: the
// provider config disk or the published runtime's disk
// (dbruntime.VolumeSize). A follower gets at least its leader's volume size,
// so a replica never has less room than the data it copies. Zero is the
// host default (20 GiB).
func instanceVolumeSize(c any, engine, runtime string, disk int64, leaderApp string) (int64, error) {
	type cataloguer interface {
		ListDBRuntimes() (*dbruntime.Catalog, error)
	}
	var cat *dbruntime.Catalog
	if g, ok := c.(cataloguer); ok {
		var err error
		if cat, err = g.ListDBRuntimes(); err != nil {
			return 0, err
		}
	}
	size, err := dbruntime.VolumeSize(cat, engine, runtime, disk)
	if err != nil || strings.TrimSpace(leaderApp) == "" {
		return size, err
	}
	type releaser interface {
		GetAppRelease(string) (*ct.Release, error)
	}
	if g, ok := c.(releaser); ok {
		if rel, err := g.GetAppRelease(leaderApp); err == nil && rel != nil {
			if lead := releaseVolumeSize(rel); lead > size {
				return lead, nil
			}
		}
	}
	return size, nil
}

// releaseVolumeSize is the first sized volume in rel.
func releaseVolumeSize(rel *ct.Release) int64 {
	if rel == nil {
		return 0
	}
	for _, p := range rel.Processes {
		for _, v := range p.Volumes {
			if v.Size > 0 {
				return v.Size
			}
		}
	}
	return 0
}
