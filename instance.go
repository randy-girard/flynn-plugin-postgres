package postgres

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// ResourceIDEnv is stamped on the isolated instance release so deprovision can
// find the Flynn app after the plugin API restarts (in-memory store is empty).
const ResourceIDEnv = "FLYNN_RESOURCE_ID"

// Role is the replication role of one resource. A follower is its own resource,
// not an extra node inside the leader's app.
type Role string

const (
	RolePrimary    Role = "primary"
	RoleFollower   Role = "follower"
	RoleStandalone Role = "standalone"
)

// ReplicationMode is stored on the follower. Streaming is same-major.
// Logical is the major-upgrade path. Neither resizes the leader in place.
type ReplicationMode string

const (
	ModeStreaming ReplicationMode = "streaming"
	ModeLogical   ReplicationMode = "logical"
)

var (
	ErrNotFound          = errors.New("postgres instance not found")
	ErrReadOnly          = errors.New("follower is read-only")
	ErrFollowFollower    = errors.New("a follower cannot follow another follower")
	ErrNotFollower       = errors.New("only a follower can be promoted or unfollowed")
	ErrNoResize          = errors.New("no in-place resize or upgrade; pg:upgrade follows, waits until caught up, then promotes")
	ErrHasFollowers      = errors.New("cannot remove a resource while it still has followers")
	ErrNotPrimary        = errors.New("only a primary can be upgraded")
	ErrUpgradeInProgress = errors.New("an upgrade is already running for this instance")
	ErrUpgradeFollower   = errors.New("followers are recreated after the new primary is promoted; do not upgrade a follower")
	ErrFollowLogical     = errors.New("followers use streaming replication on the same engine version; use pg:upgrade for a major-version swap")
	ErrFollowVersion     = errors.New("a follower must run the same engine version as its primary; use pg:upgrade to swap to a new version")
)

// User is a role that exists only in one instance's state.
type User struct {
	Name     string `json:"name"`
	Password string `json:"-"`
	Database string `json:"database,omitempty"`
}

// Database is a database name inside one instance.
type Database struct {
	Name string
}

// Row is a replicated data change. Tests use it instead of a live server.
type Row struct {
	DB    string
	Key   string
	Value string
}

// Attachment binds one app to this resource. Env holds every *_URL injected
// for the app. Those keys cannot be changed with env:set while attached.
type Attachment struct {
	App string
	As  string
	URL string
	Env map[string]string
}

// AttachmentInfo is one tenant app using this database, for pg:info and the
// dashboard overview. As is the primary env name (FLYNN_POSTGRESQL_AMBER_URL
// or --as ANALYTICS_URL). Keys lists every matching *_URL on that app.
type AttachmentInfo struct {
	App   string   `json:"app"`
	ID    string   `json:"id,omitempty"`
	As    string   `json:"as,omitempty"`
	Keys  []string `json:"keys,omitempty"`
	Owner bool     `json:"owner,omitempty"`
}

// Instance is one Postgres resource: one Flynn app, one volume, one node.
type Instance struct {
	ID                string
	Tenant            string
	App               string
	Volume            string
	Superuser         string
	SuperuserPassword string
	AppUser           string
	AppPassword       string
	Nodes             int
	Runtime           string
	EngineVersion     string
	Role              Role
	LeaderID          string
	Mode              ReplicationMode
	ReadOnly          bool
	LagBytes          int64
	BackupCopied      int64
	BackupTotal       int64
	CatchupMax        int64
	ServiceHost       string
	Databases         []Database
	Users             []User
	Rows              []Row
	Attachments       []Attachment
	Followers         []string
	seq               int64
	applied           int64
}

// Info is the pg:info view.
type Info struct {
	ID            string           `json:"id"`
	Role          Role             `json:"role"`
	LeaderID      string           `json:"leader_id,omitempty"`
	Followers     []string         `json:"followers,omitempty"`
	LagBytes      int64            `json:"lag_bytes"`
	ReadOnly      bool             `json:"read_only"`
	Nodes         int              `json:"nodes"`
	Runtime       string           `json:"runtime"`
	EngineVersion string           `json:"engine_version,omitempty"`
	Mode          ReplicationMode  `json:"replication,omitempty"`
	App           string           `json:"app"`
	Volume        string           `json:"volume"`
	Host          string           `json:"host"`
	Attachments   []AttachmentInfo `json:"attachments,omitempty"`
}

// ProvisionRequest creates a primary, or a follower when Follow is set.
type ProvisionRequest struct {
	Tenant     string
	App        string
	As         string
	Follow     string
	Mode       ReplicationMode
	Runtime    string
	ForUpgrade bool
}

// PromoteResult is a promoted follower plus the previous leader, which remains.
type PromoteResult struct {
	Promoted       *Instance
	PreviousLeader *Instance
	Rewritten      []Attachment
}

// Store is the in-memory state machine. It does not start Postgres.
type Store struct {
	mu          sync.Mutex
	byID        map[string]*Instance
	providerURL string
	contacts    []string
	// NameTaken reports app names that already exist outside this process.
	NameTaken func(name string) bool
	// LoadMissing loads a live instance (postgresql-concave-48291) after this
	// process restarts. Follow looks up by app name; the in-memory map is empty.
	LoadMissing func(idOrApp string) *Instance

	tasks           map[string]*Task
	upgradeByLeader map[string]string
	liveProgress    LiveProgressFunc
}

// Primaries are isolated instances that are not followers.
func (s *Store) Primaries() []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, inst := range s.byID {
		if inst == nil || inst.Role == RoleFollower {
			continue
		}
		out = append(out, inst.snapshot())
	}
	return out
}

// All returns every isolated instance, including followers.
func (s *Store) All() []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, inst := range s.byID {
		if inst == nil {
			continue
		}
		out = append(out, inst.snapshot())
	}
	return out
}

// NewStore returns a store aimed at this plugin's discoverd host.
func NewStore() *Store {
	return &Store{
		byID:            map[string]*Instance{},
		providerURL:     ProviderURL(),
		tasks:           map[string]*Task{},
		upgradeByLeader: map[string]string{},
	}
}

// SetProviderURL overrides the provider endpoint. The platform appliance host is rejected.
func (s *Store) SetProviderURL(raw string) error {
	if err := EndpointAllowed(raw); err != nil {
		return err
	}
	s.mu.Lock()
	s.providerURL = raw
	s.mu.Unlock()
	return nil
}

// Contacts lists provider endpoints used by provision. Tests assert the appliance is absent.
func (s *Store) Contacts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.contacts))
	copy(out, s.contacts)
	return out
}

// Provision creates an isolated instance. Follow copies the leader, then stays caught up.
func (s *Store) Provision(req ProvisionRequest) (*Instance, map[string]string, error) {
	if err := EndpointAllowed(s.providerURL); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provisionLocked(req)
}

func (s *Store) provisionLocked(req ProvisionRequest) (*Instance, map[string]string, error) {
	s.contacts = append(s.contacts, s.providerURL)

	var leader *Instance
	if req.Follow != "" {
		leader = s.lookupLocked(req.Follow)
		if leader == nil {
			return nil, nil, ErrNotFound
		}
		if leader.Role == RoleFollower || leader.ReadOnly {
			return nil, nil, ErrFollowFollower
		}
	}

	id := newID()
	inst := &Instance{
		ID:                id,
		Tenant:            firstNonEmpty(req.Tenant, req.App),
		App:               s.uniqueApp(),
		Volume:            "vol-" + id,
		Superuser:         "su_" + id,
		SuperuserPassword: "supw_" + newID(),
		AppUser:           "app_" + id,
		AppPassword:       "apppw_" + newID(),
		Nodes:             DefaultNodes,
		Runtime:           defaultRuntime(req.Runtime),
		EngineVersion:     EngineVersion(),
		Role:              RolePrimary,
	}
	inst.ServiceHost = "leader." + inst.App + ".discoverd"
	if hostOf(inst.ServiceHost) == PlatformApplianceHost {
		return nil, nil, ErrPlatformAppliance
	}
	inst.Databases = []Database{{Name: DefaultDatabaseName()}}

	if leader != nil {
		mode := req.Mode
		if req.ForUpgrade {
			if mode == "" {
				mode = ModeLogical
			}
		} else {
			if mode != "" && mode != ModeStreaming {
				return nil, nil, ErrFollowLogical
			}
			mode = ModeStreaming
			if !SameEngineVersion(leader.EngineVersion, inst.EngineVersion) {
				return nil, nil, ErrFollowVersion
			}
		}
		if mode != ModeStreaming && mode != ModeLogical {
			return nil, nil, fmt.Errorf("replication mode %q must be streaming or logical", mode)
		}
		inst.Role = RoleFollower
		inst.ReadOnly = true
		inst.LeaderID = leader.ID
		inst.Mode = mode
		inst.AppUser = leader.AppUser
		inst.AppPassword = leader.AppPassword
		inst.Databases = append([]Database(nil), leader.Databases...)
		inst.Users = append([]User(nil), leader.Users...)
		inst.Rows = append([]Row(nil), leader.Rows...)
		inst.applied = leader.seq
		leader.Followers = append(leader.Followers, inst.ID)
		if strings.TrimSpace(req.Runtime) == "" {
			inst.Runtime = leader.Runtime
		}
	}

	s.byID[inst.ID] = inst
	var env map[string]string
	if req.App != "" {
		env = s.attachLocked(inst, req.App, req.As, true)
	}
	return inst.snapshot(), env, nil
}

// lookupLocked finds a resource by id or by isolated app name (postgresql-concave-48291).
func (s *Store) lookupLocked(idOrApp string) *Instance {
	idOrApp = strings.TrimSpace(idOrApp)
	if idOrApp == "" {
		return nil
	}
	if inst := s.findLocked(idOrApp); inst != nil {
		return inst
	}
	if s.LoadMissing == nil {
		return nil
	}
	inst := s.LoadMissing(idOrApp)
	if inst == nil {
		return nil
	}
	if strings.TrimSpace(inst.ID) == "" {
		inst.ID = firstNonEmpty(inst.App, idOrApp)
	}
	if existing := s.findLocked(firstNonEmpty(inst.App, inst.ID)); existing != nil {
		mergeLiveInstance(existing, inst)
		return existing
	}
	s.byID[inst.ID] = inst
	return inst
}

func (s *Store) findLocked(idOrApp string) *Instance {
	if inst := s.byID[idOrApp]; inst != nil {
		return inst
	}
	var found *Instance
	for _, inst := range s.byID {
		if inst == nil || inst.App != idOrApp {
			continue
		}
		if found == nil || len(inst.Users) > len(found.Users) || len(inst.Databases) > len(found.Databases) {
			found = inst
		}
	}
	return found
}

func mergeLiveInstance(dst, src *Instance) {
	if dst == nil || src == nil || dst == src {
		return
	}
	if dst.AppUser == "" {
		dst.AppUser = src.AppUser
	}
	if dst.AppPassword == "" {
		dst.AppPassword = src.AppPassword
	}
	if dst.ServiceHost == "" {
		dst.ServiceHost = src.ServiceHost
	}
	if dst.App == "" {
		dst.App = src.App
	}
	if dst.Tenant == "" {
		dst.Tenant = src.Tenant
	}
	if len(dst.Databases) == 0 && len(src.Databases) > 0 {
		dst.Databases = append([]Database(nil), src.Databases...)
	}
	if len(dst.Users) == 0 && len(src.Users) > 0 {
		dst.Users = append([]User(nil), src.Users...)
	}
	if len(dst.Attachments) == 0 && len(src.Attachments) > 0 {
		dst.Attachments = append([]Attachment(nil), src.Attachments...)
	}
}

// Get returns a copy of the instance.
func (s *Store) Get(id string) (*Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return nil, ErrNotFound
	}
	return inst.snapshot(), nil
}

// FollowerApps is the isolated app names still replicating from id (NAME or ID).
func (s *Store) FollowerApps(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || name == inst.App || name == inst.ID || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	kept := inst.Followers[:0]
	for _, fid := range inst.Followers {
		fol := s.findLocked(fid)
		if fol == nil {
			continue
		}
		kept = append(kept, fid)
		add(firstNonEmpty(fol.App, fol.ID))
	}
	inst.Followers = kept
	for _, other := range s.byID {
		if other == nil || other.ID == inst.ID {
			continue
		}
		if other.LeaderID == inst.ID || other.LeaderID == inst.App {
			add(firstNonEmpty(other.App, other.ID))
		}
	}
	return names
}

// ReconcileFollowers drops in-memory replica records that are no longer on
// the cluster (the other web job may have deprovisioned them). liveApps is
// isolated instance names still pointing at this primary.
func (s *Store) ReconcileFollowers(id string, liveApps []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return
	}
	keep := map[string]bool{}
	for _, name := range liveApps {
		name = strings.TrimSpace(name)
		if name != "" {
			keep[name] = true
		}
	}
	liveFollower := func(fol *Instance, fid string) bool {
		if keep[strings.TrimSpace(fid)] {
			return true
		}
		if fol == nil {
			return false
		}
		return keep[fol.App] || keep[fol.ID]
	}
	kept := inst.Followers[:0]
	for _, fid := range inst.Followers {
		fol := s.findLocked(fid)
		if !liveFollower(fol, fid) {
			if fol != nil {
				delete(s.byID, fol.ID)
			}
			continue
		}
		kept = append(kept, fid)
	}
	inst.Followers = kept
	var gone []string
	for _, other := range s.byID {
		if other == nil || other.ID == inst.ID {
			continue
		}
		if other.LeaderID != inst.ID && other.LeaderID != inst.App {
			continue
		}
		if liveFollower(other, other.App) {
			continue
		}
		gone = append(gone, other.ID)
	}
	for _, gid := range gone {
		delete(s.byID, gid)
	}
}

// DeleteBlockedBy is the follower instance names that prevent deleting inst.
// Followers can always be deleted. A primary or standalone copy cannot while
// those names are still replicating from it.
func DeleteBlockedBy(inst *Instance, followerApps []string) []string {
	if inst == nil || inst.Role == RoleFollower {
		return nil
	}
	out := make([]string, 0, len(followerApps))
	for _, name := range followerApps {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// CanDeleteResource is nil when inst may be deprovisioned: a follower, or a
// primary/standalone with no remaining followers.
func CanDeleteResource(inst *Instance, followerApps []string) error {
	if inst == nil {
		return ErrNotFound
	}
	blocked := DeleteBlockedBy(inst, followerApps)
	if len(blocked) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrHasFollowers, strings.Join(blocked, ", "))
}

// Forget drops an instance from the in-memory store after deprovision.
func (s *Store) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.findLocked(id)
	if inst == nil {
		return
	}
	for _, other := range s.byID {
		if other == nil || other == inst {
			continue
		}
		other.Followers = removeFollowerRef(other.Followers, inst)
	}
	delete(s.byID, inst.ID)
}

// Info is pg:info for one resource.
func (s *Store) Info(id string) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return Info{}, ErrNotFound
	}
	followers := append([]string(nil), inst.Followers...)
	return Info{
		ID:            inst.ID,
		Role:          inst.Role,
		LeaderID:      inst.LeaderID,
		Followers:     followers,
		LagBytes:      inst.LagBytes,
		ReadOnly:      inst.ReadOnly,
		Nodes:         inst.Nodes,
		Runtime:       inst.Runtime,
		EngineVersion: inst.EngineVersion,
		Mode:          inst.Mode,
		App:           inst.App,
		Volume:        inst.Volume,
		Host:          inst.ServiceHost,
		Attachments:   attachmentInfos(inst),
	}, nil
}

// AddDatabase creates a database that exists only on this instance.
func (s *Store) AddDatabase(id, name string) error {
	name = strings.TrimSpace(name)
	if err := ValidDatabaseName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return ErrNotFound
	}
	if inst.ReadOnly {
		return ErrReadOnly
	}
	for _, db := range inst.Databases {
		if db.Name == name {
			return fmt.Errorf("database %s already exists", name)
		}
	}
	inst.Databases = append(inst.Databases, Database{Name: name})
	s.replicateMetaLocked(inst)
	return nil
}

// AddUser creates a user that exists only on this instance.
func (s *Store) AddUser(id, name, password, database string) error {
	name = strings.TrimSpace(name)
	if err := ValidUserName(name); err != nil {
		return err
	}
	if strings.TrimSpace(password) == "" {
		return errors.New("password is required")
	}
	database = strings.TrimSpace(database)
	if database != "" {
		if err := ValidDatabaseName(database); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return ErrNotFound
	}
	if inst.ReadOnly {
		return ErrReadOnly
	}
	for _, u := range inst.Users {
		if u.Name == name {
			return fmt.Errorf("user %s already exists", name)
		}
	}
	inst.Users = append(inst.Users, User{Name: name, Password: password, Database: database})
	s.replicateMetaLocked(inst)
	return nil
}

// DropUser removes a login created on this instance. The instance PGUSER stays.
func (s *Store) DropUser(id, name string) error {
	name = strings.TrimSpace(name)
	if err := ValidUserName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return ErrNotFound
	}
	if inst.ReadOnly {
		return ErrReadOnly
	}
	if inst.AppUser != "" && strings.EqualFold(inst.AppUser, name) {
		return fmt.Errorf("cannot drop the instance login %s", name)
	}
	kept := inst.Users[:0]
	found := false
	for _, u := range inst.Users {
		if strings.EqualFold(u.Name, name) {
			found = true
			continue
		}
		kept = append(kept, u)
	}
	if !found {
		return fmt.Errorf("user %s not found", name)
	}
	inst.Users = kept
	s.replicateMetaLocked(inst)
	return nil
}

// Users returns roles stored on this instance. Superuser is not included.
func (s *Store) Users(id string) ([]User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return nil, ErrNotFound
	}
	return append([]User(nil), inst.Users...), nil
}

// Databases returns database names stored on this instance.
func (s *Store) Databases(id string) ([]Database, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return nil, ErrNotFound
	}
	return append([]Database(nil), inst.Databases...), nil
}

// Write changes data on a writable instance and replicates to caught-up followers.
func (s *Store) Write(id, db, key, val string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
	if inst == nil {
		return ErrNotFound
	}
	if inst.ReadOnly {
		return ErrReadOnly
	}
	inst.seq++
	row := Row{DB: db, Key: key, Value: val}
	inst.Rows = append(inst.Rows, row)
	for _, fid := range inst.Followers {
		fol := s.byID[fid]
		if fol == nil || fol.Role != RoleFollower || fol.LeaderID != inst.ID {
			continue
		}
		if fol.LagBytes > 0 {
			continue
		}
		fol.Rows = append(fol.Rows, row)
		fol.applied = inst.seq
	}
	return nil
}

// Rows returns a copy of replicated rows.
func (s *Store) Rows(id string) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
	if inst == nil {
		return nil, ErrNotFound
	}
	return append([]Row(nil), inst.Rows...), nil
}

// SetLag installs a fake follower lag. Zero catches the follower up to the leader.
func (s *Store) SetLag(id string, bytes int64) error {
	if bytes < 0 {
		return errors.New("lag must be >= 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
	if inst == nil {
		return ErrNotFound
	}
	if inst.Role != RoleFollower {
		return ErrNotFollower
	}
	inst.LagBytes = bytes
	if bytes > inst.CatchupMax {
		inst.CatchupMax = bytes
	}
	if bytes == 0 {
		s.catchUpLocked(inst)
	}
	return nil
}

// Promote makes the follower writable, ends the follow, and rewrites the
// previous leader's attachment URLs. The old leader remains its own resource.
func (s *Store) Promote(id string) (*PromoteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fol := s.lookupLocked(id)
	if fol == nil {
		return nil, ErrNotFound
	}
	if fol.Role != RoleFollower || fol.LeaderID == "" {
		return nil, ErrNotFollower
	}
	leader := s.byID[fol.LeaderID]
	if leader == nil {
		return nil, ErrNotFound
	}
	s.catchUpLocked(fol)
	newURL := fol.appURL()
	var rewritten []Attachment
	for i := range leader.Attachments {
		leader.Attachments[i].URL = newURL
		for k := range leader.Attachments[i].Env {
			leader.Attachments[i].Env[k] = newURL
		}
		rewritten = append(rewritten, leader.Attachments[i])
	}
	leader.Followers = removeFollowerRef(leader.Followers, fol)
	fol.Role = RolePrimary
	fol.ReadOnly = false
	fol.LeaderID = ""
	fol.Mode = ""
	fol.LagBytes = 0
	return &PromoteResult{
		Promoted:       fol.snapshot(),
		PreviousLeader: leader.snapshot(),
		Rewritten:      rewritten,
	}, nil
}

// Unfollow stops replication and leaves a standalone writable copy.
func (s *Store) Unfollow(id string) (*Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fol := s.lookupLocked(id)
	if fol == nil {
		return nil, ErrNotFound
	}
	if fol.Role != RoleFollower || fol.LeaderID == "" {
		return nil, ErrNotFollower
	}
	if leader := s.byID[fol.LeaderID]; leader != nil {
		leader.Followers = removeFollowerRef(leader.Followers, fol)
	}
	fol.Role = RoleStandalone
	fol.ReadOnly = false
	fol.LeaderID = ""
	fol.Mode = ""
	fol.LagBytes = 0
	return fol.snapshot(), nil
}

// Attach adds one env var for app. A second app can use a different name.
func (s *Store) Attach(id, app, as string) (map[string]string, error) {
	app = strings.TrimSpace(app)
	if app == "" {
		return nil, errors.New("app is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
	if inst == nil {
		return nil, ErrNotFound
	}
	return cloneEnv(s.attachLocked(inst, app, as, false)), nil
}

// Detach removes the app's attachment and its env var.
func (s *Store) Detach(id, app string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
	if inst == nil {
		return ErrNotFound
	}
	next := inst.Attachments[:0]
	found := false
	for _, a := range inst.Attachments {
		if a.App == app {
			found = true
			continue
		}
		next = append(next, a)
	}
	if !found {
		return fmt.Errorf("app %s is not attached", app)
	}
	inst.Attachments = next
	return nil
}

// EnvForApp is the single *_URL injected into app, or an empty map.
func (s *Store) EnvForApp(id, app string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return nil, ErrNotFound
	}
	for _, a := range inst.Attachments {
		if a.App == app {
			if len(a.Env) > 0 {
				return cloneEnv(a.Env), nil
			}
			return AttachmentEnv(a.As, a.URL), nil
		}
	}
	return map[string]string{}, nil
}

// ForApp returns instances visible to one tenant app. Other tenants are omitted.
func (s *Store) ForApp(app string) []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, inst := range s.byID {
		if inst.visibleTo(app) {
			out = append(out, inst.snapshot())
		}
	}
	return out
}

// Resize is rejected. The supported path is follow, wait, promote.
func (s *Store) Resize(string, string) error {
	return ErrNoResize
}

// CheckEnvSet applies RejectAttachedURLSet for the app's current attachment.
func (s *Store) CheckEnvSet(id, app string, updates map[string]*string) error {
	env, err := s.EnvForApp(id, app)
	if err != nil {
		return err
	}
	return RejectAttachedURLSet(env, updates)
}

func (s *Store) attachLocked(inst *Instance, app, as string, newProvision bool) map[string]string {
	for i := range inst.Attachments {
		if inst.Attachments[i].App == app {
			url := inst.appURL()
			inst.Attachments[i].URL = url
			for k := range inst.Attachments[i].Env {
				inst.Attachments[i].Env[k] = url
			}
			if len(inst.Attachments[i].Env) > 0 {
				return cloneEnv(inst.Attachments[i].Env)
			}
			return AttachmentEnv(inst.Attachments[i].As, url)
		}
	}
	env := AttachmentKeys(as, "DATABASE_URL", inst.App, inst.appURL(), func(k string) bool {
		return s.urlKeyTaken(app, k)
	}, newProvision)
	stem := ""
	for k := range env {
		if postgresColorURLKey(k) || (strings.HasSuffix(k, "_URL") && k != "DATABASE_URL") {
			stem = strings.TrimSuffix(k, "_URL")
			break
		}
	}
	if stem == "" {
		stem = attachmentName(as)
	}
	inst.Attachments = append(inst.Attachments, Attachment{App: app, As: stem, URL: inst.appURL(), Env: env})
	return cloneEnv(env)
}

func (s *Store) urlKeyTaken(app, key string) bool {
	for _, inst := range s.byID {
		if inst == nil {
			continue
		}
		for _, a := range inst.Attachments {
			if a.App != app {
				continue
			}
			if a.As+"_URL" == key {
				return true
			}
			if _, ok := a.Env[key]; ok {
				return true
			}
		}
	}
	return false
}

func (s *Store) catchUpLocked(fol *Instance) {
	if fol.LeaderID == "" {
		fol.LagBytes = 0
		return
	}
	leader := s.byID[fol.LeaderID]
	if leader == nil {
		return
	}
	fol.Databases = append([]Database(nil), leader.Databases...)
	fol.Users = append([]User(nil), leader.Users...)
	fol.Rows = append([]Row(nil), leader.Rows...)
	fol.applied = leader.seq
	fol.LagBytes = 0
}

func (s *Store) replicateMetaLocked(inst *Instance) {
	for _, fid := range inst.Followers {
		fol := s.byID[fid]
		if fol == nil || fol.Role != RoleFollower || fol.LeaderID != inst.ID || fol.LagBytes > 0 {
			continue
		}
		fol.Databases = append([]Database(nil), inst.Databases...)
		fol.Users = append([]User(nil), inst.Users...)
	}
}

func (i *Instance) visibleTo(app string) bool {
	if app == "" {
		return false
	}
	if i.Tenant == app {
		return true
	}
	for _, a := range i.Attachments {
		if a.App == app {
			return true
		}
	}
	return false
}

// ConnectionURL is the app role URL for this instance.
func (i *Instance) ConnectionURL() string { return i.appURL() }

// MaintenanceURL is the TCP URL the plugin API uses for CREATE DATABASE,
// CREATE ROLE, and DROP ROLE. Isolated instances revoke CONNECT on the
// postgres catalog database, so this is the tenant database — not /postgres.
func (i *Instance) MaintenanceURL() string {
	return i.appURL()
}

// InstanceFromEnv rebuilds a live isolated instance from its Flynn app release.
// Follow uses this when the API process no longer has the in-memory leader.
// Tenant apps (app-one, shop) are not instances even when they carry
// DATABASE_URL or FLYNN_POSTGRES pointing at the datastore.
func InstanceFromEnv(id, app string, env map[string]string) *Instance {
	if env == nil {
		return nil
	}
	if strings.TrimSpace(env["FLYNN_POSTGRES"]) == "" && strings.TrimSpace(env["POSTGRES_URL"]) == "" && strings.TrimSpace(env["DATABASE_URL"]) == "" && strings.TrimSpace(env["POSTGRES_USER"]) == "" && strings.TrimSpace(env["PGUSER"]) == "" {
		return nil
	}
	name := firstNonEmpty(env["FLYNN_POSTGRES"], app)
	if !IsolatedInstanceApp(name) {
		return nil
	}
	if strings.TrimSpace(app) != "" && !IsolatedInstanceApp(app) {
		return nil
	}
	resourceID := strings.TrimSpace(env[ResourceIDEnv])
	db := firstNonEmpty(env["POSTGRES_DB"], env["PGDATABASE"])
	user := firstNonEmpty(env["POSTGRES_USER"], env["PGUSER"])
	pass := firstNonEmpty(env["POSTGRES_PASSWORD"], env["PGPASSWORD"])
	host := strings.TrimSpace(env["PGHOST"])
	for _, key := range []string{"POSTGRES_URL", "DATABASE_URL"} {
		raw := strings.TrimSpace(env[key])
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if key == "DATABASE_URL" && !strings.HasPrefix(strings.ToLower(u.Scheme), "postgres") {
			continue
		}
		if user == "" && u.User != nil {
			user = u.User.Username()
			if pass == "" {
				pass, _ = u.User.Password()
			}
		} else if pass == "" && u.User != nil {
			pass, _ = u.User.Password()
		}
		if host == "" {
			host = u.Hostname()
		}
		if db == "" {
			db = strings.Trim(u.Path, "/")
		}
	}
	if host == "" && name != "" {
		host = "leader." + name + ".discoverd"
	}
	inst := &Instance{
		ID:            firstNonEmpty(resourceID, id, name),
		App:           name,
		AppUser:       user,
		AppPassword:   pass,
		Nodes:         DefaultNodes,
		Role:          RolePrimary,
		ServiceHost:   host,
		EngineVersion: firstNonEmpty(env["ENGINE_VERSION"], env["POSTGRES_VERSION"]),
	}
	if db != "" {
		inst.Databases = []Database{{Name: db}}
	}
	role := strings.TrimSpace(env["POSTGRES_ROLE"])
	if strings.EqualFold(role, "primary") || strings.EqualFold(role, "standalone") {
		inst.Role = RolePrimary
		inst.ReadOnly = false
		inst.LeaderID = ""
	} else if strings.TrimSpace(env["POSTGRES_PRIMARY_URL"]) != "" || strings.EqualFold(role, "follower") || strings.TrimSpace(env["POSTGRES_LEADER"]) != "" {
		inst.Role = RoleFollower
		inst.ReadOnly = true
		inst.LeaderID = firstNonEmpty(env["POSTGRES_LEADER"], inst.LeaderID)
	}
	return inst
}

func (i *Instance) appURL() string {
	db := "postgres"
	if len(i.Databases) > 0 && i.Databases[0].Name != "" {
		db = i.Databases[0].Name
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(i.AppUser, i.AppPassword),
		Host:     i.ServiceHost + ":5432",
		Path:     "/" + db,
		RawQuery: "sslmode=require",
	}
	return u.String()
}

func (i *Instance) snapshot() *Instance {
	out := *i
	out.Databases = append([]Database(nil), i.Databases...)
	out.Users = append([]User(nil), i.Users...)
	out.Rows = append([]Row(nil), i.Rows...)
	out.Attachments = append([]Attachment(nil), i.Attachments...)
	out.Followers = append([]string(nil), i.Followers...)
	return &out
}

func (i *Instance) HasCredential(secret string) bool {
	if secret == "" {
		return false
	}
	if i.SuperuserPassword == secret || i.AppPassword == secret {
		return true
	}
	for _, u := range i.Users {
		if u.Password == secret {
			return true
		}
	}
	for _, a := range i.Attachments {
		if strings.Contains(a.URL, secret) {
			return true
		}
	}
	return false
}

func defaultRuntime(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "standard-1"
	}
	return name
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func hostOf(hostport string) string {
	if i := strings.IndexByte(hostport, ':'); i >= 0 {
		return hostport[:i]
	}
	return hostport
}

func removeID(ids []string, id string) []string {
	out := ids[:0]
	for _, cur := range ids {
		if cur != id {
			out = append(out, cur)
		}
	}
	return out
}

func removeFollowerRef(ids []string, fol *Instance) []string {
	if fol == nil {
		return ids
	}
	out := ids[:0]
	for _, cur := range ids {
		if cur == "" {
			continue
		}
		if cur == fol.ID || cur == fol.App {
			continue
		}
		out = append(out, cur)
	}
	return out
}

func cloneEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
