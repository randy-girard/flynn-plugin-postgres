package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestHTTPBackupClientsKeepThePasswordOffTheWire(t *testing.T) {
	const password = "s3cret-pass"
	var mu sync.Mutex
	objects := map[string]string{}
	dumps := map[string][]string{}
	var sawPassword bool
	note := func(r *http.Request, body string) {
		if strings.Contains(r.URL.String(), password) || strings.Contains(body, password) || strings.Contains(r.Header.Get("Authorization"), password) {
			sawPassword = true
		}
	}
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		body, _ := io.ReadAll(r.Body)
		note(r, string(body))
		if user != "" || pass != "cluster-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			objects[key] = string(body)
			w.WriteHeader(200)
		case http.MethodHead:
			got, ok := objects[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", itoa(len(got)))
			w.WriteHeader(200)
		case http.MethodGet:
			got, ok := objects[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(got))
		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(200)
		default:
			http.Error(w, "method", 405)
		}
	}))
	defer blob.Close()
	var running string
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		raw, _ := io.ReadAll(r.Body)
		note(r, string(raw))
		if user != "" || pass != "cluster-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		path := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(path, "/")
		if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "begin" {
			var body struct {
				Timestamp string `json:"timestamp"`
			}
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			if running != "" {
				mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"started":false}`))
				return
			}
			running = body.Timestamp
			mu.Unlock()
			_, _ = w.Write([]byte(`{"started":true,"status":{"resource_id":"11111111-1111-1111-1111-111111111111","attempt_state":"running","attempt_timestamp":"` + body.Timestamp + `"}}`))
			return
		}
		if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "heartbeat" {
			w.WriteHeader(200)
			return
		}
		if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "finish" {
			var body struct {
				State     string   `json:"state"`
				Timestamp string   `json:"timestamp"`
				Databases []string `json:"databases"`
			}
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			running = ""
			if body.State == "succeeded" {
				dumps[body.Timestamp] = append([]string(nil), body.Databases...)
			}
			mu.Unlock()
			w.WriteHeader(200)
			return
		}
		if r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "dumps" {
			mu.Lock()
			defer mu.Unlock()
			var out []backupDumpJSON
			for ts, dbs := range dumps {
				out = append(out, backupDumpJSON{Timestamp: ts, Databases: dbs})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		if r.Method == http.MethodDelete && len(parts) == 4 && parts[2] == "dumps" {
			mu.Lock()
			delete(dumps, parts[3])
			mu.Unlock()
			w.WriteHeader(200)
			return
		}
		http.NotFound(w, r)
	}))
	defer ctrl.Close()

	t.Setenv("CONTROLLER_URL", ctrl.URL)
	t.Setenv("CONTROLLER_KEY", "cluster-key")
	t.Setenv("BLOBSTORE_URL", blob.URL)
	target := postgres.BackupTarget{
		ResourceID:   "11111111-1111-1111-1111-111111111111",
		OwnerAccount: "user:ada",
		Role:         "primary",
		ConnURL:      "postgres://appuser:" + password + "@leader.postgresql-oak-10000.discoverd:5432/appdb?sslmode=require",
	}
	got, err := postgres.RunBackup(t.Context(), postgres.BackupInput{
		Target:      target,
		Now:         time.Date(2026, 11, 11, 16, 4, 5, 0, time.UTC),
		Destination: postgres.BackupDestination{Backend: "s3", Bucket: "flynn-blobs", Allowed: true},
		Blobs:       newHTTPBlobs(),
		Status:      newHTTPStatus(),
		Dump:        &fakeWireDump{databases: []string{"appdb"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Timestamp != "20261111T160405Z" {
		t.Fatalf("timestamp %s", got.Timestamp)
	}
	if sawPassword {
		t.Fatal("password was sent to the controller or blobstore")
	}
	if len(objects) != 1 {
		t.Fatalf("objects %#v", objects)
	}
	for key := range objects {
		if !strings.Contains(key, "backups/user:ada/11111111-1111-1111-1111-111111111111/20261111T160405Z/appdb") {
			t.Fatalf("key %s", key)
		}
	}
}

type fakeWireDump struct {
	databases []string
}

func (f *fakeWireDump) ListDatabases(context.Context, string) ([]string, error) {
	return append([]string(nil), f.databases...), nil
}

func (f *fakeWireDump) DumpDatabase(_ context.Context, _, database string, w io.Writer) error {
	_, err := io.WriteString(w, "custom-format "+database)
	return err
}

func (f *fakeWireDump) RestoreDatabase(context.Context, string, string, io.Reader) error {
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
