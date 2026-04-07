package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"
)

// Result is a single finding written as one JSONL line.
type Result struct {
	Type        string `json:"type"`
	URL         string `json:"url"`
	Status      int    `json:"status,omitempty"`
	RedirectURL string `json:"redirect_url,omitempty"`
	Error       string `json:"error,omitempty"`
	Parent      string `json:"parent,omitempty"`
	Timestamp   string `json:"timestamp"`
}

type compiledIgnoreUnless struct {
	pattern *regexp.Regexp
	unless  *regexp.Regexp
}

// Crawler drives the link-checking run.
type Crawler struct {
	cfg          *Config
	output       io.Writer
	ignore       []*regexp.Regexp
	ignoreUnless []compiledIgnoreUnless
	nofollow     []*regexp.Regexp
	host         string

	mu             sync.Mutex
	checked        int64
	errors         int64
	discovered     int64
	seen           sync.Map
	parentMap      sync.Map
	redirectStatus sync.Map // URL -> HTTP status code (3xx)
	start      time.Time
}

// NewCrawler compiles filter patterns and returns a ready-to-run Crawler.
func NewCrawler(cfg *Config, output io.Writer) *Crawler {
	cr := &Crawler{cfg: cfg, output: output}

	for _, p := range cfg.Filtering.Ignore {
		re, err := regexp.Compile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: bad ignore pattern %q: %v\n", p, err)
			continue
		}
		cr.ignore = append(cr.ignore, re)
	}

	for _, rule := range cfg.Filtering.IgnoreUnless {
		pat, err := regexp.Compile(rule.Pattern)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: bad ignore_unless pattern %q: %v\n", rule.Pattern, err)
			continue
		}
		unless, err := regexp.Compile(rule.Unless)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: bad ignore_unless unless %q: %v\n", rule.Unless, err)
			continue
		}
		cr.ignoreUnless = append(cr.ignoreUnless, compiledIgnoreUnless{pat, unless})
	}

	for _, p := range cfg.Filtering.Nofollow {
		re, err := regexp.Compile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: bad nofollow pattern %q: %v\n", p, err)
			continue
		}
		cr.nofollow = append(cr.nofollow, re)
	}

	return cr
}

func (cr *Crawler) isIgnored(u string) bool {
	for _, re := range cr.ignore {
		if re.MatchString(u) {
			return true
		}
	}
	for _, rule := range cr.ignoreUnless {
		if rule.pattern.MatchString(u) && !rule.unless.MatchString(u) {
			return true
		}
	}
	return false
}

func (cr *Crawler) isNofollow(u string) bool {
	for _, re := range cr.nofollow {
		if re.MatchString(u) {
			return true
		}
	}
	return false
}

func (cr *Crawler) isInternal(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return parsed.Hostname() == cr.host
}

// Run starts the crawl and blocks until it finishes.
// Returns 0 on success, 1 if any errors were recorded.
func (cr *Crawler) Run(targetURL string) int {
	cr.start = time.Now()

	parsed, err := url.Parse(targetURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid target URL: %v\n", err)
		return 1
	}
	cr.host = parsed.Hostname()

	opts := []colly.CollectorOption{colly.Async(true)}
	if !cr.cfg.Filtering.CheckExtern {
		opts = append(opts, colly.AllowedDomains(cr.host))
	}
	c := colly.NewCollector(opts...)

	if cr.cfg.Checking.MaxDepth > 0 {
		c.MaxDepth = cr.cfg.Checking.MaxDepth
	}

	// TLS, with a wrapper that records redirect status codes.
	c.WithTransport(&redirectStatusTransport{
		base: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: !cr.cfg.Checking.SSLVerify,
			},
		},
		cr: cr,
	})

	c.SetRequestTimeout(cr.cfg.Checking.TimeoutDuration())

	if err := c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: cr.cfg.Checking.Threads,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: setting limit rule: %v\n", err)
		return 1
	}

	extensions.Referer(c)

	// Track original URLs before redirects overwrite them.
	origURLMap := &sync.Map{}

	c.OnRequest(func(r *colly.Request) {
		origURLMap.Store(r.ID, r.URL.String())
		// Cookies.
		for name, value := range cr.cfg.Cookies {
			r.Headers.Set("Cookie", name+"="+value)
		}
	})

	// nofollow tracker: URLs we'll check but won't extract links from.
	nofollowSet := &sync.Map{}

	// Parent tracker is cr.parentMap (on the struct so the transport can access it).

	// checkLink validates and visits a discovered URL.
	// If followable is true, the target page will be crawled for more links.
	// If false, it is only checked (HEAD-like behavior via nofollow).
	checkLink := func(e *colly.HTMLElement, rawURL string, followable bool) {
		currentURL := e.Request.URL.String()

		link := e.Request.AbsoluteURL(rawURL)
		if link == "" {
			return
		}

		// Strip fragment.
		if u, err := url.Parse(link); err == nil {
			u.Fragment = ""
			link = u.String()
		}

		if cr.isIgnored(link) {
			return
		}

		// External link handling.
		if !cr.isInternal(link) {
			if !cr.cfg.Filtering.CheckExtern {
				return
			}
			nofollowSet.Store(link, true)
		}

		if !followable || cr.isNofollow(link) {
			nofollowSet.Store(link, true)
		}

		// Track unique discovered URLs.
		if _, loaded := cr.seen.LoadOrStore(link, true); !loaded {
			atomic.AddInt64(&cr.discovered, 1)
		}

		// Record the first parent that linked to this URL.
		cr.parentMap.LoadOrStore(link, currentURL)

		e.Request.Visit(link)
	}

	// Crawlable links — follow into the page for more links.
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		// Don't extract links from a nofollow page.
		if _, ok := nofollowSet.Load(e.Request.URL.String()); ok {
			return
		}
		checkLink(e, e.Attr("href"), true)
	})

	// Resource links — check only, don't crawl.
	c.OnHTML("img[src]", func(e *colly.HTMLElement) {
		checkLink(e, e.Attr("src"), false)
	})
	c.OnHTML("script[src]", func(e *colly.HTMLElement) {
		checkLink(e, e.Attr("src"), false)
	})
	c.OnHTML("link[href]", func(e *colly.HTMLElement) {
		checkLink(e, e.Attr("href"), false)
	})
	c.OnHTML("source[src]", func(e *colly.HTMLElement) {
		checkLink(e, e.Attr("src"), false)
	})
	c.OnHTML("source[srcset]", func(e *colly.HTMLElement) {
		for _, src := range parseSrcset(e.Attr("srcset")) {
			checkLink(e, src, false)
		}
	})
	c.OnHTML("img[srcset]", func(e *colly.HTMLElement) {
		for _, src := range parseSrcset(e.Attr("srcset")) {
			checkLink(e, src, false)
		}
	})
	c.OnHTML("video[src]", func(e *colly.HTMLElement) {
		checkLink(e, e.Attr("src"), false)
	})
	c.OnHTML("video[poster]", func(e *colly.HTMLElement) {
		checkLink(e, e.Attr("poster"), false)
	})
	c.OnHTML("audio[src]", func(e *colly.HTMLElement) {
		checkLink(e, e.Attr("src"), false)
	})
	c.OnHTML("object[data]", func(e *colly.HTMLElement) {
		checkLink(e, e.Attr("data"), false)
	})
	c.OnHTML("embed[src]", func(e *colly.HTMLElement) {
		checkLink(e, e.Attr("src"), false)
	})

	c.OnResponse(func(r *colly.Response) {
		atomic.AddInt64(&cr.checked, 1)

		// Detect redirects: if the final URL differs from the original,
		// a redirect was followed.
		if orig, ok := origURLMap.LoadAndDelete(r.Request.ID); ok {
			origURL := orig.(string)
			finalURL := r.Request.URL.String()
			if origURL != finalURL {
				var parent string
				if v, ok := cr.parentMap.Load(origURL); ok {
					parent = v.(string)
				}
				status := 0
				if s, ok := cr.redirectStatus.LoadAndDelete(origURL); ok {
					status = s.(int)
				}
				result := Result{
					Type:        "redirect",
					URL:         origURL,
					Status:      status,
					RedirectURL: finalURL,
					Parent:      parent,
					Timestamp:   time.Now().Format(time.RFC3339),
				}
				cr.mu.Lock()
				json.NewEncoder(cr.output).Encode(result)
				cr.mu.Unlock()
			}
		}
	})

	c.OnError(func(r *colly.Response, err error) {
		errMsg := err.Error()

		// Colly v2.3.0 fires OnError for revisit attempts and
		// redirects to disallowed domains.
		if strings.Contains(errMsg, "already visited") ||
			strings.Contains(errMsg, "Forbidden domain") {
			// These errors occur when a redirect target is already visited
			// or on a forbidden domain. The original URL (before redirect)
			// is still worth logging as a redirect.
			origURL := r.Request.URL.String()
			var parent string
			if v, ok := cr.parentMap.Load(origURL); ok {
				parent = v.(string)
			}
			// Extract redirect target from the error message.
			// Format: Get "/new": "http://host/new" already visited
			// Format: Not following redirect to "http://host/...": Forbidden domain
			if redirectTarget := extractRedirectTarget(errMsg); redirectTarget != "" {
				status := 0
				if s, ok := cr.redirectStatus.LoadAndDelete(origURL); ok {
					status = s.(int)
				}
				result := Result{
					Type:        "redirect",
					URL:         origURL,
					Status:      status,
					RedirectURL: redirectTarget,
					Parent:      parent,
					Timestamp:   time.Now().Format(time.RFC3339),
				}
				cr.mu.Lock()
				json.NewEncoder(cr.output).Encode(result)
				cr.mu.Unlock()
			}
			return
		}

		atomic.AddInt64(&cr.checked, 1)

		atomic.AddInt64(&cr.errors, 1)

		var parent string
		if v, ok := cr.parentMap.Load(r.Request.URL.String()); ok {
			parent = v.(string)
		}

		result := Result{
			Type:      "error",
			URL:       r.Request.URL.String(),
			Status:    r.StatusCode,
			Error:     errMsg,
			Parent:    parent,
			Timestamp: time.Now().Format(time.RFC3339),
		}

		cr.mu.Lock()
		json.NewEncoder(cr.output).Encode(result)
		cr.mu.Unlock()
	})

	// Heartbeat.
	done := make(chan struct{})
	go cr.heartbeat(done)

	c.Visit(targetURL)
	c.Wait()
	close(done)

	elapsed := time.Since(cr.start).Round(time.Second)
	fmt.Fprintf(os.Stderr, "\nDone. %d discovered, %d checked, %d errors in %v\n",
		atomic.LoadInt64(&cr.discovered),
		atomic.LoadInt64(&cr.checked),
		atomic.LoadInt64(&cr.errors),
		elapsed,
	)

	if atomic.LoadInt64(&cr.errors) > 0 {
		return 1
	}
	return 0
}

// redirectStatusTransport wraps an http.RoundTripper to record 3xx status
// codes. It does not handle redirects — Go's http.Client and Colly do that.
// It only stores the status so redirect log entries can include 301 vs 302.
type redirectStatusTransport struct {
	base http.RoundTripper
	cr   *Crawler
}

func (t *redirectStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		t.cr.redirectStatus.Store(req.URL.String(), resp.StatusCode)
	}
	return resp, err
}

// extractRedirectTarget pulls the redirect destination URL from Colly's
// "already visited" or "Forbidden domain" error messages.
// Returns empty string if no URL can be extracted.
func extractRedirectTarget(errMsg string) string {
	// "already visited" format:
	//   Get "/path": "http://host/path" already visited
	// "Forbidden domain" format:
	//   Not following redirect to "http://host/path": Forbidden domain
	//
	// In both cases, the last quoted URL before the error suffix is the target.
	// We find the last "http(s)://..." in quotes.
	idx := strings.LastIndex(errMsg, "\"http")
	if idx == -1 {
		return ""
	}
	rest := errMsg[idx+1:]
	end := strings.Index(rest, "\"")
	if end == -1 {
		return ""
	}
	return rest[:end]
}

// parseSrcset extracts URLs from an HTML srcset attribute.
// Format: "url1 1x, url2 2x" or "url1 100w, url2 200w"
func parseSrcset(srcset string) []string {
	var urls []string
	for _, candidate := range strings.Split(srcset, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		// Each candidate is "url [descriptor]" — we only need the URL.
		parts := strings.Fields(candidate)
		if len(parts) > 0 {
			urls = append(urls, parts[0])
		}
	}
	return urls
}

func (cr *Crawler) heartbeat(done chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			checked := atomic.LoadInt64(&cr.checked)
			discovered := atomic.LoadInt64(&cr.discovered)
			errors := atomic.LoadInt64(&cr.errors)
			elapsed := time.Since(cr.start).Round(time.Second)
			queued := discovered - checked
			if queued < 0 {
				queued = 0
			}
			fmt.Fprintf(os.Stderr, "%d threads, %d queued, %d checked, %d errors, %v\n",
				cr.cfg.Checking.Threads, queued, checked, errors, elapsed,
			)
		}
	}
}
