package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

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
	ErrNotFound       = errors.New("postgres instance not found")
	ErrReadOnly       = errors.New("follower is read-only")
	ErrFollowFollower = errors.New("a follower cannot follow another follower")
	ErrNotFollower    = errors.New("only a follower can be promoted or unfollowed")
	ErrNoResize       = errors.New("no in-place resize or upgrade; create a follower, pg:wait until caught up, then pg:promote")
)

// User is a role that exists only in one instance's state.
type User struct {
	Name     string
	Password string
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
	Role              Role
	LeaderID          string
	Mode              ReplicationMode
	ReadOnly          bool
	LagBytes          int64
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
	ID        string          `json:"id"`
	Role      Role            `json:"role"`
	LeaderID  string          `json:"leader_id,omitempty"`
	Followers []string        `json:"followers,omitempty"`
	LagBytes  int64           `json:"lag_bytes"`
	ReadOnly  bool            `json:"read_only"`
	Nodes     int             `json:"nodes"`
	Runtime   string          `json:"runtime"`
	Mode      ReplicationMode `json:"replication,omitempty"`
	App       string          `json:"app"`
	Volume    string          `json:"volume"`
	Host      string          `json:"host"`
}

// ProvisionRequest creates a primary, or a follower when Follow is set.
type ProvisionRequest struct {
	Tenant  string
	App     string
	As      string
	Follow  string
	Mode    ReplicationMode
	Runtime string
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
	// LoadMissing loads a live instance (pg-orchid-xkhthp) after this
	// process restarts. Follow looks up by app name; the in-memory map is empty.
	LoadMissing func(idOrApp string) *Instance
}

// NewStore returns a store aimed at this plugin's discoverd host.
func NewStore() *Store {
	return &Store{
		byID:        map[string]*Instance{},
		providerURL: ProviderURL(),
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
		App:               s.uniqueApp("pg"),
		Volume:            "vol-" + id,
		Superuser:         "su_" + id,
		SuperuserPassword: "supw_" + newID(),
		AppUser:           "app_" + id,
		AppPassword:       "apppw_" + newID(),
		Nodes:             DefaultNodes,
		Runtime:           defaultRuntime(req.Runtime),
		Role:              RolePrimary,
	}
	inst.ServiceHost = "leader." + inst.App + ".discoverd"
	if hostOf(inst.ServiceHost) == PlatformApplianceHost {
		return nil, nil, ErrPlatformAppliance
	}
	inst.Databases = []Database{{Name: DefaultDatabaseName(inst.App)}}

	if leader != nil {
		mode := req.Mode
		if mode == "" {
			mode = ModeStreaming
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
		env = s.attachLocked(inst, req.App, req.As)
	}
	return inst.snapshot(), env, nil
}

// lookupLocked finds a resource by id or by isolated app name (pg-harbor-xxxxxx).
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
	s.byID[inst.ID] = inst
	return inst
}

func (s *Store) findLocked(idOrApp string) *Instance {
	if inst := s.byID[idOrApp]; inst != nil {
		return inst
	}
	for _, inst := range s.byID {
		if inst != nil && inst.App == idOrApp {
			return inst
		}
	}
	return nil
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

// Info is pg:info for one resource.
func (s *Store) Info(id string) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
	if inst == nil {
		return Info{}, ErrNotFound
	}
	followers := append([]string(nil), inst.Followers...)
	return Info{
		ID:        inst.ID,
		Role:      inst.Role,
		LeaderID:  inst.LeaderID,
		Followers: followers,
		LagBytes:  inst.LagBytes,
		ReadOnly:  inst.ReadOnly,
		Nodes:     inst.Nodes,
		Runtime:   inst.Runtime,
		Mode:      inst.Mode,
		App:       inst.App,
		Volume:    inst.Volume,
		Host:      inst.ServiceHost,
	}, nil
}

// AddDatabase creates a database that exists only on this instance.
func (s *Store) AddDatabase(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("database name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
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
func (s *Store) AddUser(id, name, password string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("user name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
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
	inst.Users = append(inst.Users, User{Name: name, Password: password})
	s.replicateMetaLocked(inst)
	return nil
}

// Users returns roles stored on this instance. Superuser is not included.
func (s *Store) Users(id string) ([]User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
	if inst == nil {
		return nil, ErrNotFound
	}
	return append([]User(nil), inst.Users...), nil
}

// Databases returns database names stored on this instance.
func (s *Store) Databases(id string) ([]Database, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.byID[id]
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
	if bytes == 0 {
		s.catchUpLocked(inst)
	}
	return nil
}

// Wait blocks until follower lag is zero.
func (s *Store) Wait(ctx context.Context, id string) error {
	for {
		s.mu.Lock()
		inst := s.byID[id]
		if inst == nil {
			s.mu.Unlock()
			return ErrNotFound
		}
		lag := inst.LagBytes
		following := inst.Role == RoleFollower
		s.mu.Unlock()
		if !following || lag == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Promote makes the follower writable, ends the follow, and rewrites the
// previous leader's attachment URLs. The old leader remains its own resource.
func (s *Store) Promote(id string) (*PromoteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fol := s.byID[id]
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
	leader.Followers = removeID(leader.Followers, fol.ID)
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
	fol := s.byID[id]
	if fol == nil {
		return nil, ErrNotFound
	}
	if fol.Role != RoleFollower || fol.LeaderID == "" {
		return nil, ErrNotFollower
	}
	if leader := s.byID[fol.LeaderID]; leader != nil {
		leader.Followers = removeID(leader.Followers, fol.ID)
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
	return cloneEnv(s.attachLocked(inst, app, as)), nil
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
	inst := s.byID[id]
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

func (s *Store) attachLocked(inst *Instance, app, as string) map[string]string {
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
	})
	stem := attachmentName(as)
	if strings.TrimSpace(as) == "" {
		for k := range env {
			if strings.HasSuffix(k, "_DATABASE_URL") {
				stem = strings.TrimSuffix(k, "_URL")
				break
			}
		}
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

// InstanceFromEnv rebuilds a live isolated instance from its Flynn app release.
// Follow uses this when the API process no longer has the in-memory leader.
func InstanceFromEnv(id, app string, env map[string]string) *Instance {
	if env == nil {
		return nil
	}
	if strings.TrimSpace(env["FLYNN_POSTGRES"]) == "" && strings.TrimSpace(env["POSTGRES_URL"]) == "" && strings.TrimSpace(env["POSTGRES_USER"]) == "" {
		return nil
	}
	name := firstNonEmpty(env["FLYNN_POSTGRES"], app)
	db := firstNonEmpty(env["POSTGRES_DB"], env["PGDATABASE"])
	user := env["POSTGRES_USER"]
	pass := env["POSTGRES_PASSWORD"]
	host := ""
	if raw := strings.TrimSpace(env["POSTGRES_URL"]); raw != "" {
		if u, err := url.Parse(raw); err == nil {
			if user == "" && u.User != nil {
				user = u.User.Username()
				pass, _ = u.User.Password()
			}
			host = u.Hostname()
			if db == "" {
				db = strings.Trim(u.Path, "/")
			}
		}
	}
	if host == "" {
		host = "leader." + name + ".discoverd"
	}
	inst := &Instance{
		ID:          firstNonEmpty(id, name),
		App:         name,
		AppUser:     user,
		AppPassword: pass,
		Nodes:       DefaultNodes,
		Role:        RolePrimary,
		ServiceHost: host,
	}
	if db != "" {
		inst.Databases = []Database{{Name: db}}
	}
	if strings.TrimSpace(env["POSTGRES_PRIMARY_URL"]) != "" {
		inst.Role = RoleFollower
		inst.ReadOnly = true
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
