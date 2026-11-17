package upgradegraph

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const graphJSON = `{"schema": 1, "releases": [
  {"version": "v2.5.0", "kind": "minor", "minFrom": "v2.4.0", "kubernetes": "1.35.10", "crio": "1.35", "fedora": 45}]}`

// graphServer serves graphJSON until failing is set, counting the requests.
func graphServer(t *testing.T, failing *atomic.Bool, hits *atomic.Int32) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if failing.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(graphJSON))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/upgrade-graph.json"
}

func TestFetcher_ReadsTheGraphOncePerRefresh(t *testing.T) {
	var failing atomic.Bool
	var hits atomic.Int32
	url := graphServer(t, &failing, &hits)
	clock := time.Date(2026, 11, 21, 2, 30, 0, 0, time.UTC)
	f := &Fetcher{Refresh: 10 * time.Minute, Now: func() time.Time { return clock }}

	for range 2 {
		g, err := f.Get(context.Background(), url)
		if err != nil || len(g.Releases) != 1 {
			t.Fatalf("Get = %v, %v; want the one-release graph", g, err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("%d requests within the refresh interval, want 1", hits.Load())
	}
	clock = clock.Add(11 * time.Minute)
	if _, err := f.Get(context.Background(), url); err != nil || hits.Load() != 2 {
		t.Errorf("after the refresh interval: err %v, %d requests; want a second request", err, hits.Load())
	}
}

// A GitHub outage keeps the last graph read: the upgrade goes on.
func TestFetcher_KeepsTheLastGraphWhenTheServerFails(t *testing.T) {
	var failing atomic.Bool
	var hits atomic.Int32
	url := graphServer(t, &failing, &hits)
	clock := time.Date(2026, 11, 21, 2, 30, 0, 0, time.UTC)
	f := &Fetcher{Refresh: time.Minute, Now: func() time.Time { return clock }}
	if _, err := f.Get(context.Background(), url); err != nil {
		t.Fatal(err)
	}

	failing.Store(true)
	clock = clock.Add(time.Hour)
	if g, err := f.Get(context.Background(), url); err != nil || g == nil {
		t.Errorf("Get during the outage = %v, %v; want the last graph", g, err)
	}
}

func TestFetcher_FailsWithoutAGraphToFallBackOn(t *testing.T) {
	var failing atomic.Bool
	var hits atomic.Int32
	failing.Store(true)
	url := graphServer(t, &failing, &hits)

	if g, err := (&Fetcher{}).Get(context.Background(), url); err == nil {
		t.Errorf("Get = %v, want an error", g)
	}
}
