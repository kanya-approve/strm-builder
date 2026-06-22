package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type child struct {
	name  string
	isDir bool
}

func respXML(p string, isDir bool) string {
	href := (&url.URL{Path: p}).EscapedPath()
	rt := ""
	if isDir {
		rt = "<d:collection/>"
	}
	return `<d:response><d:href>` + href + `</d:href><d:propstat><d:prop><d:resourcetype>` +
		rt + `</d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
}

func mockWebDAV() *httptest.Server {
	tree := map[string][]child{
		"/movies/": {
			{"The Matrix (1999)/", true},
			{"readme.txt", false},
		},
		"/movies/The Matrix (1999)/": {
			{"The Matrix (1999).mkv", false},
		},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := r.URL.Path
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		kids, ok := tree[p]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var sb strings.Builder
		sb.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
		sb.WriteString(respXML(p, true))
		for _, c := range kids {
			sb.WriteString(respXML(p+c.name, c.isDir))
		}
		sb.WriteString(`</d:multistatus>`)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		w.Write([]byte(sb.String()))
	}))
}

func TestBuildSubfolder(t *testing.T) {
	srv := mockWebDAV()
	defer srv.Close()
	su, _ := url.Parse(srv.URL)
	out := t.TempDir()

	cfg, err := loadConfig([]string{"-root", out, "-webdav-url", srv.URL + "/movies"})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(cfg); err != nil {
		t.Fatal(err)
	}

	host := sanitizeHost(su.Host)
	want := filepath.Join(out, host, "movies", "The Matrix (1999)", "The Matrix (1999).strm")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected .strm at %s: %v", want, err)
	}
	exp := (&url.URL{Scheme: "http", Host: su.Host, Path: "/movies/The Matrix (1999)/The Matrix (1999).mkv"}).String() + "\n"
	if string(got) != exp {
		t.Fatalf("content = %q, want %q", got, exp)
	}
	if _, err := os.Stat(filepath.Join(out, host, "movies", "readme.strm")); !os.IsNotExist(err) {
		t.Fatal("non-media file should not produce a .strm")
	}
}

func indexHTML(kids []child) string {
	var sb strings.Builder
	sb.WriteString(`<html><body><pre>`)
	sb.WriteString(`<a href="?C=N;O=D">Name</a>` + "\n")        // sort link, must be ignored
	sb.WriteString(`<a href="../">Parent Directory</a>` + "\n") // parent link, must be ignored
	for _, c := range kids {
		href := (&url.URL{Path: c.name}).EscapedPath()
		sb.WriteString(`<a href="` + href + `">` + c.name + "</a>\n")
	}
	sb.WriteString(`</pre></body></html>`)
	return sb.String()
}

func mockHTTPIndex() *httptest.Server {
	pages := map[string][]child{
		"/movies/": {
			{"The Matrix (1999)/", true},
			{"readme.txt", false},
		},
		"/movies/The Matrix (1999)/": {
			{"The Matrix (1999).mkv", false},
		},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet { // 405 on PROPFIND drives the probe to the HTML index
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := r.URL.Path
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		kids, ok := pages[p]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(indexHTML(kids)))
	}))
}

func TestBuildHTTPIndex(t *testing.T) {
	srv := mockHTTPIndex()
	defer srv.Close()
	su, _ := url.Parse(srv.URL)
	out := t.TempDir()

	cfg, err := loadConfig([]string{"-root", out, "-webdav-url", srv.URL + "/movies"})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(cfg); err != nil {
		t.Fatal(err) // a stray crawl of ../ or a sort link would 404 and surface here
	}

	host := sanitizeHost(su.Host)
	want := filepath.Join(out, host, "movies", "The Matrix (1999)", "The Matrix (1999).strm")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected .strm at %s: %v", want, err)
	}
	exp := (&url.URL{Scheme: "http", Host: su.Host, Path: "/movies/The Matrix (1999)/The Matrix (1999).mkv"}).String() + "\n"
	if string(got) != exp {
		t.Fatalf("content = %q, want %q", got, exp)
	}
	if _, err := os.Stat(filepath.Join(out, host, "movies", "readme.strm")); !os.IsNotExist(err) {
		t.Fatal("non-media file should not produce a .strm")
	}
}

// TestBuildHTTPIndexLive crawls a real public HTTPS open directory (the Blender
// Foundation's Big Buck Bunny mirror) to exercise the PROPFIND-to-autoindex
// fallback and the default media filter against a real server holding real video
// files. It skips, rather than fails, when the mirror is unreachable, and is
// skipped entirely under -short.
func TestBuildHTTPIndexLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live network test in -short mode")
	}
	const base = "https://download.blender.org/peach/bigbuckbunny_movies"

	client := &http.Client{Timeout: 20 * time.Second}
	if resp, err := client.Get(base + "/"); err != nil {
		t.Skipf("mirror unreachable: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Skipf("mirror returned %s", resp.Status)
		}
	}

	out := t.TempDir()
	cfg, err := loadConfig([]string{"-root", out, base}) // default media filter
	if err != nil {
		t.Fatal(err)
	}
	if err := run(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.sources[0].kind != kindHTTP {
		t.Fatalf("kind = %v, want HTTP autoindex", cfg.sources[0].kind)
	}

	want := filepath.Join(out, "download.blender.org", "peach", "bigbuckbunny_movies", "BigBuckBunny_320x180.strm")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected .strm at %s: %v", want, err)
	}
	if exp := base + "/BigBuckBunny_320x180.mp4\n"; string(got) != exp {
		t.Fatalf("content = %q, want %q", got, exp)
	}
}
