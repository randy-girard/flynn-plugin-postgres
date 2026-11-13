package postgres

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestBackupObjectKeySeparatesResourcesAndDatabases(t *testing.T) {
	a, err := BackupObjectKey("user:ada", "11111111-1111-1111-1111-111111111111", "20261111T120000Z", "appdb")
	if err != nil {
		t.Fatal(err)
	}
	b, err := BackupObjectKey("user:ada", "22222222-2222-2222-2222-222222222222", "20261111T120000Z", "appdb")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two resources shared a key: %s", a)
	}
	otherDB, err := BackupObjectKey("user:ada", "11111111-1111-1111-1111-111111111111", "20261111T120000Z", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if otherDB == a {
		t.Fatal("two databases shared a key")
	}
	emptyOwner, err := BackupObjectKey("", "11111111-1111-1111-1111-111111111111", "20261111T120000Z", "appdb")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(emptyOwner, "/cluster/") {
		t.Fatalf("empty owner = %s", emptyOwner)
	}
	if _, err := BackupObjectKey("user/ada", "res", "20261111T120000Z", "appdb"); err == nil {
		t.Fatal("slash in owner was accepted")
	}
	if _, err := BackupObjectKey("user:ada", "../res", "20261111T120000Z", "appdb"); err == nil {
		t.Fatal("dotdot resource was accepted")
	}
	if _, err := BackupObjectKey("user:ada", "res", "not-a-time", "appdb"); err == nil {
		t.Fatal("raw path timestamp was accepted")
	}
}

func TestPgDumpCommandKeepsPasswordOffArgv(t *testing.T) {
	const password = "s3cret-pass"
	raw := "postgres://appuser:" + password + "@leader.postgresql-oak-10000.discoverd:5432/appdb?sslmode=require"
	cmd, err := PgDumpCommand(raw, "shop")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if cmd.Password != password {
		t.Fatalf("password = %q", cmd.Password)
	}
	if strings.Contains(joined, password) {
		t.Fatalf("argv contains the password: %s", joined)
	}
	if !strings.Contains(joined, "pg_dump") || !strings.Contains(joined, "--format=custom") || !strings.Contains(joined, "/shop") {
		t.Fatalf("args = %#v", cmd.Args)
	}
	if strings.Contains(joined, "pg_dumpall") {
		t.Fatal("dump uses pg_dumpall")
	}
	psql, err := PsqlStatement(raw, "shop", "-t", "-A", "-c", "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(psql.Args, " "), password) || psql.Password != password {
		t.Fatalf("psql command leaked or dropped the password: %#v", psql)
	}
	restore, err := PgRestoreCommand(raw, "shop")
	if err != nil {
		t.Fatal(err)
	}
	rjoined := strings.Join(restore.Args, " ")
	if strings.Contains(rjoined, password) {
		t.Fatalf("restore argv contains the password: %s", rjoined)
	}
	for _, flag := range []string{"--exit-on-error", "--clean", "--if-exists", "--dbname"} {
		if !strings.Contains(rjoined, flag) {
			t.Fatalf("restore missing %s: %s", flag, rjoined)
		}
	}
}

func TestFilterBackupDatabasesSkipsCatalogAndRejectsOddNames(t *testing.T) {
	got, err := FilterBackupDatabases([]string{"postgres", "template0", "template1", "shop", "appdb", "shop", ""})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "appdb,shop" {
		t.Fatalf("got %v", got)
	}
	if _, err := FilterBackupDatabases([]string{"shop", "bad-name"}); err == nil {
		t.Fatal("odd database name was skipped instead of failing the backup")
	}
}

func TestBackupBackendErrorIsForAppUsers(t *testing.T) {
	msg := ErrBackupBackend.Error()
	if strings.Contains(msg, "flynn-host") || strings.Contains(msg, "blobstore:set") {
		t.Fatalf("app-facing backup error names a host command: %s", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "administrator") {
		t.Fatalf("app-facing backup error should say an administrator has to configure storage: %s", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "blobstore") {
		t.Fatalf("app-facing backup error should name the blobstore: %s", msg)
	}
}

func TestDumpBackendAllowedIgnoresTenancy(t *testing.T) {
	cases := []struct {
		name, typ string
		ok        bool
	}{
		{"", "", false},
		{"postgres", "postgres", false},
		{"POSTGRES", "postgres", false},
		{"s3main", "", false},
		{"s3main", "postgres", false},
		{"s3main", "s3", true},
		{"miniomain", "minio", true},
		{"gcsmain", "gcs", true},
		{"azuremain", "azure", true},
	}
	for _, tc := range cases {
		if got := DumpBackendAllowed(tc.name, tc.typ); got != tc.ok {
			t.Fatalf("DumpBackendAllowed(%q, %q) = %v, want %v", tc.name, tc.typ, got, tc.ok)
		}
	}
}

func TestBackupDestinationRedactsSecrets(t *testing.T) {
	env := map[string]string{
		"DEFAULT_BACKEND": "s3main",
		"BACKEND_S3MAIN":  "backend=s3 region=us-east-1 bucket=flynn-blobs endpoint=https://user:secret@s3.example access_key_id=AKIA secret_access_key=super",
	}
	dest := BackupDestinationFromEnv(env)
	if !dest.Allowed || dest.Backend != "s3" || dest.Bucket != "flynn-blobs" || dest.Region != "us-east-1" {
		t.Fatalf("dest = %+v", dest)
	}
	if strings.Contains(dest.Endpoint, "secret") || strings.Contains(dest.Endpoint, "user:") {
		t.Fatalf("endpoint leaked credentials: %s", dest.Endpoint)
	}
	blob, err := io.ReadAll(strings.NewReader(dest.Backend + dest.Bucket + dest.Region + dest.Endpoint))
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)
	if strings.Contains(text, "super") || strings.Contains(text, "AKIA") {
		t.Fatalf("destination contains a secret: %s", text)
	}
	refused := BackupDestinationFromEnv(map[string]string{})
	if refused.Allowed || refused.Backend != "postgres" {
		t.Fatalf("unset backend = %+v", refused)
	}
}

func TestBackupSchedulePhaseCapAndRetention(t *testing.T) {
	id := "11111111-1111-1111-1111-111111111111"
	if BackupMiss(time.Time{}, time.Now()) != true {
		t.Fatal("missing success is a miss")
	}
	start := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	if BackupMiss(start, start.Add(BackupInterval+BackupGrace)) {
		t.Fatal("exactly 24h+grace is not yet a miss")
	}
	if !BackupMiss(start, start.Add(BackupInterval+BackupGrace+time.Second)) {
		t.Fatal("past 24h+grace is a miss")
	}
	if !BackupDue(time.Time{}, start, id) {
		t.Fatal("first dump waits for the daily phase")
	}
	if BackupDue(start, start.Add(23*time.Hour), id) {
		t.Fatal("a dump younger than 24h is due")
	}
	if !BackupDue(start, start.Add(BackupInterval+BackupGrace+time.Second), id) {
		t.Fatal("catch-up after grace did not become due")
	}
	phase := BackupPhase(id)
	other := BackupPhase(id + "-other")
	if phase == other && phase == 0 {
		t.Fatal("phase did not spread")
	}
	admitted := AdmitBackups([]BackupCandidate{
		{ResourceID: "b", HostID: "host-a", Due: true},
		{ResourceID: "a", HostID: "host-a", Due: true},
		{ResourceID: "c", HostID: "host-b", Due: true},
		{ResourceID: "d", HostID: "host-a", Due: false},
		{ResourceID: "e", HostID: "host-a", Running: true},
	}, BackupHostCap)
	if strings.Join(admitted, ",") != "c" {
		t.Fatalf("cap admitted %v; host-a is full with a running dump, host-b should take c", admitted)
	}
	open := AdmitBackups([]BackupCandidate{
		{ResourceID: "b", HostID: "host-a", Due: true},
		{ResourceID: "a", HostID: "host-a", Due: true},
	}, 1)
	if strings.Join(open, ",") != "a" {
		t.Fatalf("stable cap = %v", open)
	}
	stamps := []string{
		"20261101T000000Z",
		"20261102T000000Z",
		"20261103T000000Z",
		"20261104T000000Z",
		"20261105T000000Z",
		"20261106T000000Z",
		"20261107T000000Z",
		"20261108T000000Z",
		"not-a-timestamp",
	}
	drop := TimestampsToDelete(stamps, BackupKeep)
	if strings.Join(drop, ",") != "not-a-timestamp,20261101T000000Z" {
		t.Fatalf("retention drop = %v", drop)
	}
}

func TestRunBackupWritesEachDatabaseAndRefusesPostgresBackend(t *testing.T) {
	now := time.Date(2026, 11, 11, 16, 4, 5, 0, time.UTC)
	target := BackupTarget{
		ResourceID:   "11111111-1111-1111-1111-111111111111",
		OwnerAccount: "user:ada",
		App:          "postgresql-oak-10000",
		Role:         "primary",
		HostID:       "host-a",
		ConnURL:      "postgres://appuser:s3cret-pass@leader.postgresql-oak-10000.discoverd:5432/appdb?sslmode=require",
	}
	blobs := newMemBlobs()
	status := newMemStatus()
	dump := &fakeDump{databases: []string{"postgres", "shop", "appdb"}}
	got, err := RunBackup(context.Background(), BackupInput{
		Target:      target,
		Now:         now,
		Destination: BackupDestination{Backend: "s3", Bucket: "b", Allowed: true},
		Blobs:       blobs,
		Status:      status,
		Dump:        dump,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Timestamp != "20261111T160405Z" {
		t.Fatalf("timestamp %s", got.Timestamp)
	}
	if strings.Join(got.Databases, ",") != "appdb,shop" {
		t.Fatalf("databases %v", got.Databases)
	}
	if len(blobs.objects) != 2 {
		t.Fatalf("objects = %#v", blobs.objects)
	}
	for key, body := range blobs.objects {
		if strings.Contains(key, "s3cret-pass") || strings.Contains(body, "s3cret-pass") {
			t.Fatalf("object leaked the password: %s", key)
		}
		if !strings.Contains(key, "/11111111-1111-1111-1111-111111111111/20261111T160405Z/") {
			t.Fatalf("key %s", key)
		}
	}
	if len(dump.restores) != 0 {
		t.Fatal("backup restored")
	}
	st, err := status.Get(target.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastSuccessTimestamp != got.Timestamp || st.AttemptState != "succeeded" {
		t.Fatalf("status %+v", st)
	}

	refusedBlobs := newMemBlobs()
	refusedStatus := newMemStatus()
	_, err = RunBackup(context.Background(), BackupInput{
		Target:      target,
		Now:         now,
		Destination: BackupDestination{Backend: "postgres", Allowed: false},
		Blobs:       refusedBlobs,
		Status:      refusedStatus,
		Dump:        &fakeDump{databases: []string{"appdb"}},
	})
	if !errors.Is(err, ErrBackupBackend) {
		t.Fatalf("err = %v", err)
	}
	if len(refusedBlobs.objects) != 0 {
		t.Fatalf("postgres backend stored %d objects", len(refusedBlobs.objects))
	}
	refused, err := refusedStatus.Get(target.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if refused.AttemptState != "failed" || !refused.LastSuccessAt.IsZero() {
		t.Fatalf("refusal status %+v", refused)
	}

	_, err = RunBackup(context.Background(), BackupInput{
		Target:      BackupTarget{ResourceID: target.ResourceID, Role: "follower", ConnURL: target.ConnURL},
		Now:         now,
		Destination: BackupDestination{Backend: "s3", Allowed: true},
		Blobs:       blobs,
		Status:      status,
		Dump:        dump,
	})
	if !errors.Is(err, ErrBackupFollower) {
		t.Fatalf("follower err = %v", err)
	}
}

func TestRunBackupDeletesPartialAndKeepsSeven(t *testing.T) {
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)
	target := BackupTarget{
		ResourceID:   "res-1",
		OwnerAccount: "",
		Role:         "primary",
		ConnURL:      "postgres://appuser:pw@leader.postgresql-oak-10000.discoverd:5432/appdb?sslmode=require",
	}
	blobs := newMemBlobs()
	status := newMemStatus()
	dump := &fakeDump{databases: []string{"appdb", "shop"}, failOn: "shop"}
	_, err := RunBackup(context.Background(), BackupInput{
		Target:      target,
		Now:         now,
		Destination: BackupDestination{Backend: "minio", Allowed: true},
		Blobs:       blobs,
		Status:      status,
		Dump:        dump,
	})
	if err == nil {
		t.Fatal("partial dump succeeded")
	}
	if len(blobs.objects) != 0 {
		t.Fatalf("partial objects left behind: %#v", blobs.objects)
	}
	dumps, err := status.ListDumps(target.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dumps) != 0 {
		t.Fatalf("partial timestamp was catalogued: %+v", dumps)
	}

	dump.failOn = ""
	for i := 0; i < 8; i++ {
		when := now.Add(time.Duration(i) * time.Second)
		if _, err := RunBackup(context.Background(), BackupInput{
			Target:      target,
			Now:         when,
			Destination: BackupDestination{Backend: "minio", Allowed: true},
			Blobs:       blobs,
			Status:      status,
			Dump:        dump,
		}); err != nil {
			t.Fatal(err)
		}
	}
	left, err := status.ListDumps(target.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != BackupKeep {
		t.Fatalf("kept %d timestamps: %+v", len(left), left)
	}
	if len(blobs.objects) != BackupKeep*2 {
		t.Fatalf("objects = %d", len(blobs.objects))
	}
}

func TestRunRestoreRejectsUnlistedTimestamps(t *testing.T) {
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)
	target := BackupTarget{
		ResourceID:   "res-1",
		OwnerAccount: "org:acme",
		Role:         "primary",
		ConnURL:      "postgres://appuser:pw@leader.postgresql-oak-10000.discoverd:5432/appdb?sslmode=require",
	}
	blobs := newMemBlobs()
	status := newMemStatus()
	dump := &fakeDump{databases: []string{"appdb"}}
	in := BackupInput{
		Target:      target,
		Now:         now,
		Destination: BackupDestination{Backend: "s3", Allowed: true},
		Blobs:       blobs,
		Status:      status,
		Dump:        dump,
	}
	got, err := RunBackup(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunRestore(context.Background(), in, "../other"); err == nil {
		t.Fatal("raw path was restored")
	}
	if err := RunRestore(context.Background(), in, "20260101T000000Z"); !errors.Is(err, ErrBackupNotListed) {
		t.Fatalf("unlisted err = %v", err)
	}
	if err := RunRestore(context.Background(), in, got.Timestamp); err != nil {
		t.Fatal(err)
	}
	if len(dump.restores) != 1 || dump.restores[0] != "appdb" {
		t.Fatalf("restores %v", dump.restores)
	}
	busy := newMemStatus()
	if _, started, err := busy.Begin(target.ResourceID, got.Timestamp, now); err != nil || !started {
		t.Fatal(err)
	}
	_, err = RunBackup(context.Background(), BackupInput{
		Target:      target,
		Now:         now,
		Destination: in.Destination,
		Blobs:       newMemBlobs(),
		Status:      busy,
		Dump:        dump,
	})
	if !errors.Is(err, ErrBackupBusy) {
		t.Fatalf("busy err = %v", err)
	}
}

func TestRedactSecret(t *testing.T) {
	if got := RedactSecret("pg_dump: s3cret-pass failed", "s3cret-pass"); strings.Contains(got, "s3cret-pass") {
		t.Fatal(got)
	}
}

type memBlobs struct {
	objects map[string]string
}

func newMemBlobs() *memBlobs {
	return &memBlobs{objects: map[string]string{}}
}

func (m *memBlobs) Put(_ context.Context, key string, body io.Reader) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.objects[key] = string(b)
	return nil
}

func (m *memBlobs) Head(_ context.Context, key string) (int64, error) {
	body, ok := m.objects[key]
	if !ok {
		return 0, errors.New("not found")
	}
	return int64(len(body)), nil
}

func (m *memBlobs) Get(_ context.Context, key string) (io.ReadCloser, error) {
	body, ok := m.objects[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func (m *memBlobs) Delete(_ context.Context, key string) error {
	delete(m.objects, key)
	return nil
}

type memStatus struct {
	rows  map[string]BackupStatus
	dumps map[string][]BackupDump
}

func newMemStatus() *memStatus {
	return &memStatus{rows: map[string]BackupStatus{}, dumps: map[string][]BackupDump{}}
}

func (m *memStatus) Begin(resourceID, timestamp string, now time.Time) (BackupStatus, bool, error) {
	cur := m.rows[resourceID]
	if RunningFresh(cur, now) {
		return cur, false, nil
	}
	cur.ResourceID = resourceID
	cur.AttemptState = "running"
	cur.AttemptTimestamp = timestamp
	cur.AttemptStartedAt = now
	cur.HeartbeatAt = now
	cur.AttemptError = ""
	m.rows[resourceID] = cur
	return cur, true, nil
}

func (m *memStatus) Heartbeat(resourceID, timestamp string, now time.Time) error {
	cur, ok := m.rows[resourceID]
	if !ok || cur.AttemptState != "running" || cur.AttemptTimestamp != timestamp {
		return ErrBackupLock
	}
	cur.HeartbeatAt = now
	m.rows[resourceID] = cur
	return nil
}

func (m *memStatus) Finish(resourceID, timestamp, state, errMsg string, databases []string, now time.Time) error {
	cur, ok := m.rows[resourceID]
	if !ok || cur.AttemptState != "running" || cur.AttemptTimestamp != timestamp {
		return ErrBackupLock
	}
	cur.AttemptState = state
	cur.AttemptFinishedAt = now
	cur.AttemptError = errMsg
	cur.HeartbeatAt = now
	if state == "succeeded" {
		cur.LastSuccessAt = now
		cur.LastSuccessTimestamp = timestamp
		m.dumps[resourceID] = append(m.dumps[resourceID], BackupDump{Timestamp: timestamp, Databases: append([]string(nil), databases...)})
	}
	m.rows[resourceID] = cur
	return nil
}

func (m *memStatus) Fail(resourceID, errMsg string, now time.Time) error {
	cur := m.rows[resourceID]
	if RunningFresh(cur, now) {
		return nil
	}
	cur.ResourceID = resourceID
	cur.AttemptState = "failed"
	cur.AttemptFinishedAt = now
	cur.AttemptError = errMsg
	cur.AttemptTimestamp = ""
	cur.HeartbeatAt = time.Time{}
	m.rows[resourceID] = cur
	return nil
}

func (m *memStatus) Get(resourceID string) (BackupStatus, error) {
	cur, ok := m.rows[resourceID]
	if !ok {
		return BackupStatus{}, errors.New("not found")
	}
	return cur, nil
}

func (m *memStatus) ListDumps(resourceID string) ([]BackupDump, error) {
	return append([]BackupDump(nil), m.dumps[resourceID]...), nil
}

func (m *memStatus) DeleteDump(resourceID, timestamp string) error {
	var left []BackupDump
	for _, d := range m.dumps[resourceID] {
		if d.Timestamp != timestamp {
			left = append(left, d)
		}
	}
	m.dumps[resourceID] = left
	return nil
}

type fakeDump struct {
	databases []string
	failOn    string
	restores  []string
}

func (f *fakeDump) ListDatabases(context.Context, string) ([]string, error) {
	return append([]string(nil), f.databases...), nil
}

func (f *fakeDump) DumpDatabase(_ context.Context, connURL, database string, w io.Writer) error {
	if strings.Contains(connURL, "s3cret-pass") && strings.Contains(connURL, "s3cret-pass@") {
		// The URL still has the password for the child environment builder.
		// The bytes written must not echo it.
	}
	if database == f.failOn {
		return errors.New("pg_dump failed")
	}
	_, err := io.WriteString(w, "custom-format "+database)
	return err
}

func (f *fakeDump) RestoreDatabase(_ context.Context, _, database string, r io.Reader) error {
	_, _ = io.ReadAll(r)
	f.restores = append(f.restores, database)
	return nil
}
