package postgres

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type putFailBlobs struct{ err error }

// Put fails without reading the body.
func (b putFailBlobs) Put(context.Context, string, io.Reader) error { return b.err }
func (putFailBlobs) Head(context.Context, string) (int64, error)    { return 1, nil }
func (putFailBlobs) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (putFailBlobs) Delete(context.Context, string) error { return nil }

type putFailStatus struct{ finished chan string }

func (putFailStatus) Begin(string, string, time.Time) (BackupStatus, bool, error) {
	return BackupStatus{}, true, nil
}
func (putFailStatus) Heartbeat(string, string, time.Time) error { return nil }
func (s putFailStatus) Finish(_, _, state, _ string, _ []string, _ time.Time) error {
	s.finished <- state
	return nil
}
func (putFailStatus) Fail(string, string, time.Time) error   { return nil }
func (putFailStatus) Get(string) (BackupStatus, error)       { return BackupStatus{}, nil }
func (putFailStatus) ListDumps(string) ([]BackupDump, error) { return nil, nil }
func (putFailStatus) DeleteDump(string, string) error        { return nil }

// A dump that ignores its context and keeps writing, like a child process
// stuck on a full pipe.
type stubbornDump struct{}

func (stubbornDump) ListDatabases(context.Context, string) ([]string, error) {
	return []string{"appdb"}, nil
}
func (stubbornDump) DumpDatabase(_ context.Context, _, _ string, w io.Writer) error {
	buf := make([]byte, 1<<20)
	for {
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
}
func (stubbornDump) RestoreDatabase(context.Context, string, string, io.Reader) error { return nil }

// When Put fails without draining the pipe, RunBackup must return the Put
// error and release the backup lock instead of waiting on the dump forever.
func TestRunBackupReturnsWhenPutFailsEarly(t *testing.T) {
	putErr := errors.New("blobstore refused the object")
	status := putFailStatus{finished: make(chan string, 4)}
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := RunBackup(context.Background(), BackupInput{
			Target: BackupTarget{
				ResourceID:   "11111111-1111-1111-1111-111111111111",
				OwnerAccount: "user:ada",
				Role:         "primary",
				ConnURL:      "postgres://u:p@h:1/appdb",
			},
			Now:         time.Date(2026, 11, 11, 16, 4, 5, 0, time.UTC),
			Destination: BackupDestination{Backend: "s3", Bucket: "b", Allowed: true},
			Blobs:       putFailBlobs{err: putErr},
			Status:      status,
			Dump:        stubbornDump{},
		})
		done <- result{err}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, putErr) {
			t.Fatalf("err = %v, want the Put error", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunBackup still blocked 2s after Put failed; the lock is held forever")
	}
	select {
	case state := <-status.finished:
		if state != "failed" {
			t.Fatalf("state = %s, want failed", state)
		}
	default:
		t.Fatal("backup was not marked finished, so the lock stays held")
	}
}
