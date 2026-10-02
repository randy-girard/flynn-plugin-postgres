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
		return fmt.Errorf("usage: flynn-postgres-api task upgrade <resource> | create-db <resource> <database> | info <resource>")
	}
	switch args[0] {
	case "info":
		if len(args) < 2 {
			return fmt.Errorf("usage: flynn-postgres-api task info <resource>")
		}
		return runInfoTask(strings.TrimSpace(args[1]))
	case "upgrade":
		target := ""
		if len(args) > 1 {
			target = strings.TrimSpace(args[1])
		}
		if target == "" {
			return fmt.Errorf("upgrade requires a postgres resource name")
		}
		return runUpgradeTask(target)
	case "follow":
		if len(args) < 3 {
			return fmt.Errorf("usage: flynn-postgres-api task follow <resource> <app>")
		}
		return runFollowTask(strings.TrimSpace(args[1]), strings.TrimSpace(args[2]))
	case "create-db":
		if len(args) < 3 {
			return fmt.Errorf("usage: flynn-postgres-api task create-db <resource> <database>")
		}
		return runCreateDBTask(strings.TrimSpace(args[1]), strings.TrimSpace(args[2]))
	case "wait":
		if len(args) < 2 {
			return fmt.Errorf("usage: flynn-postgres-api task wait <resource>")
		}
		return runWaitTask(strings.TrimSpace(args[1]))
	case "promote", "unfollow":
		if len(args) < 2 {
			return fmt.Errorf("usage: flynn-postgres-api task %s <resource>", args[0])
		}
		return runInstanceActionTask(args[0], strings.TrimSpace(args[1]))
	default:
		return fmt.Errorf("unknown task %q", args[0])
	}
}

func runFollowTask(leader, app string) error {
	if leader == "" || app == "" {
		return fmt.Errorf("follow requires a postgres resource and app name")
	}
	body, err := json.Marshal(map[string]string{"app": app})
	if err != nil {
		return err
	}
	resp, err := http.Post(pluginAPIBase()+"/databases/"+leader+"/follow", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("follow %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Env map[string]string `json:"env"`
	}
	_ = json.Unmarshal(raw, &out)
	name := ""
	if out.Env != nil {
		name = strings.TrimSpace(out.Env["FLYNN_POSTGRES"])
	}
	if name != "" {
		fmt.Printf("started streaming follower %s of %s for %s\n", name, leader, app)
		return nil
	}
	fmt.Printf("started streaming follower of %s for %s\n", leader, app)
	return nil
}

func runCreateDBTask(resource, name string) error {
	if resource == "" {
		return fmt.Errorf("create-db requires a postgres resource name")
	}
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return err
	}
	resp, err := http.Post(pluginAPIBase()+"/databases/"+resource+"/databases", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("create-db %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	fmt.Printf("created database %s on %s\n", name, resource)
	return nil
}

func runInfoTask(resource string) error {
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return fmt.Errorf("info requires a postgres resource name")
	}
	resp, err := http.Get(pluginAPIBase() + "/databases/" + resource)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("info %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var info postgres.Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return fmt.Errorf("info decode: %w", err)
	}
	followers := "-"
	if len(info.Followers) > 0 {
		followers = strings.Join(info.Followers, ", ")
	}
	leader := strings.TrimSpace(info.LeaderID)
	if leader == "" {
		leader = "-"
	}
	fmt.Printf("app\t%s\nrole\t%s\nleader\t%s\nfollowers\t%s\nlag_bytes\t%d\nengine\t%s\nhost\t%s\n",
		info.App, info.Role, leader, followers, info.LagBytes, info.EngineVersion, info.Host)
	if len(info.Attachments) == 0 {
		fmt.Printf("attached\t-\n")
		return nil
	}
	for _, att := range info.Attachments {
		name := strings.Join(att.Keys, ",")
		if name == "" {
			name = strings.TrimSpace(att.As)
		}
		if name == "" {
			name = "-"
		}
		owner := ""
		if att.Owner {
			owner = "owner"
		}
		fmt.Printf("attached\t%s\t%s\t%s\n", att.App, name, owner)
	}
	return nil
}

func runWaitTask(resource string) error {
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return fmt.Errorf("wait requires a postgres resource name")
	}
	deadline := time.Now().Add(postgres.DefaultUpgradeTimeout)
	var last string
	for time.Now().Before(deadline) {
		p, err := fetchProgress(resource)
		if err != nil {
			return err
		}
		line := postgres.FormatProgress(p)
		if line != last {
			fmt.Println(line)
			last = line
		}
		if p.Phase == postgres.PhaseFailed {
			msg := strings.TrimSpace(p.Error)
			if msg == "" {
				msg = "replica failed"
			}
			return fmt.Errorf("%s", msg)
		}
		if p.Ready {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("wait timed out")
}

func fetchProgress(resource string) (postgres.ReplicaProgress, error) {
	resp, err := http.Get(pluginAPIBase() + "/databases/" + resource + "/progress")
	if err != nil {
		return postgres.ReplicaProgress{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return postgres.ReplicaProgress{}, fmt.Errorf("progress %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var p postgres.ReplicaProgress
	if err := json.Unmarshal(raw, &p); err != nil {
		return postgres.ReplicaProgress{}, fmt.Errorf("progress decode: %w", err)
	}
	return p, nil
}

func runInstanceActionTask(action, resource string) error {
	action = strings.TrimSpace(action)
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return fmt.Errorf("%s requires a postgres resource name", action)
	}
	resp, err := http.Post(pluginAPIBase()+"/databases/"+resource+"/"+action, "application/json", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s %s: %s", action, resp.Status, strings.TrimSpace(string(raw)))
	}
	fmt.Printf("%s %s\n", action, resource)
	return nil
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
	var last string
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
		line := "status " + task.Status
		if task.Progress != nil {
			line = formatWaitLine(task.Status, *task.Progress)
		}
		if line != last {
			fmt.Println(line)
			last = line
		}
		switch task.Status {
		case postgres.TaskDone:
			fmt.Printf("upgrade complete (follower %s)\n", task.FollowerID)
			return nil
		case postgres.TaskFailed:
			return fmt.Errorf("upgrade failed: %s", task.Error)
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("upgrade timed out")
}
