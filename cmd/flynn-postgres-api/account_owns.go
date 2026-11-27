package main

import (
	"strings"

	ct "github.com/randy-girard/flynn/controller/types"
)

type resourceLister interface {
	ResourceListAll() ([]*ct.Resource, error)
}

// accountOwnsLeader is true when the controller resource of the instance app
// leaderApp is owned by account. It authorizes an unattached follow
// (addons:create --unattached --follow), whose provision has no app, only the
// owner account the controller stamped (NEW-27).
func accountOwnsLeader(c resourceLister, leaderApp, account string) bool {
	leaderApp = strings.TrimSpace(leaderApp)
	account = strings.TrimSpace(account)
	if c == nil || leaderApp == "" || account == "" {
		return false
	}
	list, err := c.ResourceListAll()
	if err != nil {
		return false
	}
	for _, r := range list {
		if r != nil && r.Env != nil && strings.TrimSpace(r.Env["FLYNN_POSTGRES"]) == leaderApp {
			return r.OwnerAccount == account
		}
	}
	return false
}
