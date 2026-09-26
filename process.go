package postgres

// ProcessName is the only Postgres process a resource starts.
const ProcessName = "postgres"

// DefaultNodes is the process count. A resource does not start a sirenia pair.
const DefaultNodes = 1

// VolumePath is the single data volume mount.
const VolumePath = "/data"

// NodePlan is the scale request for one instance.
// Applying it is a controller concern. These tests never start Postgres.
type NodePlan struct {
	App        string
	Volume     string
	VolumePath string
	Processes  map[string]int
	Sirenia    bool
}

// NodePlan returns the one-node, one-volume shape for this instance.
func (i *Instance) NodePlan() NodePlan {
	nodes := i.Nodes
	if nodes == 0 {
		nodes = DefaultNodes
	}
	return NodePlan{
		App:        i.App,
		Volume:     i.Volume,
		VolumePath: VolumePath,
		Processes:  map[string]int{ProcessName: nodes},
		Sirenia:    false,
	}
}
