package main

// Serve mode: a request-driven bridge. A media-request front-end (Overseerr /
// Jellyseerr / Seerr) fires a webhook when a request is approved; this bridge
// resolves the title through ANY Stremio stream addon (configurable, nothing
// hardcoded) and writes a .strm file laid out for the selected media server.
//
//   request (Seerr webhook)  ->  Stremio addon /stream/{type}/{id}.json
//     ->  pick a direct URL  ->  Movies/Title (Year)/Title (Year).strm
//                                Shows/Title (Year)/Season 01/Title - S01E01.strm
//
// The addon must return streams carrying a direct http(s) `url` (debrid-backed
// addons do); torrent-only (infoHash) streams have no URL and are skipped.
// TMDB is used for titles/years and for expanding a series into episodes, so a
// TMDB API key is required - the same one the Seerr instance already uses.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ptt "github.com/itsrenoria/ptt-go"
)

const tmdbBase = "https://api.themoviedb.org/3"

type serveConfig struct {
	addon          string // Stremio addon base URL, no trailing /manifest.json
	root           string // output root for the .strm trees
	target         string // plex | jellyfin | emby | kodi (layout preset)
	listen         string // HTTP listen address
	tmdbKey        string // TMDB v3 API key
	seerrURL       string // Overseerr/Jellyseerr base URL (empty disables the callback)
	seerrKey       string // Overseerr/Jellyseerr API key
	versions       int    // how many versions (by size) to keep per item
	pick           string // "largest" or "smallest" - which single version to keep
	sample         int    // addon calls to union before ranking
	secret         string // optional shared secret required on the webhook
	anime          bool   // route Japanese animation into its own top folder
	animeDir       string // anime series folder
	animeMoviesDir string // anime movies folder (empty = put them in Movies)
	dryRun         bool
	timeout        time.Duration
	concurrency    int
}

// addonManifest is the subset of a Stremio addon manifest we need to know how to
// address content (which id scheme it accepts).
type addonManifest struct {
	Name       string   `json:"name"`
	IDPrefixes []string `json:"idPrefixes"`
	Types      []string `json:"types"`
}

func loadServeConfig(args []string) (*serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addon := fs.String("addon", getenv("STREMIO_ADDON", ""), "Stremio addon URL (the manifest.json URL, or its base)")
	root := fs.String("root", getenv("ROOT_FOLDER", "/strm"), "root folder to write the .strm tree under")
	target := fs.String("target", getenv("TARGET", "plex"), "layout preset: plex, jellyfin, emby, or kodi")
	listen := fs.String("listen", getenv("LISTEN_ADDR", ":8080"), "HTTP listen address for the webhook")
	tmdb := fs.String("tmdb-key", getenv("TMDB_API_KEY", ""), "TMDB v3 API key (required)")
	seerrURL := fs.String("seerr-url", getenv("SEERR_URL", ""), "Overseerr/Jellyseerr base URL, to mark requests available after fulfilling")
	seerrKey := fs.String("seerr-api-key", getenv("SEERR_API_KEY", ""), "Overseerr/Jellyseerr API key")
	versions := fs.Int("versions", getint("VERSIONS", 1), "how many versions (largest first) to keep per item as separate .strm files")
	pick := fs.String("pick", getenv("PICK", "largest"), "which single version to keep: largest or smallest")
	sample := fs.Int("sample", getint("SAMPLE", 1), "addon calls to union before ranking (it returns a random subset per call; raise for -versions >1)")
	secret := fs.String("webhook-secret", getenv("WEBHOOK_SECRET", ""), "if set, require this value in the webhook Authorization header or ?secret=")
	anime := fs.Bool("anime", getbool("ANIME_SPLIT", true), "route anime (Japanese animation) into its own top folder")
	animeDir := fs.String("anime-folder", getenv("ANIME_FOLDER", "Anime"), "folder for anime series when -anime is set")
	animeMoviesDir := fs.String("anime-movies-folder", getenv("ANIME_MOVIES_FOLDER", "Anime Movies"), "folder for anime movies; empty puts them in Movies")
	dry := fs.Bool("dry-run", getbool("DRY_RUN", false), "log actions without writing")
	timeout := fs.Duration("timeout", getdur("TIMEOUT", 30*time.Second), "per-request timeout")
	conc := fs.Int("concurrency", getint("CONCURRENCY", 2), "parallel episode resolutions (addons rate-limit; keep it low)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	a := strings.TrimRight(strings.TrimSpace(*addon), "/")
	a = strings.TrimSuffix(a, "/manifest.json")
	a = strings.TrimRight(a, "/")
	if a == "" {
		return nil, errors.New("a Stremio addon URL is required (-addon or STREMIO_ADDON)")
	}
	if !strings.HasPrefix(a, "http://") && !strings.HasPrefix(a, "https://") {
		return nil, fmt.Errorf("addon URL %q must be http or https", a)
	}
	switch *target {
	case "plex", "jellyfin", "emby", "kodi":
	default:
		return nil, fmt.Errorf("target %q must be one of plex, jellyfin, emby, kodi", *target)
	}
	if strings.TrimSpace(*tmdb) == "" {
		return nil, errors.New("a TMDB API key is required (-tmdb-key or TMDB_API_KEY)")
	}
	switch *pick {
	case "largest", "smallest":
	default:
		return nil, fmt.Errorf("pick %q must be largest or smallest", *pick)
	}
	if *versions < 1 {
		*versions = 1
	}
	if *sample < 1 {
		*sample = 1
	}
	if *conc < 1 {
		*conc = 1
	}
	if *timeout <= 0 {
		*timeout = 30 * time.Second
	}

	return &serveConfig{
		addon:          a,
		root:           *root,
		target:         *target,
		listen:         *listen,
		tmdbKey:        strings.TrimSpace(*tmdb),
		seerrURL:       strings.TrimRight(strings.TrimSpace(*seerrURL), "/"),
		seerrKey:       strings.TrimSpace(*seerrKey),
		versions:       *versions,
		pick:           *pick,
		sample:         *sample,
		secret:         strings.TrimSpace(*secret),
		anime:          *anime,
		animeDir:       strings.TrimSpace(*animeDir),
		animeMoviesDir: strings.TrimSpace(*animeMoviesDir),
		dryRun:         *dry,
		timeout:        *timeout,
		concurrency:    *conc,
	}, nil
}

type bridge struct {
	cfg      *serveConfig
	client   *http.Client
	manifest addonManifest
	inflight sync.Map // fulfil key -> struct{}, coalesces duplicate requests
}

func runServe(args []string) error {
	cfg, err := loadServeConfig(args)
	if err != nil {
		return err
	}
	b := &bridge{cfg: cfg, client: &http.Client{Timeout: cfg.timeout}}

	if m, err := b.fetchManifest(); err != nil {
		slog.Warn("could not read addon manifest; will address content by tmdb id", "addon", cfg.addon, "err", err)
	} else {
		b.manifest = m
		slog.Info("addon", "name", m.Name, "idPrefixes", m.IDPrefixes, "types", m.Types)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/webhook", b.handleWebhook)
	mux.HandleFunc("/fulfill", b.handleManual)

	slog.Info("serve: listening", "addr", cfg.listen, "target", cfg.target, "root", cfg.root,
		"versions", cfg.versions, "pick", cfg.pick, "sample", cfg.sample, "dryRun", cfg.dryRun)
	return http.ListenAndServe(cfg.listen, mux)
}

func (b *bridge) fetchManifest() (addonManifest, error) {
	var m addonManifest
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.addon+"/manifest.json", nil)
	if err != nil {
		return m, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return m, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return m, fmt.Errorf("manifest status %s", resp.Status)
	}
	return m, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m)
}

// flexID unmarshals a JSON value that may be a number or a string into a string,
// since Seerr webhook templates emit tmdb/tvdb ids either way.
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	*f = flexID(strings.Trim(string(b), `"`))
	return nil
}

type seerrHook struct {
	NotificationType string `json:"notification_type"`
	Subject          string `json:"subject"`
	Media            struct {
		MediaType string `json:"media_type"`
		TmdbID    flexID `json:"tmdbId"`
	} `json:"media"`
	Request struct {
		RequestID flexID `json:"request_id"`
	} `json:"request"`
	Extra []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"extra"`
}

type fulfillReq struct {
	mediaType string // "movie" or "tv"
	tmdbID    string
	seasons   []int  // empty = all regular seasons (tv only)
	episode   int    // set with a single season for a one-episode direct-url write
	url       string // if set, written verbatim instead of resolving via the addon
	requestID string // Seerr request id, to mark it available on completion
}

func (b *bridge) authOK(r *http.Request) bool {
	if b.cfg.secret == "" {
		return true
	}
	if r.URL.Query().Get("secret") == b.cfg.secret {
		return true
	}
	h := r.Header.Get("Authorization")
	h = strings.TrimPrefix(h, "Bearer ")
	return h == b.cfg.secret || r.Header.Get("X-Webhook-Secret") == b.cfg.secret
}

func (b *bridge) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !b.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var hook seerrHook
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&hook); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	if hook.NotificationType == "TEST_NOTIFICATION" {
		slog.Info("webhook: test notification ok")
		io.WriteString(w, "ok\n")
		return
	}
	switch hook.NotificationType {
	case "MEDIA_APPROVED", "MEDIA_AUTO_APPROVED":
	default:
		slog.Debug("webhook: ignoring", "type", hook.NotificationType)
		io.WriteString(w, "ignored\n")
		return
	}

	req, err := hookToReq(hook)
	if err != nil {
		slog.Warn("webhook: unusable payload", "err", err, "subject", hook.Subject)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	slog.Info("webhook: accepted", "type", req.mediaType, "tmdb", req.tmdbID, "seasons", req.seasons, "subject", hook.Subject)
	go b.fulfill(req)
	w.WriteHeader(http.StatusAccepted)
	io.WriteString(w, "accepted\n")
}

func hookToReq(h seerrHook) (fulfillReq, error) {
	mt := h.Media.MediaType
	if mt != "movie" && mt != "tv" {
		return fulfillReq{}, fmt.Errorf("unsupported media_type %q", mt)
	}
	if string(h.Media.TmdbID) == "" {
		return fulfillReq{}, errors.New("missing tmdbId")
	}
	return fulfillReq{
		mediaType: mt,
		tmdbID:    string(h.Media.TmdbID),
		seasons:   parseSeasons(h.Extra),
		requestID: string(h.Request.RequestID),
	}, nil
}

var digitsRE = regexp.MustCompile(`\d+`)

func parseSeasons(extra []struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}) []int {
	var out []int
	for _, e := range extra {
		if !strings.Contains(strings.ToLower(e.Name), "season") {
			continue
		}
		for _, m := range digitsRE.FindAllString(e.Value, -1) {
			if n, err := strconv.Atoi(m); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// handleManual is a test / backfill / generic-URL hook:
//
//	/fulfill?type=movie&tmdb=278                     resolve a movie via the addon
//	/fulfill?type=tv&tmdb=1396&season=1              resolve a whole season via the addon
//	/fulfill?type=movie&tmdb=278&url=https://…/x.mkv write a given URL, named from TMDB
//	/fulfill?type=tv&tmdb=1396&season=1&episode=1&url=https://…/x.mkv   one episode, verbatim
func (b *bridge) handleManual(w http.ResponseWriter, r *http.Request) {
	if !b.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	mt := q.Get("type")
	if mt == "" {
		mt = "movie"
	}
	if mt != "movie" && mt != "tv" {
		http.Error(w, "type must be movie or tv", http.StatusBadRequest)
		return
	}
	if q.Get("tmdb") == "" {
		http.Error(w, "tmdb query param required", http.StatusBadRequest)
		return
	}
	req := fulfillReq{mediaType: mt, tmdbID: q.Get("tmdb"), url: strings.TrimSpace(q.Get("url"))}
	if s := q.Get("season"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			req.seasons = []int{n}
		}
	}
	if e := q.Get("episode"); e != "" {
		if n, err := strconv.Atoi(e); err == nil {
			req.episode = n
		}
	}
	go b.fulfill(req)
	w.WriteHeader(http.StatusAccepted)
	io.WriteString(w, "accepted\n")
}

func (b *bridge) fulfill(req fulfillReq) {
	key := req.mediaType + ":" + req.tmdbID
	if _, busy := b.inflight.LoadOrStore(key, struct{}{}); busy {
		slog.Info("already fulfilling", "key", key)
		return
	}
	defer b.inflight.Delete(key)

	var written, missing int64
	status := "available"
	start := time.Now()
	if req.mediaType == "movie" {
		written, missing = b.fulfillMovie(req)
	} else {
		written, missing, status = b.fulfillSeries(req)
	}
	slog.Info("fulfilled", "key", key, "written", written, "missing", missing, "status", status,
		"took", time.Since(start).Round(time.Millisecond).String())

	if written > 0 && req.requestID != "" {
		b.updateSeerr(req.requestID, status)
	}
}

func (b *bridge) updateSeerr(requestID, status string) {
	if b.cfg.seerrURL == "" || b.cfg.seerrKey == "" {
		return
	}
	var info struct {
		Is4k  bool `json:"is4k"`
		Media struct {
			ID int `json:"id"`
		} `json:"media"`
	}
	if err := b.seerrDo(http.MethodGet, "/api/v1/request/"+requestID, nil, &info); err != nil {
		slog.Warn("seerr: request lookup failed", "request", requestID, "err", err)
		return
	}
	if info.Media.ID == 0 {
		slog.Warn("seerr: no media id for request", "request", requestID)
		return
	}
	if err := b.seerrDo(http.MethodPost, fmt.Sprintf("/api/v1/media/%d/%s", info.Media.ID, status), map[string]bool{"is4k": info.Is4k}, nil); err != nil {
		slog.Warn("seerr: status update failed", "media", info.Media.ID, "status", status, "err", err)
		return
	}
	slog.Info("seerr: status updated", "request", requestID, "media", info.Media.ID, "status", status, "is4k", info.Is4k)
}

func (b *bridge) seerrDo(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, b.cfg.seerrURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", b.cfg.seerrKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("status %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return nil
}

func (b *bridge) fulfillMovie(req fulfillReq) (written, missing int64) {
	meta, err := b.tmdbMovie(req.tmdbID)
	if err != nil {
		slog.Warn("tmdb movie lookup failed", "tmdb", req.tmdbID, "err", err)
		return 0, 1
	}
	anime := meta.isAnime()

	if req.url != "" {
		out := b.moviePath(meta.title(), meta.year(), req.tmdbID, releaseName(req.url), anime)
		if b.writeStrm(out, req.url) {
			return 1, 0
		}
		return 0, 0
	}

	id := b.baseID(req.tmdbID, meta.ImdbID)
	cands, err := b.resolveStreams("movie", id)
	if err != nil || len(cands) == 0 {
		slog.Warn("no stream for movie", "title", meta.title(), "id", id, "err", err)
		return 0, 1
	}
	for _, p := range b.selectVersions(cands) {
		out := b.moviePath(meta.title(), meta.year(), req.tmdbID, releaseName(p.url), anime)
		if b.writeStrm(out, p.url) {
			written++
		}
	}
	return written, 0
}

func (b *bridge) fulfillSeries(req fulfillReq) (written, missing int64, status string) {
	meta, err := b.tmdbTV(req.tmdbID)
	if err != nil {
		slog.Warn("tmdb tv lookup failed", "tmdb", req.tmdbID, "err", err)
		return 0, 1, "partial"
	}
	anime := meta.isAnime()
	tvdbID := meta.tvdb()

	if req.url != "" {
		if req.episode == 0 || len(req.seasons) != 1 {
			slog.Warn("direct url for tv needs exactly one season and an episode", "tmdb", req.tmdbID)
			return 0, 1, "partial"
		}
		out := b.episodePath(meta.title(), meta.year(), tvdbID, req.tmdbID, req.seasons[0], releaseName(req.url), anime)
		if b.writeStrm(out, req.url) {
			return 1, 0, "partial"
		}
		return 0, 0, "partial"
	}

	seasons := req.seasons
	if len(seasons) == 0 {
		for _, s := range meta.Seasons {
			if s.SeasonNumber > 0 {
				seasons = append(seasons, s.SeasonNumber)
			}
		}
	}

	sem := make(chan struct{}, b.cfg.concurrency)
	var wg sync.WaitGroup
	for _, sn := range seasons {
		eps, err := b.tmdbSeason(req.tmdbID, sn)
		if err != nil {
			slog.Warn("tmdb season lookup failed", "tmdb", req.tmdbID, "season", sn, "err", err)
			continue
		}
		for _, ep := range eps {
			wg.Add(1)
			sem <- struct{}{}
			go func(season, episode int) {
				defer wg.Done()
				defer func() { <-sem }()
				id := fmt.Sprintf("%s:%d:%d", b.baseID(req.tmdbID, meta.ImdbID), season, episode)
				cands, err := b.resolveStreams("series", id)
				if err != nil || len(cands) == 0 {
					slog.Warn("no stream for episode", "show", meta.title(), "s", season, "e", episode, "err", err)
					atomic.AddInt64(&missing, 1)
					return
				}
				for _, p := range b.selectVersions(cands) {
					out := b.episodePath(meta.title(), meta.year(), tvdbID, req.tmdbID, season, releaseName(p.url), anime)
					if b.writeStrm(out, p.url) {
						atomic.AddInt64(&written, 1)
					}
				}
			}(sn, ep)
		}
	}
	wg.Wait()

	totalRegular := 0
	for _, s := range meta.Seasons {
		if s.SeasonNumber > 0 {
			totalRegular++
		}
	}
	status = "partial"
	if missing == 0 && len(seasons) >= totalRegular {
		status = "available"
	}
	return written, missing, status
}

// baseID picks the id scheme the addon accepts: tmdb: when advertised (or when we
// know nothing), otherwise an imdb tt id when we have one.
func (b *bridge) baseID(tmdbID, imdbID string) string {
	tmdbOK, ttOK := false, false
	for _, p := range b.manifest.IDPrefixes {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "tmdb:":
			tmdbOK = true
		case "tt":
			ttOK = true
		}
	}
	if len(b.manifest.IDPrefixes) == 0 {
		tmdbOK = true // manifest unknown: tmdb ids are what Seerr gives us
	}
	if !tmdbOK && ttOK && imdbID != "" {
		return imdbID
	}
	return "tmdb:" + tmdbID
}

type stremioStream struct {
	Title         string `json:"title"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	BehaviorHints struct {
		VideoSize int64 `json:"videoSize"`
	} `json:"behaviorHints"`
}

type candidate struct {
	url  string
	size int64
	res  string
}

var sizeRE = regexp.MustCompile(`(?i)([\d.]+)\s*(TB|GB|MB)`)

// streamSize prefers the structured behaviorHints.videoSize (bytes); otherwise it
// parses a "12.3 GB"-style figure out of the title, which is where this addon
// puts it.
func streamSize(s stremioStream) int64 {
	if s.BehaviorHints.VideoSize > 0 {
		return s.BehaviorHints.VideoSize
	}
	m := sizeRE.FindStringSubmatch(s.Title + " " + s.Name)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(m[2]) {
	case "TB":
		return int64(v * (1 << 40))
	case "GB":
		return int64(v * (1 << 30))
	default:
		return int64(v * (1 << 20))
	}
}

// streamResolution parses the release name with ptt-go for a normalised
// resolution (2160p/1080p/720p/…); a light regex is the fallback when ptt-go
// finds none.
func streamResolution(s stremioStream) string {
	if r := ptt.Parse(s.Title).Resolution; r != "" {
		return r
	}
	t := strings.ToLower(s.Title + " " + s.Name)
	switch {
	case strings.Contains(t, "2160p") || strings.Contains(t, "4k") || strings.Contains(t, "uhd"):
		return "2160p"
	case strings.Contains(t, "1080p") || strings.Contains(t, "1080i"):
		return "1080p"
	case strings.Contains(t, "720p"):
		return "720p"
	case strings.Contains(t, "480p") || strings.Contains(t, "576p"):
		return "480p"
	}
	return ""
}

func resRank(res string) int {
	switch res {
	case "2160p", "4k":
		return 5
	case "1080p":
		return 4
	case "720p":
		return 3
	case "576p":
		return 2
	case "480p":
		return 1
	}
	return 0
}

// resolveStreams queries the addon (cfg.sample times, unioned by URL) and returns
// every stream carrying a direct URL, each tagged with its parsed resolution and
// size. The addon returns a shuffled random SUBSET per call; sampling widens
// coverage when keeping more than one resolution.
func (b *bridge) resolveStreams(addonType, id string) ([]candidate, error) {
	u := b.cfg.addon + "/stream/" + addonType + "/" + id + ".json"
	seen := map[string]candidate{}
	var firstErr error
	for i := 0; i < b.cfg.sample; i++ {
		data, err := b.getBody(u, 8<<20)
		if err != nil {
			firstErr = err
			continue
		}
		var body struct {
			Streams []stremioStream `json:"streams"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			firstErr = err
			continue
		}
		for _, s := range body.Streams {
			if s.URL == "" {
				continue
			}
			if _, ok := seen[s.URL]; !ok {
				seen[s.URL] = candidate{url: s.URL, size: streamSize(s), res: streamResolution(s)}
			}
		}
	}
	if len(seen) == 0 {
		return nil, firstErr
	}
	out := make([]candidate, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	return out, nil
}

// selectVersions ranks by quality: it keeps the best release of each resolution
// (largest by size, or smallest with pick=smallest), orders those resolutions
// best-first, and returns the top cfg.versions of them. So -versions 1 yields the
// largest file of the highest resolution; -versions 2 adds the next resolution
// down, and so on.
func (b *bridge) selectVersions(cands []candidate) []candidate {
	best := map[string]candidate{}
	for _, c := range cands {
		cur, ok := best[c.res]
		if !ok || betterWithin(c, cur, b.cfg.pick) {
			best[c.res] = c
		}
	}
	reps := make([]candidate, 0, len(best))
	for _, c := range best {
		reps = append(reps, c)
	}
	sort.Slice(reps, func(i, j int) bool {
		if ri, rj := resRank(reps[i].res), resRank(reps[j].res); ri != rj {
			return ri > rj
		}
		return reps[i].size > reps[j].size
	})
	if len(reps) > b.cfg.versions {
		reps = reps[:b.cfg.versions]
	}
	return reps
}

// betterWithin reports whether c beats cur for the same resolution: largest wins
// by default, smallest with pick=smallest; URL breaks size ties deterministically.
func betterWithin(c, cur candidate, pick string) bool {
	if c.size != cur.size {
		if pick == "smallest" {
			return c.size < cur.size
		}
		return c.size > cur.size
	}
	return c.url < cur.url
}

// versionLabels returns a filename suffix per kept version. One version → no
// suffix (clean name). Multiple → the resolution (each kept version is a distinct
// resolution, so these never collide); unknown resolution becomes "SD".
// getBody GETs url and returns the body, retrying on 429/5xx with backoff that
// honours a Retry-After header. Addons commonly rate-limit, so this keeps a
// season's worth of episode lookups from failing under a burst.
func (b *bridge) getBody(url string, limit int64) ([]byte, error) {
	const maxAttempts = 5
	var lastErr error
	var hint time.Duration
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(backoff(attempt-1, hint))
		}
		ctx, cancel := context.WithTimeout(context.Background(), b.cfg.timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		resp, err := b.client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			data, rerr := io.ReadAll(io.LimitReader(resp.Body, limit))
			resp.Body.Close()
			cancel()
			return data, rerr
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		hint = parseRetryAfter(resp.Header.Get("Retry-After"))
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		cancel()
		lastErr = fmt.Errorf("status %s", resp.Status)
		if !retryable {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func backoff(retry int, hint time.Duration) time.Duration {
	d := 500 * time.Millisecond << (retry - 1) // 500ms, 1s, 2s, 4s...
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	if hint > d {
		d = hint
	}
	return d
}

func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

func (b *bridge) moviesDir() string { return "Movies" }

func (b *bridge) showsDir() string {
	if b.cfg.target == "plex" || b.cfg.target == "kodi" {
		return "TV Shows"
	}
	return "Shows"
}

// topDir keeps each library one type: anime series and anime movies go to
// separate folders (a Plex library can't mix movies and shows). An empty anime
// folder name falls back to the regular Movies/TV folder.
func (b *bridge) topDir(mediaType string, anime bool) string {
	if anime && b.cfg.anime {
		if mediaType == "movie" {
			if b.cfg.animeMoviesDir != "" {
				return b.cfg.animeMoviesDir
			}
		} else if b.cfg.animeDir != "" {
			return b.cfg.animeDir
		}
	}
	if mediaType == "movie" {
		return b.moviesDir()
	}
	return b.showsDir()
}

func (b *bridge) moviePath(title, year, tmdbID, release string, anime bool) string {
	folder := titleYear(title, year) + " {tmdb-" + tmdbID + "}"
	return filepath.Join(b.cfg.root, b.topDir("movie", anime), folder, strmName(release))
}

func (b *bridge) episodePath(show, year, tvdbID, tmdbID string, season int, release string, anime bool) string {
	folder := titleYear(show, year) + " " + seriesIDTag(tvdbID, tmdbID)
	return filepath.Join(b.cfg.root, b.topDir("tv", anime), folder, fmt.Sprintf("Season %02d", season), strmName(release))
}

func seriesIDTag(tvdbID, tmdbID string) string {
	if tvdbID != "" {
		return "{tvdb-" + tvdbID + "}"
	}
	return "{tmdb-" + tmdbID + "}"
}

func releaseName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

func strmName(release string) string {
	return sanitizeName(strings.TrimSuffix(release, filepath.Ext(release))) + ".strm"
}

func titleYear(title, year string) string {
	t := sanitizeName(title)
	if year != "" {
		return fmt.Sprintf("%s (%s)", t, year)
	}
	return t
}

var illegalFname = regexp.MustCompile(`[\\/:*?"<>|]`)

func sanitizeName(s string) string {
	s = illegalFname.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(s, ". ")
}

// writeStrm writes url to out (idempotently). Returns true when a file was created
// or its content changed.
func (b *bridge) writeStrm(out, url string) bool {
	content := url + "\n"
	if cur, err := os.ReadFile(out); err == nil && string(cur) == content {
		slog.Debug("unchanged", "strm", out)
		return false
	}
	if b.cfg.dryRun {
		slog.Info("would write", "strm", out, "url", url)
		return true
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		slog.Warn("mkdir failed", "dir", filepath.Dir(out), "err", err)
		return false
	}
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		slog.Warn("write failed", "path", out, "err", err)
		return false
	}
	slog.Info("wrote", "strm", out)
	return true
}

type tmdbKeyword struct {
	ID int `json:"id"`
}

type tmdbMeta struct {
	Name         string `json:"name"`  // tv
	Title        string `json:"title"` // movie
	FirstAirDate string `json:"first_air_date"`
	ReleaseDate  string `json:"release_date"`
	Seasons      []struct {
		SeasonNumber int `json:"season_number"`
		EpisodeCount int `json:"episode_count"`
	} `json:"seasons"`
	Genres []struct {
		ID int `json:"id"`
	} `json:"genres"`
	OriginalLanguage string   `json:"original_language"`
	OriginCountry    []string `json:"origin_country"`
	Keywords         struct {
		Keywords []tmdbKeyword `json:"keywords"` // movie shape
		Results  []tmdbKeyword `json:"results"`  // tv shape
	} `json:"keywords"`
	ExternalIDs struct {
		ImdbID string `json:"imdb_id"`
		TvdbID int    `json:"tvdb_id"`
	} `json:"external_ids"`
	ImdbID string `json:"imdb_id"` // present directly on /movie
}

func (m tmdbMeta) tvdb() string {
	if m.ExternalIDs.TvdbID > 0 {
		return strconv.Itoa(m.ExternalIDs.TvdbID)
	}
	return ""
}

// isAnime mirrors how Overseerr/Jellyseerr separate anime: the TMDB "anime"
// keyword (210024). A Japanese-animation heuristic is kept as a fallback for the
// rare title that lacks the keyword.
func (m tmdbMeta) isAnime() bool {
	const animeKeywordID = 210024
	for _, k := range append(append([]tmdbKeyword{}, m.Keywords.Keywords...), m.Keywords.Results...) {
		if k.ID == animeKeywordID {
			return true
		}
	}
	animation := false
	for _, g := range m.Genres {
		if g.ID == 16 {
			animation = true
		}
	}
	if !animation {
		return false
	}
	if m.OriginalLanguage == "ja" {
		return true
	}
	for _, c := range m.OriginCountry {
		if c == "JP" {
			return true
		}
	}
	return false
}

func (m tmdbMeta) title() string {
	if m.Title != "" {
		return m.Title
	}
	return m.Name
}

func (m tmdbMeta) year() string {
	d := m.ReleaseDate
	if d == "" {
		d = m.FirstAirDate
	}
	if len(d) >= 4 {
		return d[:4]
	}
	return ""
}

func (m tmdbMeta) imdb() string {
	if m.ImdbID != "" {
		return m.ImdbID
	}
	return m.ExternalIDs.ImdbID
}

// tmdbMovie/tmdbTV flatten ImdbID onto the returned struct so callers can read meta.ImdbID.
func (b *bridge) tmdbMovie(id string) (tmdbMeta, error) {
	m, err := b.tmdbGet("/movie/" + id + "?append_to_response=external_ids,keywords")
	m.ImdbID = m.imdb()
	return m, err
}

func (b *bridge) tmdbTV(id string) (tmdbMeta, error) {
	m, err := b.tmdbGet("/tv/" + id + "?append_to_response=external_ids,keywords")
	m.ImdbID = m.imdb()
	return m, err
}

func (b *bridge) tmdbSeason(id string, season int) ([]int, error) {
	var body struct {
		Episodes []struct {
			EpisodeNumber int `json:"episode_number"`
		} `json:"episodes"`
	}
	if err := b.tmdbGetInto(fmt.Sprintf("/tv/%s/season/%d", id, season), &body); err != nil {
		return nil, err
	}
	var out []int
	for _, e := range body.Episodes {
		out = append(out, e.EpisodeNumber)
	}
	return out, nil
}

func (b *bridge) tmdbGet(path string) (tmdbMeta, error) {
	var m tmdbMeta
	err := b.tmdbGetInto(path, &m)
	return m, err
}

func (b *bridge) tmdbGetInto(path string, v any) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	u := tmdbBase + path + sep + "api_key=" + b.cfg.tmdbKey
	data, err := b.getBody(u, 8<<20)
	if err != nil {
		return fmt.Errorf("tmdb %s: %w", path, err)
	}
	return json.Unmarshal(data, v)
}
