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

// Result is a single error written as one JSONL line.
type Result struct {
	URL       string `json:"url"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error"`
	Parent    string `json:"parent,omitempty"`
	Timestamp string `json:"timestamp"`
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

	mu         sync.Mutex
	checked    int64
	errors     int64
	discovered int64
	seen       sync.Map
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

	// TLS
	c.WithTransport(&http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: !cr.cfg.Checking.SSLVerify,
		},
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

	// Cookies — set on every request.
	if len(cr.cfg.Cookies) > 0 {
		c.OnRequest(func(r *colly.Request) {
			for name, value := range cr.cfg.Cookies {
				r.Headers.Set("Cookie", name+"="+value)
			}
		})
	}

	// nofollow tracker: URLs we'll check but won't extract links from.
	nofollowSet := &sync.Map{}

	// Parent tracker: records the first page that linked to each URL.
	parentMap := &sync.Map{}

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
		parentMap.LoadOrStore(link, currentURL)

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
	})

	c.OnError(func(r *colly.Response, err error) {
		errMsg := err.Error()

		// Colly v2.3.0 fires OnError for revisit attempts and
		// redirects to disallowed domains. Neither is a real failure.
		if strings.Contains(errMsg, "already visited") ||
			strings.Contains(errMsg, "Forbidden domain") {
			return
		}

		atomic.AddInt64(&cr.checked, 1)

		atomic.AddInt64(&cr.errors, 1)

		var parent string
		if v, ok := parentMap.Load(r.Request.URL.String()); ok {
			parent = v.(string)
		}

		result := Result{
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
