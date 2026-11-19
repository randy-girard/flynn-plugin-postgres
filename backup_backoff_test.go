package postgres

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

const backoffID = "11111111-1111-1111-1111-111111111111"

func failedStatus(last, finished time.Time) BackupStatus {
	return BackupStatus{
		ResourceID:        backoffID,
		AttemptState:      "failed",
		AttemptFinishedAt: finished,
		AttemptError:      "boom",
		LastSuccessAt:     last,
	}
}

// A failed attempt must not be retried on the next one-minute tick.
func TestBackupDueBacksOffAfterFailure(t *testing.T) {
	last := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)

	// Past catch-up, failure 3h after the success window opened: wait 3h.
	fin := last.Add(26 * time.Hour)
	st := failedStatus(last, fin)
	if BackupDueFor(st, fin.Add(time.Minute), backoffID, time.Time{}) {
		t.Fatal("retried one minute after a failure")
	}
	if BackupDueFor(st, fin.Add(3*time.Hour-time.Second), backoffID, time.Time{}) {
		t.Fatal("retried before the doubled backoff elapsed")
	}
	if !BackupDueFor(st, fin.Add(3*time.Hour), backoffID, time.Time{}) {
		t.Fatal("not retried after the backoff elapsed")
	}

	// The wait grows with the time spent failing, and is capped.
	fin = last.Add(100 * time.Hour)
	st = failedStatus(last, fin)
	if BackupDueFor(st, fin.Add(BackupRetryMax-time.Second), backoffID, time.Time{}) {
		t.Fatal("backoff exceeded no cap")
	}
	if !BackupDueFor(st, fin.Add(BackupRetryMax), backoffID, time.Time{}) {
		t.Fatal("backoff is longer than the cap")
	}

	// Never succeeded: grows from the resource creation time, minimum 5m.
	created := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	fin = created.Add(time.Minute)
	st = failedStatus(time.Time{}, fin)
	if BackupDueFor(st, fin.Add(BackupRetryMin-time.Second), backoffID, created) {
		t.Fatal("first retry came before the minimum backoff")
	}
	if !BackupDueFor(st, fin.Add(BackupRetryMin), backoffID, created) {
		t.Fatal("first retry did not come at the minimum backoff")
	}
}

func TestBackupDueIgnoresStaleOrNonFailedAttempts(t *testing.T) {
	last := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	now := last.Add(30 * time.Hour)
	// A failure older than the last success does not delay anything.
	if !BackupDueFor(failedStatus(last, last.Add(-time.Hour)), now, backoffID, time.Time{}) {
		t.Fatal("a failure before the last success delayed the next dump")
	}
	// Not failed: plain schedule.
	st := BackupStatus{ResourceID: backoffID, AttemptState: "succeeded", AttemptFinishedAt: now.Add(-time.Second), LastSuccessAt: last}
	if !BackupDueFor(st, now, backoffID, time.Time{}) {
		t.Fatal("non-failed status delayed a due dump")
	}
	// Not yet due stays not due.
	if BackupDueFor(BackupStatus{LastSuccessAt: last}, last.Add(time.Hour), backoffID, time.Time{}) {
		t.Fatal("backoff made a fresh dump due")
	}
}

func TestFilterBackupDatabasesSkipsUnsupportedName(t *testing.T) {
	kept, skipped := FilterBackupDatabases([]string{"postgres", "template0", "shop", "bad-name", "appdb", "shop", "", "bad-name"})
	if strings.Join(kept, ",") != "appdb,shop" {
		t.Fatalf("kept = %v", kept)
	}
	if strings.Join(skipped, ",") != "bad-name" {
		t.Fatalf("skipped = %v, want [bad-name]", skipped)
	}
}

type okBlobs struct{}

func (okBlobs) Put(_ context.Context, _ string, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}
func (okBlobs) Head(context.Context, string) (int64, error) { return 1, nil }
func (okBlobs) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (okBlobs) Delete(context.Context, string) error { return nil }

type oddNameDump struct{}

func (oddNameDump) ListDatabases(context.Context, string) ([]string, error) {
	return []string{"bad-name", "appdb"}, nil
}
func (oddNameDump) DumpDatabase(_ context.Context, _, _ string, w io.Writer) error {
	_, err := w.Write([]byte("data"))
	return err
}
func (oddNameDump) RestoreDatabase(context.Context, string, string, io.Reader) error { return nil }

// One unsupported database name must not abort the dump of the others.
func TestRunBackupSkipsUnsupportedDatabaseName(t *testing.T) {
	status := putFailStatus{finished: make(chan string, 4)}
	res, err := RunBackup(context.Background(), BackupInput{
		Target:      BackupTarget{ResourceID: backoffID, OwnerAccount: "user:ada", Role: "primary", ConnURL: "postgres://u:p@h:1/appdb"},
		Now:         time.Date(2026, 11, 11, 16, 4, 5, 0, time.UTC),
		Destination: BackupDestination{Backend: "s3", Bucket: "b", Allowed: true},
		Blobs:       okBlobs{},
		Status:      status,
		Dump:        oddNameDump{},
	})
	if err != nil {
		t.Fatalf("backup failed over one odd name: %v", err)
	}
	if strings.Join(res.Databases, ",") != "appdb" {
		t.Fatalf("databases = %v", res.Databases)
	}
	if strings.Join(res.Skipped, ",") != "bad-name" {
		t.Fatalf("skipped = %v", res.Skipped)
	}
	if st := <-status.finished; st != "succeeded" {
		t.Fatalf("state = %s", st)
	}
}

// pg_dump by the instance login must read every tenant database.
func TestAdminPrivilegesGrantReadAllData(t *testing.T) {
	sql, err := AdminPrivilegesSQL("flynn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "GRANT pg_read_all_data TO flynn") {
		t.Fatalf("backup role cannot read other roles' tables: %s", sql)
	}
}
