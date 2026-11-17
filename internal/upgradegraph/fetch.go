package upgradegraph

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// maxGraphBytes bounds what is read from the graph URL.
const maxGraphBytes = 4 << 20

// Fetcher reads upgrade graphs over HTTP, at most once per Refresh for each
// URL. When a read fails it returns the last graph it read from that URL.
type Fetcher struct {
	Client  *http.Client     // http.Client with a 30 s timeout when nil
	Refresh time.Duration    // 10 minutes when zero
	Now     func() time.Time // time.Now when nil

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	graph *Graph
	read  time.Time
}

// Get returns the graph at url.
func (f *Fetcher) Get(ctx context.Context, url string) (*Graph, error) {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	refresh := f.Refresh
	if refresh == 0 {
		refresh = 10 * time.Minute
	}

	f.mu.Lock()
	last, ok := f.cache[url]
	f.mu.Unlock()
	if ok && now().Sub(last.read) < refresh {
		return last.graph, nil
	}

	g, err := f.read(ctx, url)
	if err != nil {
		if ok {
			return last.graph, nil
		}
		return nil, err
	}
	f.mu.Lock()
	if f.cache == nil {
		f.cache = map[string]cached{}
	}
	f.cache[url] = cached{graph: g, read: now()}
	f.mu.Unlock()
	return g, nil
}

func (f *Fetcher) read(ctx context.Context, url string) (*Graph, error) {
	c := f.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGraphBytes))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	return Parse(data)
}
