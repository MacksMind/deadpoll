package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// collectResults runs the crawler and returns all JSONL results.
func collectResults(t *testing.T, handler http.Handler, cfg *Config) []Result {
	t.Helper()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	if cfg == nil {
		cfg = &Config{
			Checking: CheckingConfig{
				SSLVerify: true,
				Threads:   2,
				MaxDepth:  0,
				Timeout:   "5s",
			},
		}
	}

	var buf bytes.Buffer
	cr := NewCrawler(cfg, &buf)
	cr.Run(ts.URL)

	var results []Result
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad JSONL line: %v\n%s", err, line)
		}
		results = append(results, r)
	}
	return results
}

func filterByType(results []Result, typ string) []Result {
	var filtered []Result
	for _, r := range results {
		if r.Type == typ {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

func TestRedirectIsLogged(t *testing.T) {
	mux := http.NewServeMux()

	// Page with a link to /old which redirects to /new.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>
			<a href="/old">old link</a>
			<a href="/new">new link</a>
		</body></html>`))
	})
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><p>new page</p></body></html>`))
	})

	results := collectResults(t, mux, nil)
	redirects := filterByType(results, "redirect")

	if len(redirects) == 0 {
		t.Fatalf("expected at least one redirect, got none.\nAll results: %+v", results)
	}

	var found *Result
	for _, r := range redirects {
		if strings.HasSuffix(r.URL, "/old") && strings.HasSuffix(r.RedirectURL, "/new") {
			found = &r
			break
		}
	}
	if found == nil {
		t.Fatalf("expected redirect from /old to /new, got redirects: %+v\nall: %+v", redirects, results)
	}
	if found.Status != http.StatusMovedPermanently {
		t.Errorf("expected status %d (301), got %d", http.StatusMovedPermanently, found.Status)
	}
}

func TestRedirectToAlreadyVisitedIsLogged(t *testing.T) {
	mux := http.NewServeMux()

	// /new is linked directly and also via /old (which redirects to /new).
	// Since /new will be visited first (or at least also directly), the
	// redirect from /old should still be logged even though /new is
	// "already visited".
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>
			<a href="/new">direct link</a>
			<a href="/old">old link</a>
		</body></html>`))
	})
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusFound)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><p>new page</p></body></html>`))
	})

	results := collectResults(t, mux, nil)
	redirects := filterByType(results, "redirect")

	var found *Result
	for _, r := range redirects {
		if strings.HasSuffix(r.URL, "/old") {
			found = &r
			break
		}
	}
	if found == nil {
		t.Fatalf("expected redirect logged for /old -> /new (already visited), got: %+v", results)
	}
	if found.Status != http.StatusFound {
		t.Errorf("expected status %d (302), got %d", http.StatusFound, found.Status)
	}
}

func TestBrokenLinkIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><a href="/missing">broken</a></body></html>`))
	})

	results := collectResults(t, mux, nil)
	errors := filterByType(results, "error")

	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d: %+v", len(errors), results)
	}
	if !strings.HasSuffix(errors[0].URL, "/missing") {
		t.Errorf("expected error for /missing, got: %s", errors[0].URL)
	}
	if errors[0].Status != 404 {
		t.Errorf("expected status 404, got %d", errors[0].Status)
	}
}

func TestIgnorePattern(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>
			<a href="/admin/delete/1">delete</a>
			<a href="/about">about</a>
		</body></html>`))
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><p>about</p></body></html>`))
	})

	cfg := &Config{
		Checking: CheckingConfig{
			SSLVerify: true,
			Threads:   2,
			Timeout:   "5s",
		},
		Filtering: FilteringConfig{
			Ignore: []string{"/admin/delete/"},
		},
	}

	results := collectResults(t, mux, cfg)
	errors := filterByType(results, "error")

	// /admin/delete/1 should be ignored, not reported as 404.
	for _, e := range errors {
		if strings.Contains(e.URL, "delete") {
			t.Errorf("ignored URL should not appear in errors: %s", e.URL)
		}
	}
}

func TestImageLinkChecked(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><img src="/missing.jpg"></body></html>`))
	})

	results := collectResults(t, mux, nil)
	errors := filterByType(results, "error")

	if len(errors) != 1 {
		t.Fatalf("expected 1 error for missing image, got %d: %+v", len(errors), results)
	}
	if !strings.HasSuffix(errors[0].URL, "/missing.jpg") {
		t.Errorf("expected error for /missing.jpg, got: %s", errors[0].URL)
	}
}
