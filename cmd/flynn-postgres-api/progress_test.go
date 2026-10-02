package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestParseBasebackupRow(t *testing.T) {
	copied, total, phase, ok := parseBasebackupRow("12345|67890|streaming database files\n")
	if !ok || copied != 12345 || total != 67890 || phase != "streaming database files" {
		t.Fatalf("%d %d %q %v", copied, total, phase, ok)
	}
	if _, _, _, ok := parseBasebackupRow(""); ok {
		t.Fatal("empty")
	}
}

func TestHTTPProgressJSON(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID  string            `json:"id"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	folReq := httptest.NewRequest(http.MethodPost, "/databases/"+created.ID+"/follow", strings.NewReader(`{"app":"shop"}`))
	folRec := httptest.NewRecorder()
	h.ServeHTTP(folRec, folReq)
	if folRec.Code != 200 {
		t.Fatalf("follow %d %s", folRec.Code, folRec.Body.String())
	}
	var fol struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(folRec.Body.Bytes(), &fol); err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSpace(fol.Env["FLYNN_POSTGRES"])
	if name == "" {
		t.Fatalf("follower env %#v", fol.Env)
	}
	if err := h.store.SetBackupProgress(name, 25, 100); err != nil {
		t.Fatal(err)
	}
	progReq := httptest.NewRequest(http.MethodGet, "/databases/"+name+"/progress", nil)
	progRec := httptest.NewRecorder()
	h.ServeHTTP(progRec, progReq)
	if progRec.Code != 200 {
		t.Fatalf("progress %d %s", progRec.Code, progRec.Body.String())
	}
	var p postgres.ReplicaProgress
	if err := json.Unmarshal(progRec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Phase != postgres.PhaseBasebackup || p.Percent == 0 || p.Ready {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(p.Message, "%") {
		t.Fatalf("message %q", p.Message)
	}
}
