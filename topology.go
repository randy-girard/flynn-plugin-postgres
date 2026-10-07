package postgres

import (
	"fmt"
	"strings"
)

// Dashboard event-chart codes for datastore topology (leader election,
// failover, and planned follower-swap). Keep labels in sync with
// flynn-plugin-dashboard internal/events.Catalog and web eventCodes.
const (
	CodeFollowerStarted = "P10"
	CodeReplicaReady    = "P11"
	CodePromoted        = "P12"
	CodeFailover        = "P13"
	CodeDeposed         = "P14"
	CodeSwapStarted     = "P15"
	CodeSwapDone        = "P16"
	CodeSwapFailed      = "P17"
)

// FormatTopologyLine is a Flynn-level resource log line (same flynn-postgres
// prefix as sample# metrics so flynn-host shows flynn[postgres.N]).
func FormatTopologyLine(source, event, message string) string {
	source = firstNonEmpty(strings.TrimSpace(source), "postgres")
	event = strings.ToLower(strings.TrimSpace(event))
	if event == "" {
		event = "topology"
	}
	msg := strings.TrimSpace(message)
	if msg == "" {
		return fmt.Sprintf("flynn-postgres source=%s event#%s", source, event)
	}
	return fmt.Sprintf("flynn-postgres source=%s event#%s %s", source, event, msg)
}

// TopologyEventName is the event# token for a P-code (P13 -> failover).
func TopologyEventName(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case CodeFollowerStarted:
		return "follower"
	case CodeReplicaReady:
		return "replica-ready"
	case CodePromoted:
		return "promoted"
	case CodeFailover:
		return "failover"
	case CodeDeposed:
		return "deposed"
	case CodeSwapStarted:
		return "swap-start"
	case CodeSwapDone:
		return "swap-done"
	case CodeSwapFailed:
		return "swap-failed"
	default:
		return "topology"
	}
}

func TopologySeverity(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case CodeSwapFailed:
		return "error"
	case CodeFailover, CodeDeposed:
		return "warning"
	default:
		return "info"
	}
}

// TopologyDescription is the EventSwimlane tooltip / webhook description.
func TopologyDescription(code, from, to, extra string) string {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	extra = strings.TrimSpace(extra)
	var s string
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case CodeFollowerStarted:
		s = fmt.Sprintf("Follower %s started replicating from %s", firstNonEmpty(to, "replica"), firstNonEmpty(from, "primary"))
	case CodeReplicaReady:
		s = fmt.Sprintf("Replica %s caught up with %s and is ready to promote", firstNonEmpty(to, "replica"), firstNonEmpty(from, "primary"))
	case CodePromoted:
		s = fmt.Sprintf("Promoted %s to primary", firstNonEmpty(to, "replica"))
		if from != "" {
			s += " (was " + from + ")"
		}
	case CodeFailover:
		s = fmt.Sprintf("Auto-failover: %s is the new primary", firstNonEmpty(to, "replica"))
		if from != "" {
			s += "; previous primary " + from + " is down"
		}
	case CodeDeposed:
		s = fmt.Sprintf("Primary %s deposed", firstNonEmpty(from, "previous"))
		if to != "" {
			s += "; leader is now " + to
		}
	case CodeSwapStarted:
		s = fmt.Sprintf("Image refresh follower-swap started for %s", firstNonEmpty(from, "primary"))
	case CodeSwapDone:
		s = fmt.Sprintf("Image refresh follower-swap complete; %s is primary", firstNonEmpty(to, from, "primary"))
	case CodeSwapFailed:
		s = fmt.Sprintf("Image refresh follower-swap failed for %s", firstNonEmpty(from, "primary"))
	default:
		s = "Datastore topology change"
	}
	if extra != "" {
		s += " (" + extra + ")"
	}
	return s
}
