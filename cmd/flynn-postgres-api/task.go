package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func pluginAPIBase() string {
	if u := strings.TrimSpace(os.Getenv("POSTGRES_PLUGIN_URL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://postgres-plugin.discoverd"
}

func runPluginTask(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flynn-postgres-api task upgrade <resource>")
	}
	switch args[0] {
	case "upgrade":
		target := ""
		if len(args) > 1 {
			target = strings.TrimSpace(args[1])
		}
		if target == "" {
			return fmt.Errorf("upgrade requires a postgres resource name")
		}
		return runUpgradeTask(target)
	default:
		return fmt.Errorf("unknown task %q", args[0])
	}
}

func runUpgradeTask(target string) error {
	body, err := json.Marshal(map[string]string{"replication": string(postgres.ModeLogical)})
	if err != nil {
		return err
	}
	resp, err := http.Post(pluginAPIBase()+"/databases/"+target+"/upgrade", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != 200 {
		return fmt.Errorf("upgrade start %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var task postgres.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		return fmt.Errorf("upgrade start: %w", err)
	}
	fmt.Printf("upgrade task %s for %s (%s)\n", task.ID, target, task.Status)
	return waitForTask(task.ID)
}

func waitForTask(id string) error {
	deadline := time.Now().Add(postgres.DefaultUpgradeTimeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(pluginAPIBase() + "/tasks/" + id)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("upgrade status %s: %s", resp.Status, strings.TrimSpace(string(raw)))
		}
		var task postgres.Task
		if err := json.Unmarshal(raw, &task); err != nil {
			return err
		}
		fmt.Printf("status %s\n", task.Status)
		switch task.Status {
		case postgres.TaskDone:
			fmt.Printf("upgrade complete (follower %s)\n", task.FollowerID)
			return nil
		case postgres.TaskFailed:
			return fmt.Errorf("upgrade failed: %s", task.Error)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("upgrade timed out")
}
