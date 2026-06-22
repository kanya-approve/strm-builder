// Command strm-builder mirrors the media files on one or more WebDAV or HTTP
// directory-index servers into a tree of .strm files, each holding the source
// file's HTTP(S) URL, so a media server (Plex via plex-strm-assistant, Jellyfin,
// Emby, Kodi) can stream it without a FUSE mount in the playback path.
//
// Each source is probed once: a WebDAV PROPFIND if the server supports it,
// otherwise its HTML directory index (autoindex) is parsed. Output is laid out
// as <root>/<host>/<path-from-server-root>/<name>.strm. A source URL may include
// a subfolder: only that subfolder is crawled, but the path is still mirrored
// from the server root. Configured via flags or env.
package main

import (
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	propfindBody = `<?xml version="1.0" encoding="utf-8"?>` +
		`<d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`
	defaultExts = "mkv,mp4,avi,mov,m4v,wmv,flv,ts,m2ts,mpg,mpeg,webm,iso,vob,rmvb,3gp,divx,mka,m2v"
)

const maxIndexBytes = 8 << 20

var hrefRE = regexp.MustCompile(`(?i)<a\s[^>]*?href\s*=\s*["']([^"']*)["']`)

type sourceKind int

const (
	kindWebDAV sourceKind = iota
	kindHTTP
)

func (k sourceKind) String() string {
	if k == kindHTTP {
		return "http-index"
	}
	return "webdav"
}

type source struct {
	url  *url.URL
	host string
	user string
	pass string
	kind sourceKind
}

type config struct {
	sources     []*source
	outputDir   string
	mediaExts   map[string]bool
	allExts     bool
	embedCreds  bool
	concurrency int
	prune       bool
	dryRun      bool
	timeout     time.Duration
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		slog.Error("run failed", "err", err)
		os.Exit(1)
	}
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func loadConfig(args []string) (*config, error) {
	fs := flag.NewFlagSet("strm-builder", flag.ContinueOnError)
	var urlsFlag multiFlag
	fs.Var(&urlsFlag, "url", "source URL to crawl, credentials as user:pass@host (repeatable)")
	root := fs.String("root", getenv("ROOT_FOLDER", "/strm"), "root folder to save .strm trees under")
	extList := fs.String("ext", getenv("MEDIA_EXTENSIONS", defaultExts), "media extensions, or * for all")
	embed := fs.Bool("embed-creds", getbool("EMBED_CREDENTIALS", true), "embed user:pass@ in the .strm URLs")
	conc := fs.Int("concurrency", getint("CONCURRENCY", 8), "parallel PROPFIND requests")
	prune := fs.Bool("prune", getbool("PRUNE", false), "delete .strm whose source no longer exists")
	dry := fs.Bool("dry-run", getbool("DRY_RUN", false), "log actions without writing")
	timeout := fs.Duration("timeout", getdur("TIMEOUT", 30*time.Second), "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	raw := append([]string{}, urlsFlag...)
	raw = append(raw, fs.Args()...)
	raw = append(raw, splitList(getenv("SOURCE_URLS", ""))...)
	if len(raw) == 0 {
		return nil, errors.New("at least one source URL is required (-url, positional arg, or SOURCE_URLS)")
	}

	var sources []*source
	for _, r := range raw {
		u, err := url.Parse(strings.TrimRight(r, "/"))
		if err != nil {
			return nil, fmt.Errorf("source URL %q: %w", r, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("source URL %q must be http or https", r)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("source URL %q must include a host", r)
		}
		s := &source{host: sanitizeHost(u.Host)}
		if u.User != nil {
			s.user = u.User.Username()
			if p, ok := u.User.Password(); ok {
				s.pass = p
			}
			u.User = nil
		}
		s.url = u
		sources = append(sources, s)
	}

	exts := map[string]bool{}
	allExts := strings.TrimSpace(*extList) == "*"
	if !allExts {
		for _, e := range strings.Split(*extList, ",") {
			if e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), ".")); e != "" {
				exts[e] = true
			}
		}
	}
	if *conc < 1 {
		*conc = 1
	}
	if *timeout <= 0 {
		*timeout = 30 * time.Second
	}

	return &config{
		sources:     sources,
		outputDir:   *root,
		mediaExts:   exts,
		allExts:     allExts,
		embedCreds:  *embed,
		concurrency: *conc,
		prune:       *prune,
		dryRun:      *dry,
		timeout:     *timeout,
	}, nil
}

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func getbool(k string, def bool) bool {
	if v := getenv(k, ""); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getint(k string, def int) int {
	if v := getenv(k, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getdur(k string, def time.Duration) time.Duration {
	if v := getenv(k, ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t' || r == ' '
	})
}

func sanitizeHost(h string) string {
	return strings.NewReplacer(":", "_", "/", "_").Replace(h)
}

type entry struct {
	name  string
	isDir bool
	abs   *url.URL
}

type builder struct {
	cfg     *config
	client  *http.Client
	sem     chan struct{}
	wg      sync.WaitGroup
	written sync.Map

	created int64
	skipped int64
	dirs    int64
	errs    int64
}

func run(cfg *config) error {
	b := &builder{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.timeout},
		sem:    make(chan struct{}, cfg.concurrency),
	}
	start := time.Now()
	slog.Info("starting", "sources", len(cfg.sources), "output", cfg.outputDir,
		"concurrency", cfg.concurrency, "prune", cfg.prune, "dryRun", cfg.dryRun)
	for _, s := range cfg.sources {
		slog.Info("source", "url", s.url.String(), "bucket", s.host)
		b.wg.Add(1)
		go b.crawlSource(s)
	}
	b.wg.Wait()

	pruned := 0
	if cfg.prune && !cfg.dryRun {
		for _, s := range cfg.sources {
			scope := filepath.Join(cfg.outputDir, s.host, filepath.FromSlash(strings.Trim(s.url.Path, "/")))
			pruned += b.pruneStale(scope)
		}
	}

	slog.Info("done",
		"written", atomic.LoadInt64(&b.created), "unchanged", atomic.LoadInt64(&b.skipped),
		"dirs", atomic.LoadInt64(&b.dirs), "pruned", pruned, "errors", atomic.LoadInt64(&b.errs),
		"took", time.Since(start).Round(time.Millisecond).String())

	if n := atomic.LoadInt64(&b.errs); n > 0 {
		return fmt.Errorf("%d error(s) during crawl", n)
	}
	return nil
}

func (b *builder) crawlSource(s *source) {
	defer b.wg.Done()
	entries, err := b.probe(s)
	if err != nil {
		slog.Warn("listing failed", "url", s.url.String(), "err", err)
		atomic.AddInt64(&b.errs, 1)
		return
	}
	b.process(s, entries)
}

func (b *builder) walk(s *source, dirURL *url.URL) {
	defer b.wg.Done()
	entries, err := b.listDir(s, dirURL)
	if err != nil {
		slog.Warn("listing failed", "url", dirURL.String(), "err", err)
		atomic.AddInt64(&b.errs, 1)
		return
	}
	b.process(s, entries)
}

func (b *builder) process(s *source, entries []entry) {
	atomic.AddInt64(&b.dirs, 1)
	for _, e := range entries {
		if e.isDir {
			b.wg.Add(1)
			go b.walk(s, e.abs)
		} else if b.isMedia(e.name) {
			b.writeStrm(s, e.abs)
		}
	}
}

func (b *builder) probe(s *source) ([]entry, error) {
	entries, err := b.propfind(s, s.url)
	if err == nil {
		s.kind = kindWebDAV
		slog.Info("detected protocol", "url", s.url.String(), "protocol", s.kind)
		return entries, nil
	}
	slog.Debug("PROPFIND failed, trying HTTP index", "url", s.url.String(), "err", err)
	s.kind = kindHTTP
	entries, err = b.httpList(s, s.url)
	if err == nil {
		slog.Info("detected protocol", "url", s.url.String(), "protocol", s.kind)
	}
	return entries, err
}

func (b *builder) listDir(s *source, dirURL *url.URL) ([]entry, error) {
	if s.kind == kindHTTP {
		return b.httpList(s, dirURL)
	}
	return b.propfind(s, dirURL)
}

func (b *builder) propfind(s *source, dirURL *url.URL) ([]entry, error) {
	b.sem <- struct{}{}
	defer func() { <-b.sem }()

	u := *dirURL
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/" // WebDAV collections must be requested with a trailing slash
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "PROPFIND", u.String(), strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	if s.user != "" {
		req.SetBasicAuth(s.user, s.pass)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return parseMultistatus(&u, resp.Body)
}

func parseMultistatus(base *url.URL, r io.Reader) ([]entry, error) {
	var ms struct {
		Responses []struct {
			Href     string `xml:"href"`
			Propstat []struct {
				ResourceType struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"prop>resourcetype"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(r).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decode multistatus: %w", err)
	}

	self := strings.TrimRight(base.Path, "/")
	var out []entry
	for _, resp := range ms.Responses {
		if resp.Href == "" {
			continue
		}
		abs, err := base.Parse(resp.Href)
		if err != nil {
			continue
		}
		if strings.TrimRight(abs.Path, "/") == self {
			continue
		}
		isDir := false
		for _, ps := range resp.Propstat {
			if ps.ResourceType.Collection != nil {
				isDir = true
			}
		}
		out = append(out, entry{
			name:  path.Base(strings.TrimRight(abs.Path, "/")),
			isDir: isDir,
			abs:   abs,
		})
	}
	return out, nil
}

func (b *builder) httpList(s *source, dirURL *url.URL) ([]entry, error) {
	b.sem <- struct{}{}
	defer func() { <-b.sem }()

	u := *dirURL
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/" // request the directory index, not a redirect to it
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if s.user != "" {
		req.SetBasicAuth(s.user, s.pass)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxIndexBytes {
		slog.Warn("directory index truncated; some entries may be skipped", "url", u.String(), "limit", maxIndexBytes)
		body = body[:maxIndexBytes]
	}
	return parseHTMLIndex(&u, body), nil
}

// parseHTMLIndex extracts the direct children of an HTTP autoindex page. It keeps
// only same-host links one level below base, dropping parent, self, and sort
// links; a trailing slash marks a directory.
func parseHTMLIndex(base *url.URL, body []byte) []entry {
	dirPath := base.Path
	if !strings.HasSuffix(dirPath, "/") {
		dirPath += "/"
	}
	seen := map[string]bool{}
	var out []entry
	for _, m := range hrefRE.FindAllSubmatch(body, -1) {
		href := strings.TrimSpace(html.UnescapeString(string(m[1])))
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "?") {
			continue
		}
		abs, err := base.Parse(href)
		if err != nil || abs.Scheme != base.Scheme || abs.Host != base.Host {
			continue
		}
		if !strings.HasPrefix(abs.Path, dirPath) {
			continue
		}
		rest := strings.Trim(strings.TrimPrefix(abs.Path, dirPath), "/")
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		if seen[abs.Path] {
			continue
		}
		seen[abs.Path] = true
		abs.RawQuery, abs.Fragment = "", ""
		out = append(out, entry{
			name:  path.Base(strings.TrimRight(abs.Path, "/")),
			isDir: strings.HasSuffix(abs.Path, "/"),
			abs:   abs,
		})
	}
	return out
}

func (b *builder) isMedia(name string) bool {
	if b.cfg.allExts {
		return true
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return ext != "" && b.cfg.mediaExts[ext]
}

func (b *builder) writeStrm(s *source, abs *url.URL) {
	rel := strings.TrimPrefix(abs.Path, "/")
	if rel == "" {
		return
	}
	out := filepath.Join(b.cfg.outputDir, s.host, filepath.FromSlash(rel))
	out = strings.TrimSuffix(out, filepath.Ext(out)) + ".strm"
	b.written.Store(out, struct{}{})

	content := b.fileURL(s, abs) + "\n"
	if cur, err := os.ReadFile(out); err == nil && string(cur) == content {
		atomic.AddInt64(&b.skipped, 1)
		return
	}
	if b.cfg.dryRun {
		slog.Info("would write", "strm", out)
		atomic.AddInt64(&b.created, 1)
		return
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		slog.Warn("mkdir failed", "dir", filepath.Dir(out), "err", err)
		atomic.AddInt64(&b.errs, 1)
		return
	}
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		slog.Warn("write failed", "path", out, "err", err)
		atomic.AddInt64(&b.errs, 1)
		return
	}
	atomic.AddInt64(&b.created, 1)
}

func (b *builder) fileURL(s *source, abs *url.URL) string {
	u := *abs
	if b.cfg.embedCreds && s.user != "" {
		u.User = url.UserPassword(s.user, s.pass)
	}
	return u.String()
}

// pruneStale removes .strm under scope not written this run; scope is assumed to be owned by this tool.
func (b *builder) pruneStale(scope string) int {
	count := 0
	var dirs []string
	filepath.WalkDir(scope, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), ".strm") {
			if _, ok := b.written.Load(p); !ok {
				if os.Remove(p) == nil {
					count++
					slog.Info("pruned", "strm", p)
				}
			}
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		if dirs[i] != scope {
			if entries, _ := os.ReadDir(dirs[i]); len(entries) == 0 {
				os.Remove(dirs[i])
			}
		}
	}
	return count
}
