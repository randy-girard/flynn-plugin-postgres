package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/julienschmidt/httprouter"
	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	ct "github.com/randy-girard/flynn/controller/types"
)

type backupView struct {
	Destination postgres.BackupDestination `json:"destination"`
	Miss        bool                       `json:"miss"`
	Status      postgres.BackupStatus      `json:"status"`
	Backups     []postgres.BackupDump      `json:"backups"`
}

type backupAPI interface {
	View(ref string) (backupView, error)
	Create(ref string) (postgres.BackupResult, error)
	Restore(ref, timestamp string) error
}

func (h *handler) currentBackups() backupAPI {
	if h == nil {
		return nil
	}
	if h.backupAPI != nil {
		return h.backupAPI
	}
	if h.client == nil {
		return nil
	}
	return liveBackups{h}
}

func (h *handler) listBackups(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	h.writeBackupView(w, p.ByName("id"))
}

func (h *handler) createBackup(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	h.writeBackupCreate(w, p.ByName("id"))
}

func (h *handler) restoreBackup(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		Timestamp string `json:"timestamp"`
	}
	_ = decode(r, &body)
	h.writeBackupRestore(w, p.ByName("id"), body.Timestamp)
}

func (h *handler) dashBackupList(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	ref, err := h.backupRef(sess)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	h.writeBackupView(w, ref)
}

func (h *handler) dashBackupCreate(w http.ResponseWriter, _ *http.Request, sess *dashui.Session) {
	ref, err := h.backupRef(sess)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	h.writeBackupCreate(w, ref)
}

func (h *handler) dashBackupRestore(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	ref, err := h.backupRef(sess)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	var body struct {
		Timestamp string `json:"timestamp"`
	}
	_ = decode(r, &body)
	h.writeBackupRestore(w, ref, body.Timestamp)
}

func (h *handler) backupRef(sess *dashui.Session) (string, error) {
	inst, err := h.dumpTarget(sess)
	if err != nil {
		return "", err
	}
	if inst.App != "" {
		return inst.App, nil
	}
	return inst.ID, nil
}

func (h *handler) writeBackupView(w http.ResponseWriter, ref string) {
	api := h.currentBackups()
	if api == nil {
		dashui.WriteJSON(w, 200, backupView{
			Destination: postgres.BackupDestination{Backend: "postgres", Allowed: false},
			Miss:        true,
		})
		return
	}
	view, err := api.View(ref)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	if view.Backups == nil {
		view.Backups = []postgres.BackupDump{}
	}
	dashui.WriteJSON(w, 200, view)
}

func (h *handler) writeBackupCreate(w http.ResponseWriter, ref string) {
	api := h.currentBackups()
	if api == nil {
		writeBackupError(w, fmt.Errorf("backup is not configured"))
		return
	}
	got, err := api.Create(ref)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	dashui.WriteJSON(w, 200, map[string]any{"timestamp": got.Timestamp, "databases": got.Databases})
}

func (h *handler) writeBackupRestore(w http.ResponseWriter, ref, timestamp string) {
	api := h.currentBackups()
	if api == nil {
		writeBackupError(w, fmt.Errorf("backup is not configured"))
		return
	}
	if err := api.Restore(ref, strings.TrimSpace(timestamp)); err != nil {
		writeBackupError(w, err)
		return
	}
	dashui.WriteJSON(w, 200, map[string]string{"status": "ok"})
}

func writeBackupError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, postgres.ErrBackupBusy):
		code = http.StatusConflict
	case errors.Is(err, postgres.ErrNotFound), errors.Is(err, postgres.ErrBackupNotListed):
		code = http.StatusNotFound
	}
	dashui.WriteJSON(w, code, map[string]string{"error": err.Error()})
}

func rejectFileBackup(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":"tenant dumps are stored in the blobstore; use the backup list, not a file"}`))
}

type liveBackups struct{ h *handler }

func (l liveBackups) View(ref string) (backupView, error) {
	target, err := l.h.resolveBackupTarget(ref)
	if err != nil {
		return backupView{}, err
	}
	dest, err := l.h.backupDestination()
	if err != nil {
		return backupView{}, err
	}
	status := newHTTPStatus()
	st, err := status.Get(target.ResourceID)
	if errors.Is(err, errBackupStatusMissing) {
		st = postgres.BackupStatus{ResourceID: target.ResourceID}
		err = nil
	}
	if err != nil {
		return backupView{}, err
	}
	dumps, err := status.ListDumps(target.ResourceID)
	if err != nil {
		return backupView{}, err
	}
	return backupView{
		Destination: dest,
		Miss:        postgres.BackupMiss(st.LastSuccessAt, time.Now().UTC()),
		Status:      st,
		Backups:     dumps,
	}, nil
}

func (l liveBackups) Create(ref string) (postgres.BackupResult, error) {
	target, err := l.h.resolveBackupTarget(ref)
	if err != nil {
		return postgres.BackupResult{}, err
	}
	dest, err := l.h.backupDestination()
	if err != nil {
		return postgres.BackupResult{}, err
	}
	return postgres.RunBackup(context.Background(), postgres.BackupInput{
		Target:      target,
		Now:         time.Now().UTC(),
		Destination: dest,
		Blobs:       newHTTPBlobs(),
		Status:      newHTTPStatus(),
		Dump:        execDump{},
	})
}

func (l liveBackups) Restore(ref, timestamp string) error {
	target, err := l.h.resolveBackupTarget(ref)
	if err != nil {
		return err
	}
	status := newHTTPStatus()
	if strings.TrimSpace(timestamp) == "" {
		dumps, err := status.ListDumps(target.ResourceID)
		if err != nil {
			return err
		}
		if len(dumps) == 0 {
			return postgres.ErrBackupNotListed
		}
		timestamp = dumps[0].Timestamp
	}
	return postgres.RunRestore(context.Background(), postgres.BackupInput{
		Target: target,
		Blobs:  newHTTPBlobs(),
		Status: status,
		Dump:   execDump{},
	}, timestamp)
}

func (h *handler) backupDestination() (postgres.BackupDestination, error) {
	if h.appReleaseEnv != nil {
		if env := h.appReleaseEnv("blobstore"); env != nil {
			return postgres.BackupDestinationFromEnv(env), nil
		}
	}
	if h.client == nil {
		return postgres.BackupDestination{}, fmt.Errorf("backup is not configured")
	}
	rel, err := h.client.GetAppRelease("blobstore")
	if err != nil || rel == nil {
		if err == nil {
			err = fmt.Errorf("blobstore release is empty")
		}
		return postgres.BackupDestination{Backend: "postgres", Allowed: false}, err
	}
	return postgres.BackupDestinationFromEnv(rel.Env), nil
}

func (h *handler) resolveBackupTarget(ref string) (postgres.BackupTarget, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return postgres.BackupTarget{}, postgres.ErrNotFound
	}
	list, err := h.postgresResources()
	if err != nil {
		return postgres.BackupTarget{}, err
	}
	res := matchPostgresResource(list, ref)
	if res == nil {
		return postgres.BackupTarget{}, postgres.ErrNotFound
	}
	app := ""
	role := ""
	if res.Env != nil {
		app = strings.TrimSpace(res.Env["FLYNN_POSTGRES"])
		role = strings.TrimSpace(res.Env["FLYNN_POSTGRES_ROLE"])
		if role == "" {
			role = strings.TrimSpace(res.Env["POSTGRES_ROLE"])
		}
	}
	var env map[string]string
	if app != "" {
		env = h.releaseEnv(app)
		if relRole := strings.TrimSpace(env["FLYNN_POSTGRES_ROLE"]); relRole != "" {
			role = relRole
		} else if relRole = strings.TrimSpace(env["POSTGRES_ROLE"]); relRole != "" {
			role = relRole
		}
	}
	return postgres.BackupTarget{
		ResourceID:   res.ID,
		OwnerAccount: res.OwnerAccount,
		App:          app,
		Role:         role,
		HostID:       h.hostFor(res, app),
		ConnURL:      backupConnURL(env),
	}, nil
}

func (h *handler) postgresResources() ([]*ct.Resource, error) {
	if h.backupResources != nil {
		return h.backupResources()
	}
	if h.client == nil {
		return nil, fmt.Errorf("backup is not configured")
	}
	return h.client.ResourceList("postgres")
}

func (h *handler) releaseEnv(app string) map[string]string {
	if h.appReleaseEnv != nil {
		if env := h.appReleaseEnv(app); env != nil {
			return env
		}
	}
	if h.client == nil || app == "" {
		return nil
	}
	rel, err := h.client.GetAppRelease(app)
	if err != nil || rel == nil {
		return nil
	}
	return rel.Env
}

func (h *handler) hostFor(res *ct.Resource, app string) string {
	if h.backupHost != nil {
		return h.backupHost(res)
	}
	if h.client == nil || app == "" {
		return ""
	}
	jobs, err := h.client.JobList(app)
	if err != nil {
		return ""
	}
	return backupHostID(jobs)
}

func matchPostgresResource(list []*ct.Resource, ref string) *ct.Resource {
	var named *ct.Resource
	for _, res := range list {
		if res == nil {
			continue
		}
		if res.ID == ref || res.ExternalID == ref {
			return res
		}
		if res.Env == nil {
			continue
		}
		if res.Env["FLYNN_POSTGRES"] == ref || res.Env[postgres.ResourceIDEnv] == ref {
			named = res
		}
	}
	return named
}

func backupConnURL(env map[string]string) string {
	if env == nil {
		return ""
	}
	user := strings.TrimSpace(env["POSTGRES_USER"])
	db := strings.TrimSpace(env["POSTGRES_DB"])
	app := strings.TrimSpace(env["FLYNN_POSTGRES"])
	if user == "" || db == "" || app == "" {
		return ""
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, env["POSTGRES_PASSWORD"]),
		Host:     "leader." + app + ".discoverd:5432",
		Path:     "/" + db,
		RawQuery: "sslmode=require",
	}
	return u.String()
}

func backupHostID(jobs []*ct.Job) string {
	for _, job := range jobs {
		if job == nil || job.HostID == "" {
			continue
		}
		if job.Type != "" && job.Type != postgres.ProcessName {
			continue
		}
		switch job.State {
		case ct.JobStateUp, ct.JobStateStarting, ct.JobStatePending:
			return job.HostID
		}
	}
	return ""
}

func (h *handler) purgeTenantBackups(inst *postgres.Instance) {
	if h == nil || inst == nil {
		return
	}
	if h.log == nil {
		h.log = log15.New()
	}
	ref := ""
	if inst != nil {
		ref = firstNonEmpty(inst.App, inst.ID)
	}
	if ref == "" || h.client == nil {
		return
	}
	view, err := liveBackups{h}.View(ref)
	if err != nil {
		h.log.Error("list tenant backups before delete", "err", err, "ref", ref)
		return
	}
	target, err := h.resolveBackupTarget(ref)
	if err != nil {
		h.log.Error("resolve tenant backup before delete", "err", err, "ref", ref)
		return
	}
	blobs := newHTTPBlobs()
	status := newHTTPStatus()
	ctx := context.Background()
	for _, dump := range view.Backups {
		for _, db := range dump.Databases {
			key, err := postgres.BackupObjectKey(target.OwnerAccount, target.ResourceID, dump.Timestamp, db)
			if err != nil {
				continue
			}
			if err := blobs.Delete(ctx, key); err != nil {
				h.log.Error("delete tenant backup object", "err", err, "key", key)
			}
		}
		if err := status.DeleteDump(target.ResourceID, dump.Timestamp); err != nil {
			h.log.Error("delete tenant backup catalog", "err", err)
		}
	}
}
