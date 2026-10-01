package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const targetURLvietmixthitoeic = "https://ducnguynx.codes/music"

type bucket struct {
	start time.Time
	count int
}
type app struct {
	dataDir string
	db      *sql.DB
	proxies []netip.Prefix
	mu      sync.Mutex
	clients map[string]bucket
	global  bucket
}

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS song_requests (
 id INTEGER PRIMARY KEY, song_name TEXT NOT NULL CHECK(length(song_name) BETWEEN 1 AND 200),
 ip TEXT NOT NULL, country TEXT NOT NULL, user_agent TEXT NOT NULL, created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS song_requests_ip_time ON song_requests(ip, created_at);
 CREATE INDEX IF NOT EXISTS song_requests_time ON song_requests(created_at);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (a *app) identity(r *http.Request) (string, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown", "Unknown"
	}
	ip = ip.Unmap()
	for _, p := range a.proxies {
		if p.Contains(ip) {
			// Only trust Cloudflare headers from explicitly configured proxy peers.
			forwarded, err := netip.ParseAddr(r.Header.Get("CF-Connecting-IP"))
			if err == nil {
				country := strings.ToUpper(r.Header.Get("CF-IPCountry"))
				if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
					country = "Unknown"
				}
				return forwarded.Unmap().String(), country
			}
		}
	}
	return ip.String(), "Unknown"
}

func (a *app) allow(ip string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Sub(a.global.start) >= time.Minute {
		a.global = bucket{start: now}
	}
	a.global.count++
	if a.global.count > 100 {
		return false
	}
	for key, b := range a.clients {
		if now.Sub(b.start) >= time.Minute {
			delete(a.clients, key)
		}
	}
	b, exists := a.clients[ip]
	if !exists {
		if len(a.clients) >= 10000 {
			return false
		}
		b = bucket{start: now}
	}
	b.count++
	a.clients[ip] = b
	return b.count <= 5
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}

func (a *app) submit(w http.ResponseWriter, r *http.Request) {
	// Let a separately hosted frontend use the API without an origin allowlist.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Retry-After")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		fail(w, 405, "use POST")
		return
	}
	ip, country := a.identity(r)
	now := time.Now().UTC()
	if !a.allow(ip, now) {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "too many requests; try again later")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, 415, "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "request body exceeds 4096 bytes")
		} else {
			fail(w, 400, "cannot read request body")
		}
		return
	}
	if !utf8.Valid(data) {
		fail(w, 400, "body must be valid UTF-8")
		return
	}
	var input struct {
		SongName string `json:"song_name"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		fail(w, 400, "expected a JSON object containing only song_name")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		fail(w, 400, "expected one JSON object")
		return
	}
	// Reject controls before trimming; normalize ordinary Unicode whitespace.
	for _, c := range input.SongName {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || c == utf8.RuneError {
			fail(w, 400, "song_name contains unsupported characters")
			return
		}
	}
	song := strings.Join(strings.Fields(input.SongName), " ")
	if n := utf8.RuneCountInString(song); n < 1 || n > 200 {
		fail(w, 400, "song_name must contain 1 to 200 characters")
		return
	}
	ua := strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, r.UserAgent())
	uaRunes := []rune(ua)
	if len(uaRunes) > 512 {
		ua = string(uaRunes[:512])
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, 503, "storage unavailable")
		return
	}
	defer tx.Rollback()
	// The single database connection serializes this check with the insert.
	// Count accepted submissions across every IP in the rolling hour.
	now = time.Now().UTC()
	var total int
	var oldest sql.NullInt64
	err = tx.QueryRowContext(r.Context(), `SELECT count(*), min(created_at) FROM song_requests WHERE created_at > ?`, now.Add(-time.Hour).Unix()).Scan(&total, &oldest)
	if err != nil {
		fail(w, 503, "storage unavailable")
		return
	}
	if total >= 100 {
		retryAfter := max(int64(1), oldest.Int64+3600-now.Unix())
		w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
		fail(w, 429, "global hourly submission limit reached; try again later")
		return
	}
	var recent, duplicates int
	err = tx.QueryRowContext(r.Context(), `SELECT count(*), coalesce(sum(CASE WHEN song_name = ? AND created_at >= ? THEN 1 ELSE 0 END),0) FROM song_requests WHERE ip = ? AND created_at >= ?`, song, now.Add(-10*time.Minute).Unix(), ip, now.Add(-time.Hour).Unix()).Scan(&recent, &duplicates)
	if err != nil {
		fail(w, 503, "storage unavailable")
		return
	}
	if recent >= 5 {
		w.Header().Set("Retry-After", "3600")
		fail(w, 429, "hourly submission limit reached")
		return
	}
	if duplicates > 0 {
		fail(w, 409, "you recently submitted this song")
		return
	}
	result, err := tx.ExecContext(r.Context(), `INSERT INTO song_requests(song_name,ip,country,user_agent,created_at) VALUES(?,?,?,?,?)`, song, ip, country, ua, now.Unix())
	if err != nil {
		fail(w, 503, "storage unavailable")
		return
	}
	id, err := result.LastInsertId()
	if err != nil {
		fail(w, 503, "storage unavailable")
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, 503, "storage unavailable")
		return
	}
	reply(w, 201, map[string]any{"id": id, "song_name": song, "created_at": now.Format(time.RFC3339)})
}

func (a *app) redirect(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", 405)
		return
	}
	ip, country := a.identity(r)
	line := fmt.Sprintf("[%s] IP: %s | Country: %s | User-Agent: %q\n", time.Now().UTC().Format(time.RFC3339), ip, country, r.UserAgent())
	log.Print(strings.TrimSpace(line))
	f, err := os.OpenFile(filepath.Join(a.dataDir, "qr_access.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err == nil {
		_, err = f.WriteString(line)
		f.Close()
	}
	if err != nil {
		log.Printf("access log: %v", err)
	}
	http.Redirect(w, r, targetURLvietmixthitoeic, http.StatusFound)
}

func main() {
	path := os.Getenv("DATABASE_PATH")
	if path == "" {
		path = "/app/data/music.db"
	}
	db, err := openDB(path)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, dataDir: filepath.Dir(path), clients: map[string]bucket{}}
	for _, cidr := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		if cidr = strings.TrimSpace(cidr); cidr == "" {
			continue
		}
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			log.Fatalf("invalid TRUSTED_PROXY_CIDRS: %v", err)
		}
		a.proxies = append(a.proxies, p)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/song-requests", a.submit)
	mux.HandleFunc("/", a.redirect)
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	log.Print("listening on :8080")
	log.Fatal(server.ListenAndServe())
}
