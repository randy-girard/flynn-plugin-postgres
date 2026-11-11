package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn/pkg/dialer"
)

const backupTransferTimeout = 6 * time.Hour

var errBackupStatusMissing = errors.New("tenant backup status not found")

type backupStatusJSON struct {
	ResourceID           string     `json:"resource_id"`
	AttemptState         string     `json:"attempt_state"`
	AttemptStartedAt     *time.Time `json:"attempt_started_at"`
	AttemptFinishedAt    *time.Time `json:"attempt_finished_at"`
	AttemptError         string     `json:"attempt_error"`
	AttemptTimestamp     string     `json:"attempt_timestamp"`
	HeartbeatAt          *time.Time `json:"heartbeat_at"`
	LastSuccessAt        *time.Time `json:"last_success_at"`
	LastSuccessTimestamp string     `json:"last_success_timestamp"`
}

func (s backupStatusJSON) status() postgres.BackupStatus {
	out := postgres.BackupStatus{
		ResourceID:           s.ResourceID,
		AttemptState:         s.AttemptState,
		AttemptError:         s.AttemptError,
		AttemptTimestamp:     s.AttemptTimestamp,
		LastSuccessTimestamp: s.LastSuccessTimestamp,
	}
	if s.AttemptStartedAt != nil {
		out.AttemptStartedAt = *s.AttemptStartedAt
	}
	if s.AttemptFinishedAt != nil {
		out.AttemptFinishedAt = *s.AttemptFinishedAt
	}
	if s.HeartbeatAt != nil {
		out.HeartbeatAt = *s.HeartbeatAt
	}
	if s.LastSuccessAt != nil {
		out.LastSuccessAt = *s.LastSuccessAt
	}
	return out
}

type backupDumpJSON struct {
	Timestamp string   `json:"timestamp"`
	Databases []string `json:"databases"`
}

type httpStatus struct {
	base string
	key  string
	http *http.Client
}

func newHTTPStatus() *httpStatus {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("CONTROLLER_URL")), "/")
	if base == "" {
		base = "http://controller.discoverd"
	}
	return &httpStatus{base: base, key: strings.TrimSpace(os.Getenv("CONTROLLER_KEY")), http: backupHTTPClient(base, 30*time.Second)}
}

type httpBlobs struct {
	base string
	key  string
	http *http.Client
}

func newHTTPBlobs() *httpBlobs {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("BLOBSTORE_URL")), "/")
	if base == "" {
		base = "http://blobstore.discoverd"
	}
	return &httpBlobs{base: base, key: strings.TrimSpace(os.Getenv("CONTROLLER_KEY")), http: backupHTTPClient(base, backupTransferTimeout)}
}

func backupHTTPClient(rawURL string, headerTimeout time.Duration) *http.Client {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || !strings.HasSuffix(u.Hostname(), ".discoverd") {
		return http.DefaultClient
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			return dialer.Retry.Dial(network, addr)
		},
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       90 * time.Second,
	}}
}

func (s *httpStatus) Begin(resourceID, timestamp string, _ time.Time) (postgres.BackupStatus, bool, error) {
	var out struct {
		Started bool             `json:"started"`
		Status  backupStatusJSON `json:"status"`
	}
	code, err := s.post("/tenant-backups/"+url.PathEscape(resourceID)+"/begin", map[string]string{"timestamp": timestamp}, &out)
	if err != nil {
		return postgres.BackupStatus{}, false, err
	}
	if code == http.StatusConflict {
		return out.Status.status(), false, nil
	}
	if code != http.StatusOK {
		return postgres.BackupStatus{}, false, fmt.Errorf("begin backup: status %d", code)
	}
	return out.Status.status(), out.Started, nil
}

func (s *httpStatus) Heartbeat(resourceID, timestamp string, _ time.Time) error {
	code, err := s.post("/tenant-backups/"+url.PathEscape(resourceID)+"/heartbeat", map[string]string{"timestamp": timestamp}, nil)
	if err != nil {
		return err
	}
	if code == http.StatusConflict {
		return postgres.ErrBackupLock
	}
	if code != http.StatusOK {
		return fmt.Errorf("backup heartbeat: status %d", code)
	}
	return nil
}

func (s *httpStatus) Finish(resourceID, timestamp, state, errMsg string, databases []string, _ time.Time) error {
	code, err := s.post("/tenant-backups/"+url.PathEscape(resourceID)+"/finish", map[string]any{
		"state": state, "error": errMsg, "timestamp": timestamp, "databases": databases,
	}, nil)
	if err != nil {
		return err
	}
	if code == http.StatusConflict {
		return postgres.ErrBackupLock
	}
	if code != http.StatusOK {
		return fmt.Errorf("finish backup: status %d", code)
	}
	return nil
}

func (s *httpStatus) Fail(resourceID, errMsg string, _ time.Time) error {
	code, err := s.post("/tenant-backups/"+url.PathEscape(resourceID)+"/fail", map[string]string{"error": errMsg}, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("record backup failure: status %d", code)
	}
	return nil
}

func (s *httpStatus) Get(resourceID string) (postgres.BackupStatus, error) {
	var out backupStatusJSON
	code, err := s.do(http.MethodGet, "/tenant-backups/"+url.PathEscape(resourceID), nil, &out)
	if err != nil {
		return postgres.BackupStatus{}, err
	}
	if code == http.StatusNotFound {
		return postgres.BackupStatus{}, errBackupStatusMissing
	}
	if code != http.StatusOK {
		return postgres.BackupStatus{}, fmt.Errorf("get backup: status %d", code)
	}
	return out.status(), nil
}

func (s *httpStatus) List() ([]postgres.BackupStatus, error) {
	var raw []backupStatusJSON
	code, err := s.do(http.MethodGet, "/tenant-backups", nil, &raw)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("list backups: status %d", code)
	}
	out := make([]postgres.BackupStatus, 0, len(raw))
	for _, row := range raw {
		out = append(out, row.status())
	}
	return out, nil
}

func (s *httpStatus) ListDumps(resourceID string) ([]postgres.BackupDump, error) {
	var raw []backupDumpJSON
	code, err := s.do(http.MethodGet, "/tenant-backups/"+url.PathEscape(resourceID)+"/dumps", nil, &raw)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, nil
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("list dumps: status %d", code)
	}
	out := make([]postgres.BackupDump, 0, len(raw))
	for _, row := range raw {
		out = append(out, postgres.BackupDump{Timestamp: row.Timestamp, Databases: append([]string(nil), row.Databases...)})
	}
	return out, nil
}

func (s *httpStatus) DeleteDump(resourceID, timestamp string) error {
	code, err := s.do(http.MethodDelete, "/tenant-backups/"+url.PathEscape(resourceID)+"/dumps/"+url.PathEscape(timestamp), nil, nil)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound || code == http.StatusOK {
		return nil
	}
	return fmt.Errorf("delete dump: status %d", code)
}

func (s *httpStatus) post(path string, in, out any) (int, error) {
	return s.do(http.MethodPost, path, in, out)
}

func (s *httpStatus) do(method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth("", s.key)
	res, err := s.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return res.StatusCode, err
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 && (res.StatusCode < 300 || res.StatusCode == http.StatusConflict) {
		if err := json.Unmarshal(raw, out); err != nil {
			return res.StatusCode, err
		}
	}
	if res.StatusCode >= 300 && res.StatusCode != http.StatusConflict && res.StatusCode != http.StatusNotFound {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = res.Status
		}
		return res.StatusCode, fmt.Errorf("controller %s: %s", res.Status, msg)
	}
	return res.StatusCode, nil
}

func blobKeyPath(key string) string {
	parts := strings.Split(key, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/" + strings.Join(parts, "/")
}

func (b *httpBlobs) Put(ctx context.Context, key string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.base+blobKeyPath(key), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.SetBasicAuth("", b.key)
	res, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("blobstore put: %s %s", res.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (b *httpBlobs) Head(ctx context.Context, key string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, b.base+blobKeyPath(key), nil)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth("", b.key)
	res, err := b.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("blobstore head: %s", res.Status)
	}
	return res.ContentLength, nil
}

func (b *httpBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+blobKeyPath(key), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("", b.key)
	res, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		return nil, fmt.Errorf("blobstore get: %s", res.Status)
	}
	return res.Body, nil
}

func (b *httpBlobs) Delete(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.base+blobKeyPath(key), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("", b.key)
	res, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusNoContent {
		return nil
	}
	return fmt.Errorf("blobstore delete: %s", res.Status)
}
