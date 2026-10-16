package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func stubInstancePsql(t *testing.T, fn func(context.Context, string) (string, error)) {
	t.Helper()
	orig := instancePsql
	t.Cleanup(func() { instancePsql = orig })
	instancePsql = fn
}

func TestMetricsLogEndpointServesResourceSample(t *testing.T) {
	t.Setenv("FLYNN_POSTGRES", "postgresql-basin-73690")
	t.Setenv(postgres.ResourceIDEnv, "39cd10de1737e3e7")
	stubInstancePsql(t, func(_ context.Context, query string) (string, error) {
		if strings.Contains(query, "concat_ws") {
			return "24114245|11|6|0|100|0.93548|1|70583|26097078|18898|0|0", nil
		}
		if strings.Contains(query, "pg_extension") {
			return "t", nil
		}
		return `[{"query":"SELECT 1"}]`, nil
	})
	origLog := instanceMetricsLog
	t.Cleanup(func() { instanceMetricsLog = origLog })
	var logged []string
	instanceMetricsLog = func(line string) { logged = append(logged, line) }

	ts := httptest.NewServer(instanceMetricsHandler())
	t.Cleanup(ts.Close)
	res, err := http.Get(ts.URL + postgres.MetricsLogPath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	got := string(body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type %q", ct)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("must be one line with no trailing junk: %q", got)
	}
	if len(logged) != 0 {
		t.Fatalf("scrape must not write sample lines to stdout: %v", logged)
	}
	if !strings.HasPrefix(got, "flynn-postgres ") {
		t.Fatalf("line=%q", got)
	}
	if !strings.Contains(got, "source=postgresql-basin-73690") {
		t.Fatalf("line=%q", got)
	}
	if !strings.Contains(got, "addon=39cd10de1737e3e7") {
		t.Fatalf("line=%q", got)
	}
	if !strings.Contains(got, "sample#service-available=1") || !strings.Contains(got, "sample#tables=11") {
		t.Fatalf("line=%q", got)
	}
	if !strings.Contains(got, "sample#slow-queries=1") {
		t.Fatalf("line=%q", got)
	}
	if strings.Contains(got, "CONTEXT") || strings.Contains(got, "RAISE") {
		t.Fatalf("must print a clean sample line, not plpgsql noise: %q", got)
	}
}

func TestMetricsLogEndpointOnlyServesGetMetricsLog(t *testing.T) {
	stubInstancePsql(t, func(context.Context, string) (string, error) { return "", errors.New("down") })
	ts := httptest.NewServer(instanceMetricsHandler())
	t.Cleanup(ts.Close)
	res, err := http.Post(ts.URL+postgres.MetricsLogPath, "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", res.StatusCode)
	}
	res, err = http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("other path status %d", res.StatusCode)
	}
}

func TestInstanceMetricLineFailedScrapeOmitsKeys(t *testing.T) {
	t.Setenv("FLYNN_POSTGRES", "postgresql-basin-73690")
	t.Setenv(postgres.ResourceIDEnv, "39cd10de1737e3e7")
	stubInstancePsql(t, func(context.Context, string) (string, error) { return "", errors.New("down") })
	got := instanceMetricLine(context.Background())
	if got != "flynn-postgres source=postgresql-basin-73690 addon=39cd10de1737e3e7 sample#service-available=0" {
		t.Fatalf("line=%q", got)
	}
}

func TestInstanceMetricLineOmitsSlowQueriesOnTimeout(t *testing.T) {
	stubInstancePsql(t, func(ctx context.Context, query string) (string, error) {
		if strings.Contains(query, "concat_ws") {
			return "1|1|1|0|100|1|1|1|1|1|0|0", nil
		}
		<-ctx.Done()
		return "", ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	got := instanceMetricLine(ctx)
	if !strings.Contains(got, "sample#service-available=1") {
		t.Fatalf("line=%q", got)
	}
	if strings.Contains(got, "slow-queries") {
		t.Fatalf("timed-out slow query probe must be omitted, not zero: %q", got)
	}
}

func TestInstanceMetricsPortDefault(t *testing.T) {
	t.Setenv(postgres.MetricsPortEnv, "")
	if got := postgres.InstanceMetricsPort(); got != "9180" {
		t.Fatalf("default port %q", got)
	}
	t.Setenv(postgres.MetricsPortEnv, "9999")
	if got := postgres.InstanceMetricsPort(); got != "9999" {
		t.Fatalf("env port %q", got)
	}
}

func TestLogInstanceTopologyWritesFlynnLine(t *testing.T) {
	t.Setenv("FLYNN_POSTGRES", "postgresql-upland-22935")
	t.Setenv("FLYNN_POSTGRES_ROLE", "follower")
	t.Setenv("FLYNN_POSTGRES_LEADER", "postgresql-basin-73690")
	origLog := instanceMetricsLog
	t.Cleanup(func() { instanceMetricsLog = origLog })
	var got string
	instanceMetricsLog = func(line string) { got = line }
	logInstanceTopology()
	if got != "flynn-postgres source=postgresql-upland-22935 event#follower role=follower leader=postgresql-basin-73690" {
		t.Fatalf("got %q", got)
	}
}
