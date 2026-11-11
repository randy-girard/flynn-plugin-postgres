package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// BackupInterval is the steady gap between successful dumps of one primary.
	BackupInterval = 24 * time.Hour
	// BackupGrace is how long past BackupInterval a success still counts.
	// A miss is strictly older than BackupInterval+BackupGrace.
	BackupGrace = time.Hour
	// BackupKeep is how many successful timestamps one resource retains.
	BackupKeep = 7
	// BackupHostCap is how many dumps may run at once on one host.
	BackupHostCap = 1
	// BackupTick is how often the leader looks for due primaries. There is no cron.
	BackupTick = time.Minute
	// BackupRunningStale is how long a running attempt may go without a
	// heartbeat before another attempt may take the lock.
	BackupRunningStale = 2 * time.Minute
	// BackupHeartbeatEvery is how often a live dump refreshes that lock.
	BackupHeartbeatEvery = 30 * time.Second
)

var (
	ErrBackupBusy      = errors.New("a backup is already running for this database")
	ErrBackupBackend   = errors.New("backups are unavailable until an administrator configures object storage for this cluster")
	ErrBackupNotListed = errors.New("that timestamp is not a backup of this database")
	ErrBackupFollower  = errors.New("backups run on the primary")
	ErrBackupLock      = errors.New("backup lock lost")
)

// BackupDestination is the blobstore the cluster already uses. Secrets are
// not copied onto it.
type BackupDestination struct {
	Backend   string `json:"backend"`
	Bucket    string `json:"bucket,omitempty"`
	Container string `json:"container,omitempty"`
	Region    string `json:"region,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Allowed   bool   `json:"allowed"`
}

// BackupTarget is one primary. ResourceID is the controller resource id.
// ConnURL is used only inside this process. It is not a job argument.
type BackupTarget struct {
	ResourceID   string
	OwnerAccount string
	App          string
	Role         string
	HostID       string
	ConnURL      string
}

// BackupStatus is the latest attempt for one resource. LastSuccessAt stays
// put when a later attempt fails.
type BackupStatus struct {
	ResourceID           string    `json:"resource_id"`
	AttemptState         string    `json:"attempt_state,omitempty"`
	AttemptStartedAt     time.Time `json:"attempt_started_at,omitempty"`
	AttemptFinishedAt    time.Time `json:"attempt_finished_at,omitempty"`
	AttemptError         string    `json:"attempt_error,omitempty"`
	AttemptTimestamp     string    `json:"attempt_timestamp,omitempty"`
	HeartbeatAt          time.Time `json:"heartbeat_at,omitempty"`
	LastSuccessAt        time.Time `json:"last_success_at,omitempty"`
	LastSuccessTimestamp string    `json:"last_success_timestamp,omitempty"`
}

// BackupDump is one timestamp this job stored, with the databases in it.
type BackupDump struct {
	Timestamp string   `json:"timestamp"`
	Databases []string `json:"databases"`
}

// BackupResult is a finished successful dump.
type BackupResult struct {
	Timestamp      string
	Databases      []string
	RetentionError error
}

// BlobStore is the platform blobstore. Keys come from BackupObjectKey.
type BlobStore interface {
	Put(ctx context.Context, key string, body io.Reader) error
	Head(ctx context.Context, key string) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// StatusStore is the controller catalog. Dump bytes are not stored here.
type StatusStore interface {
	Begin(resourceID, timestamp string, now time.Time) (BackupStatus, bool, error)
	Heartbeat(resourceID, timestamp string, now time.Time) error
	Finish(resourceID, timestamp, state, errMsg string, databases []string, now time.Time) error
	Fail(resourceID, errMsg string, now time.Time) error
	Get(resourceID string) (BackupStatus, error)
	ListDumps(resourceID string) ([]BackupDump, error)
	DeleteDump(resourceID, timestamp string) error
}

// DumpClient runs pg_dump and pg_restore. Implementations must not put the
// URL password on the process argument list.
type DumpClient interface {
	ListDatabases(ctx context.Context, connURL string) ([]string, error)
	DumpDatabase(ctx context.Context, connURL, database string, w io.Writer) error
	RestoreDatabase(ctx context.Context, connURL, database string, r io.Reader) error
}

// BackupInput is one backup or the refusal to start one.
type BackupInput struct {
	Target      BackupTarget
	Now         time.Time
	Destination BackupDestination
	Blobs       BlobStore
	Status      StatusStore
	Dump        DumpClient
}

// BackupCandidate is one primary the scheduler may admit.
type BackupCandidate struct {
	ResourceID string
	HostID     string
	Due        bool
	Running    bool
}

// PGCommand is a pg_dump or pg_restore invocation. Password is for
// PGPASSWORD or a pgpass file, never for Args.
type PGCommand struct {
	Args     []string
	Password string
}

// BackupMiss is true when there is no success, or the last success is
// strictly older than 24h plus grace. Tenancy does not change this.
func BackupMiss(lastSuccess, now time.Time) bool {
	if lastSuccess.IsZero() {
		return true
	}
	return now.UTC().Sub(lastSuccess.UTC()) > BackupInterval+BackupGrace
}

// BackupDue is true for a first dump, for the daily phase slot, and for a
// catch-up once the last success is older than 24h plus grace.
func BackupDue(lastSuccess, now time.Time, resourceID string) bool {
	now = now.UTC()
	if lastSuccess.IsZero() {
		return true
	}
	last := lastSuccess.UTC()
	if now.Sub(last) > BackupInterval+BackupGrace {
		return true
	}
	if now.Sub(last) < BackupInterval {
		return false
	}
	slot := NextBackupSlot(last.Add(BackupInterval), resourceID)
	return !now.Before(slot)
}

// BackupPhase is hash(resource id) mod 24h. Resources do not share midnight.
func BackupPhase(resourceID string) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(resourceID))
	mod := uint64(BackupInterval)
	if mod == 0 {
		return 0
	}
	return time.Duration(h.Sum64() % mod)
}

// NextBackupSlot is the next phase boundary at or after earliest.
func NextBackupSlot(earliest time.Time, resourceID string) time.Time {
	earliest = earliest.UTC()
	phase := BackupPhase(resourceID)
	epoch := time.Unix(0, 0).UTC()
	delta := earliest.Sub(epoch) - phase
	var n int64
	if delta > 0 {
		n = int64(delta / BackupInterval)
		if delta%BackupInterval != 0 {
			n++
		}
	}
	slot := epoch.Add(phase + time.Duration(n)*BackupInterval)
	if slot.Before(earliest) {
		slot = slot.Add(BackupInterval)
	}
	return slot
}

// BackupTimestamp is the object path segment for t. It sorts in time order.
func BackupTimestamp(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format("20060102T150405Z")
}

// ParseBackupTimestamp accepts only the form BackupTimestamp writes.
func ParseBackupTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	t, err := time.Parse("20060102T150405Z", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid backup timestamp")
	}
	if t.UTC().Format("20060102T150405Z") != s {
		return time.Time{}, fmt.Errorf("invalid backup timestamp")
	}
	return t.UTC(), nil
}

// BackupOwnerSegment is the first path segment under backups/. An empty
// owner_account is cluster, the same shared bucket the rest of the controller
// uses for an unset owner.
func BackupOwnerSegment(owner string) (string, error) {
	return backupSegment(owner, "cluster")
}

func backupSegment(raw, emptyLabel string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		s = emptyLabel
	}
	if s == "" || strings.Contains(s, "/") || strings.Contains(s, "\\") || strings.Contains(s, "..") || s == "." {
		return "", fmt.Errorf("invalid backup path segment")
	}
	return s, nil
}

// BackupObjectKey is backups/<owner>/<resource UUID>/<timestamp>/<database>.
// Each object is one custom-format database. Two resources do not share one.
func BackupObjectKey(owner, resourceID, timestamp, database string) (string, error) {
	ownerSeg, err := BackupOwnerSegment(owner)
	if err != nil {
		return "", err
	}
	resSeg, err := backupSegment(resourceID, "")
	if err != nil {
		return "", err
	}
	if _, err := ParseBackupTimestamp(timestamp); err != nil {
		return "", err
	}
	if err := ValidDatabaseName(database); err != nil {
		return "", err
	}
	return "backups/" + ownerSeg + "/" + resSeg + "/" + timestamp + "/" + database, nil
}

// FilterBackupDatabases drops the catalog database and templates. Any other
// name that is not a Flynn database identifier fails the backup.
func FilterBackupDatabases(names []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		switch strings.ToLower(n) {
		case "template0", "template1", "postgres":
			continue
		}
		if err := ValidDatabaseName(n); err != nil {
			return nil, fmt.Errorf("database %q cannot be backed up: %w", n, err)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// DumpBackendAllowed is false for an empty default, the name postgres, or a
// parsed type that is missing or postgres. Tenancy is not the switch.
func DumpBackendAllowed(defaultName, backendType string) bool {
	name := strings.TrimSpace(defaultName)
	typ := strings.TrimSpace(backendType)
	if name == "" || strings.EqualFold(name, "postgres") {
		return false
	}
	if typ == "" || strings.EqualFold(typ, "postgres") {
		return false
	}
	return true
}

// BackupDestinationFromEnv reads DEFAULT_BACKEND and that backend's
// non-secret fields. A postgres or unset default is not allowed, in either
// tenancy mode.
func BackupDestinationFromEnv(env map[string]string) BackupDestination {
	name := strings.TrimSpace(env["DEFAULT_BACKEND"])
	if name == "" {
		name = "postgres"
	}
	info := parseBackendTokens(env["BACKEND_"+strings.ToUpper(name)])
	typ := strings.TrimSpace(info["backend"])
	if strings.EqualFold(name, "postgres") {
		typ = "postgres"
	}
	dest := BackupDestination{
		Backend: typ,
		Allowed: DumpBackendAllowed(name, typ),
	}
	if !dest.Allowed {
		if dest.Backend == "" {
			dest.Backend = "postgres"
		}
		return dest
	}
	dest.Bucket = info["bucket"]
	dest.Container = info["container"]
	dest.Region = info["region"]
	dest.Endpoint = publicEndpoint(info["endpoint"])
	return dest
}

func parseBackendTokens(raw string) map[string]string {
	out := map[string]string{}
	for _, token := range strings.Fields(raw) {
		kv := strings.SplitN(token, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		switch k {
		case "secret_access_key", "account_key", "key", "access_key_id":
			continue
		}
		out[k] = kv[1]
	}
	return out
}

func publicEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	u.User = nil
	return u.String()
}

// TimestampsToDelete returns timestamps past the newest keep, plus any
// timestamp that is not in the compact UTC form.
func TimestampsToDelete(stamps []string, keep int) []string {
	if keep < 0 {
		keep = 0
	}
	type item struct {
		raw string
		at  time.Time
	}
	var ok []item
	var drop []string
	seen := map[string]bool{}
	for _, s := range stamps {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		t, err := ParseBackupTimestamp(s)
		if err != nil {
			drop = append(drop, s)
			continue
		}
		ok = append(ok, item{raw: s, at: t})
	}
	sort.Slice(ok, func(i, j int) bool {
		if ok[i].at.Equal(ok[j].at) {
			return ok[i].raw > ok[j].raw
		}
		return ok[i].at.After(ok[j].at)
	})
	if len(ok) > keep {
		for _, it := range ok[keep:] {
			drop = append(drop, it.raw)
		}
	}
	return drop
}

// AdmitBackups returns due resource ids, at most cap per host, in resource
// id order. A running attempt on a host uses one of the slots.
func AdmitBackups(in []BackupCandidate, cap int) []string {
	if cap < 1 {
		cap = 1
	}
	running := map[string]int{}
	var due []BackupCandidate
	for _, c := range in {
		if c.Running {
			running[c.HostID]++
		}
		if c.Due && !c.Running && c.ResourceID != "" {
			due = append(due, c)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].ResourceID < due[j].ResourceID })
	admitted := map[string]int{}
	var out []string
	for _, c := range due {
		if running[c.HostID]+admitted[c.HostID] >= cap {
			continue
		}
		admitted[c.HostID]++
		out = append(out, c.ResourceID)
	}
	return out
}

// PrimaryRole reports whether a stored role is a backup source. Followers
// and deposed primaries are not.
func PrimaryRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "follower", "deposed":
		return false
	default:
		return true
	}
}

// PgDumpCommand builds pg_dump --format=custom for one database. The
// password is returned separately so it is not placed on argv.
func PgDumpCommand(connURL, database string) (PGCommand, error) {
	raw, password, err := libpqCommandURL(connURL, database)
	if err != nil {
		return PGCommand{}, err
	}
	return PGCommand{
		Args:     []string{"pg_dump", "--no-owner", "--format=custom", raw},
		Password: password,
	}, nil
}

// PsqlStatement runs one statement against database. The password is not placed on argv.
func PsqlStatement(connURL, database string, extra ...string) (PGCommand, error) {
	raw, password, err := libpqCommandURL(connURL, database)
	if err != nil {
		return PGCommand{}, err
	}
	args := []string{"psql", raw, "-v", "ON_ERROR_STOP=1"}
	args = append(args, extra...)
	return PGCommand{Args: args, Password: password}, nil
}

// PgRestoreCommand builds a restore bound to one database. --clean stays
// because the target database is explicit. --exit-on-error stops on the
// first error. The password is not placed on argv.
func PgRestoreCommand(connURL, database string) (PGCommand, error) {
	raw, password, err := libpqCommandURL(connURL, database)
	if err != nil {
		return PGCommand{}, err
	}
	return PGCommand{
		Args: []string{
			"pg_restore",
			"--no-owner",
			"--clean",
			"--if-exists",
			"--exit-on-error",
			"--dbname",
			raw,
		},
		Password: password,
	}, nil
}

func libpqCommandURL(connURL, database string) (string, string, error) {
	if err := ValidDatabaseName(database); err != nil {
		return "", "", err
	}
	u, err := url.Parse(strings.TrimSpace(connURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("missing postgres URL")
	}
	var password string
	if u.User != nil {
		password, _ = u.User.Password()
		user := u.User.Username()
		if user != "" {
			u.User = url.User(user)
		} else {
			u.User = nil
		}
	}
	u.Path = "/" + database
	return u.String(), password, nil
}

// RedactSecret removes secret from msg. An empty secret leaves msg as it is.
func RedactSecret(msg, secret string) string {
	if secret == "" || msg == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "[redacted]")
}

// ListedTimestamp reports whether timestamp is one of the catalog dumps.
func ListedTimestamp(dumps []BackupDump, timestamp string) bool {
	timestamp = strings.TrimSpace(timestamp)
	if timestamp == "" {
		return false
	}
	for _, d := range dumps {
		if d.Timestamp == timestamp && len(d.Databases) > 0 {
			return true
		}
	}
	return false
}

// RunBackup dumps every logical database on a primary into the blobstore and
// records the timestamp. A postgres blobstore records a failure and does not
// Put. A partial timestamp is deleted and is not catalogued.
func RunBackup(ctx context.Context, in BackupInput) (BackupResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !PrimaryRole(in.Target.Role) {
		return BackupResult{}, ErrBackupFollower
	}
	if in.Status == nil || in.Dump == nil {
		return BackupResult{}, fmt.Errorf("backup is not configured")
	}
	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !in.Destination.Allowed {
		_ = in.Status.Fail(in.Target.ResourceID, ErrBackupBackend.Error(), now)
		return BackupResult{}, ErrBackupBackend
	}
	if in.Blobs == nil {
		return BackupResult{}, fmt.Errorf("backup is not configured")
	}
	ts := BackupTimestamp(now)
	if _, started, err := in.Status.Begin(in.Target.ResourceID, ts, now); err != nil {
		return BackupResult{}, err
	} else if !started {
		return BackupResult{}, ErrBackupBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	beatErr := make(chan error, 1)
	go beatUntil(ctx, in.Status, in.Target.ResourceID, ts, beatErr)
	names, err := in.Dump.ListDatabases(ctx, in.Target.ConnURL)
	if err != nil {
		return failBackup(ctx, in, ts, nil, err)
	}
	names, err = FilterBackupDatabases(names)
	if err != nil {
		return failBackup(ctx, in, ts, nil, err)
	}
	if len(names) == 0 {
		return failBackup(ctx, in, ts, nil, fmt.Errorf("no databases to back up"))
	}
	var written []string
	for _, db := range names {
		select {
		case <-ctx.Done():
			return failBackup(ctx, in, ts, written, ctx.Err())
		case err := <-beatErr:
			return failBackup(ctx, in, ts, written, err)
		default:
		}
		key, err := BackupObjectKey(in.Target.OwnerAccount, in.Target.ResourceID, ts, db)
		if err != nil {
			return failBackup(ctx, in, ts, written, err)
		}
		if err := putDatabase(ctx, in, key, db); err != nil {
			return failBackup(ctx, in, ts, append(written, key), err)
		}
		size, err := in.Blobs.Head(ctx, key)
		if err != nil {
			return failBackup(ctx, in, ts, append(written, key), err)
		}
		if size <= 0 {
			return failBackup(ctx, in, ts, append(written, key), fmt.Errorf("backup object %s is empty", db))
		}
		written = append(written, key)
	}
	if err := in.Status.Finish(in.Target.ResourceID, ts, "succeeded", "", names, now); err != nil {
		return failBackup(ctx, in, ts, written, err)
	}
	result := BackupResult{Timestamp: ts, Databases: names}
	result.RetentionError = pruneBackups(ctx, in, ts)
	return result, nil
}

func beatUntil(ctx context.Context, status StatusStore, resourceID, timestamp string, errCh chan error) {
	t := time.NewTicker(BackupHeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := status.Heartbeat(resourceID, timestamp, now.UTC()); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}
}

func putDatabase(ctx context.Context, in BackupInput, key, database string) error {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		err := in.Dump.DumpDatabase(ctx, in.Target.ConnURL, database, pw)
		_ = pw.CloseWithError(err)
		errCh <- err
	}()
	putErr := in.Blobs.Put(ctx, key, pr)
	dumpErr := <-errCh
	_ = pr.Close()
	if dumpErr != nil {
		return dumpErr
	}
	return putErr
}

func failBackup(ctx context.Context, in BackupInput, ts string, keys []string, cause error) (BackupResult, error) {
	for _, key := range keys {
		_ = in.Blobs.Delete(context.Background(), key)
	}
	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_ = in.Status.Finish(in.Target.ResourceID, ts, "failed", cause.Error(), nil, now)
	return BackupResult{}, cause
}

func pruneBackups(ctx context.Context, in BackupInput, keepTimestamp string) error {
	dumps, err := in.Status.ListDumps(in.Target.ResourceID)
	if err != nil {
		return err
	}
	var stamps []string
	byStamp := map[string][]string{}
	for _, d := range dumps {
		stamps = append(stamps, d.Timestamp)
		byStamp[d.Timestamp] = append(byStamp[d.Timestamp], d.Databases...)
	}
	var errs []string
	for _, ts := range TimestampsToDelete(stamps, BackupKeep) {
		if ts == keepTimestamp {
			continue
		}
		for _, db := range byStamp[ts] {
			key, err := BackupObjectKey(in.Target.OwnerAccount, in.Target.ResourceID, ts, db)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			if err := in.Blobs.Delete(ctx, key); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if err := in.Status.DeleteDump(in.Target.ResourceID, ts); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("prune backups: %s", strings.Join(errs, "; "))
	}
	return nil
}

// RunRestore copies a catalogued timestamp back into the same primary.
// timestamp must be one ListDumps returned. A raw object id or path is not
// accepted.
func RunRestore(ctx context.Context, in BackupInput, timestamp string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !PrimaryRole(in.Target.Role) {
		return ErrBackupFollower
	}
	if in.Status == nil || in.Blobs == nil || in.Dump == nil {
		return fmt.Errorf("backup is not configured")
	}
	timestamp = strings.TrimSpace(timestamp)
	if _, err := ParseBackupTimestamp(timestamp); err != nil {
		return ErrBackupNotListed
	}
	dumps, err := in.Status.ListDumps(in.Target.ResourceID)
	if err != nil {
		return err
	}
	var databases []string
	for _, d := range dumps {
		if d.Timestamp == timestamp {
			databases = append(databases, d.Databases...)
		}
	}
	if len(databases) == 0 {
		return ErrBackupNotListed
	}
	sort.Strings(databases)
	for _, db := range databases {
		key, err := BackupObjectKey(in.Target.OwnerAccount, in.Target.ResourceID, timestamp, db)
		if err != nil {
			return err
		}
		body, err := in.Blobs.Get(ctx, key)
		if err != nil {
			return err
		}
		err = in.Dump.RestoreDatabase(ctx, in.Target.ConnURL, db, body)
		_ = body.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// RunningFresh reports whether a running attempt still holds the lock.
func RunningFresh(st BackupStatus, now time.Time) bool {
	if st.AttemptState != "running" {
		return false
	}
	if st.HeartbeatAt.IsZero() {
		return false
	}
	return now.UTC().Sub(st.HeartbeatAt.UTC()) <= BackupRunningStale
}
