package main

import (
	"fmt"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &app{db: db, dataDir: dir, clients: map[string]bucket{}}
}
func post(a *app, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/song-requests", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "test-browser")
	w := httptest.NewRecorder()
	a.submit(w, r)
	return w
}
func TestSubmissionAndSQLSafety(t *testing.T) {
	a := testApp(t)
	song := `L'été'); DROP TABLE song_requests; -- 🎵`
	w := post(a, fmt.Sprintf(`{"song_name":%q}`, song))
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var saved, ip, country, ua string
	if err := a.db.QueryRow(`SELECT song_name,ip,country,user_agent FROM song_requests`).Scan(&saved, &ip, &country, &ua); err != nil {
		t.Fatal(err)
	}
	if saved != song || ip != "192.0.2.1" || country != "Unknown" || ua != "test-browser" {
		t.Fatalf("unexpected row: %q %q %q %q", saved, ip, country, ua)
	}
	if post(a, fmt.Sprintf(`{"song_name":%q}`, song)).Code != 409 {
		t.Fatal("duplicate accepted")
	}
}
func TestInvalidInput(t *testing.T) {
	cases := []struct {
		body   string
		status int
	}{
		{`{"song_name":"   "}`, 400}, {`{"song_name":"hello\nworld"}`, 400},
		{`{"song_name":"a\u0000b"}`, 400}, {`{"song_name":"a\u202eb"}`, 400},
		{`{"song_name":"valid","country":"VN"}`, 400}, {`null`, 400},
		{`{"song_name":12}`, 400}, {`{"song_name":"ok"} {}`, 400},
		{`{"song_name":"` + strings.Repeat("界", 201) + `"}`, 400},
		{`{"song_name":"` + strings.Repeat("x", 5000) + `"}`, 413},
		{"{\"song_name\":\"\xff\"}", 400},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(len(tc.body), tc.body[:min(len(tc.body), 25)]), func(t *testing.T) {
			a := testApp(t)
			w := post(a, tc.body)
			if w.Code != tc.status {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			var count int
			a.db.QueryRow(`SELECT count(*) FROM song_requests`).Scan(&count)
			if count != 0 {
				t.Fatal("invalid row saved")
			}
		})
	}
}
func TestNormalization(t *testing.T) {
	a := testApp(t)
	w := post(a, `{"song_name":"  Một   bài hát 🎵  "}`)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var song string
	a.db.QueryRow(`SELECT song_name FROM song_requests`).Scan(&song)
	if song != "Một bài hát 🎵" {
		t.Fatal(song)
	}
}
func TestCORSAndMethods(t *testing.T) {
	for _, tc := range []struct {
		method, origin, media string
		status                int
	}{
		{"POST", "https://another-frontend.example", "application/json", 201},
		{"OPTIONS", "https://frontend.example", "", 204},
		{"GET", "", "", 405}, {"POST", "", "text/plain", 415},
		{"POST", "https://frontend.example", "application/json", 201},
	} {
		a := testApp(t)
		r := httptest.NewRequest(tc.method, "/api/song-requests", strings.NewReader(`{"song_name":"test"}`))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Content-Type", tc.media)
		w := httptest.NewRecorder()
		a.submit(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d", tc, w.Code)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatal("missing CORS header")
		}
		if w.Header().Get("Access-Control-Expose-Headers") != "Retry-After" {
			t.Fatal("frontend cannot read Retry-After")
		}
	}
}
func TestProxyTrust(t *testing.T) {
	a := testApp(t)
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("CF-Connecting-IP", "203.0.113.5")
	r.Header.Set("CF-IPCountry", "VN")
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	ip, country := a.identity(r)
	if ip != "192.0.2.1" || country != "Unknown" {
		t.Fatal(ip, country)
	}
	a.proxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	ip, country = a.identity(r)
	if ip != "203.0.113.5" || country != "VN" {
		t.Fatal(ip, country)
	}
	r.Header.Set("CF-Connecting-IP", "bad")
	ip, country = a.identity(r)
	if ip != "192.0.2.1" || country != "Unknown" {
		t.Fatal(ip, country)
	}
}
func TestPersistentHourlyLimit(t *testing.T) {
	a := testApp(t)
	for i := 0; i < 5; i++ {
		if post(a, fmt.Sprintf(`{"song_name":"song %d"}`, i)).Code != 201 {
			t.Fatal("submission failed")
		}
	}
	// Reopen the persisted database with a new application instance.
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(filepath.Join(a.dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := &app{db: db, clients: map[string]bucket{}}
	if w := post(b, `{"song_name":"sixth"}`); w.Code != 429 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestAttemptLimits(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	for i := 0; i < 5; i++ {
		if !a.allow("ip", now) {
			t.Fatal("limited too early")
		}
	}
	if a.allow("ip", now) {
		t.Fatal("sixth attempt allowed")
	}
	if !a.allow("ip", now.Add(time.Minute)) {
		t.Fatal("window did not reset")
	}
	b := testApp(t)
	for i := 0; i < 100; i++ {
		if !b.allow(fmt.Sprint(i), now) {
			t.Fatal("global limit too early")
		}
	}
	if b.allow("another", now) {
		t.Fatal("global limit missing")
	}
}
func TestConcurrentDuplicate(t *testing.T) {
	a := testApp(t)
	var wg sync.WaitGroup
	codes := make(chan int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- post(a, `{"song_name":"same"}`).Code }()
	}
	wg.Wait()
	close(codes)
	created := 0
	for code := range codes {
		if code == 201 {
			created++
		} else if code != 409 {
			t.Fatal(code)
		}
	}
	if created != 1 {
		t.Fatal("created", created)
	}
}
func TestRedirect(t *testing.T) {
	a := testApp(t)
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	a.redirect(w, r)
	if w.Code != 302 || w.Header().Get("Location") != targetURLvietmixthitoeic {
		t.Fatal(w.Code)
	}
}

func seedSubmissions(t *testing.T, a *app, count int, timestamp int64) {
	t.Helper()
	tx, err := a.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < count; i++ {
		if _, err := tx.Exec(`INSERT INTO song_requests(song_name,ip,country,user_agent,created_at) VALUES(?,?,?,?,?)`, fmt.Sprintf("seed %d", i), fmt.Sprintf("seed-ip-%d", i), "Unknown", "test", timestamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalHourlyLimitPersistsAndExpires(t *testing.T) {
	a := testApp(t)
	seedSubmissions(t, a, 100, time.Now().Add(-time.Minute).Unix())
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(filepath.Join(a.dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := &app{db: db, clients: map[string]bucket{}}
	w := post(b, `{"song_name":"new song"}`)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "global hourly") || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %s %v", w.Code, w.Body.String(), w.Header())
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM song_requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 100 {
		t.Fatalf("rejected submission inserted: %d", count)
	}
	if _, err := db.Exec(`UPDATE song_requests SET created_at = ?`, time.Now().Add(-time.Hour-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if w := post(b, `{"song_name":"new song"}`); w.Code != 201 {
		t.Fatalf("expired quota did not reopen: %d %s", w.Code, w.Body.String())
	}
}

func TestConcurrentGlobalHourlyLimit(t *testing.T) {
	a := testApp(t)
	seedSubmissions(t, a, 99, time.Now().Unix())
	var wg sync.WaitGroup
	codes := make(chan int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest("POST", "/api/song-requests", strings.NewReader(fmt.Sprintf(`{"song_name":"song %d"}`, i)))
			r.RemoteAddr = fmt.Sprintf("203.0.113.%d:1234", i+1)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			a.submit(w, r)
			codes <- w.Code
		}(i)
	}
	wg.Wait()
	close(codes)
	created := 0
	for code := range codes {
		if code == 201 {
			created++
		} else if code != 429 {
			t.Fatalf("unexpected status: %d", code)
		}
	}
	if created != 1 {
		t.Fatalf("accepted %d submissions with one slot left", created)
	}
	var count int
	if err := a.db.QueryRow(`SELECT count(*) FROM song_requests`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 100 {
		t.Fatalf("global quota exceeded: %d", count)
	}
}
