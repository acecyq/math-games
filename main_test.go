package main

import (
	"bytes"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormCodeIgnoresCaseAndSeparators(t *testing.T) {
	if normCode("k7np-4qhm") != normCode("K7NP 4QHM") {
		t.Fatal("codes should match after normalising")
	}
	if normCode("---") != "" {
		t.Fatal("expected empty code")
	}
}

func TestSafeNextStaysInsideTheWorkshop(t *testing.T) {
	ok := "/primary/algebra/demo/demo.html"
	if got := safeNext(ok); got != ok {
		t.Fatalf("safe path: got %q", got)
	}
	for _, bad := range []string{
		"https://evil.example/primary/x.html",
		"//evil.example",
		`/\evil.example`,
		"/logout",
		"/login",
		"primary/algebra/demo.html",
	} {
		if got := safeNext(bad); got != "/" {
			t.Fatalf("safeNext(%q) = %q, want /", bad, got)
		}
	}
	withQuery := "/secondary/linear-graphs/slope-striker/slope-striker.html?x=1"
	if got := safeNext(withQuery); got != withQuery {
		t.Fatalf("query: got %q", got)
	}
}

func TestGameFileRejectsEscape(t *testing.T) {
	root := t.TempDir()
	game := filepath.Join(root, "primary", "algebra", "demo", "demo.html")
	if err := os.MkdirAll(filepath.Dir(game), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(game, []byte("<title>Demo</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := gameFile(root, "/primary/algebra/demo/demo.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "demo.html") {
		t.Fatalf("got %s", got)
	}
	for _, p := range []string{
		"/primary/../../secret.txt",
		"/primary/algebra/demo/../../../../secret.txt",
		"/primary/.hidden/demo.html",
		"/secret.txt",
		"/main.go",
	} {
		if _, err := gameFile(root, p); err == nil {
			t.Fatalf("allowed %s", p)
		}
	}
}

func TestCatalogReadsRealLibrary(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	levels, total, err := buildCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	htmlCount := 0
	for _, level := range []string{"primary", "secondary"} {
		_ = filepath.WalkDir(filepath.Join(root, level), func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".html") && !strings.HasPrefix(d.Name(), ".") {
				htmlCount++
			}
			return nil
		})
	}
	if total != htmlCount {
		t.Fatalf("catalog has %d games, found %d html files", total, htmlCount)
	}
	if len(levels) != 2 {
		t.Fatalf("levels: %+v", levels)
	}
	var sawAlchemy, sawSub bool
	for _, level := range levels {
		for _, topic := range level.Topics {
			if topic.Name == "Area & Perimeter" || topic.Name == "Factors & Multiples" || topic.Name == "Angles & Polygons" {
				sawSub = true
			}
			for _, group := range topic.Groups {
				for _, game := range group.Games {
					if game.Title == "Alchemy Balance" && game.Detail == "Potion Symbol Foundry" {
						sawAlchemy = true
					}
					if strings.Contains(game.Href, "..") || !strings.HasPrefix(game.Href, "/"+level.ID+"/") {
						t.Fatalf("bad href %s", game.Href)
					}
				}
			}
		}
	}
	if !sawAlchemy || !sawSub {
		t.Fatal("expected titled games and joined topic names")
	}
}

func TestWorkshopRequiresForgeCode(t *testing.T) {
	root := t.TempDir()
	gameDir := filepath.Join(root, "primary", "algebra", "demo")
	if err := os.MkdirAll(gameDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "<title>Demo Game: Test Lab</title>"
	if err := os.WriteFile(filepath.Join(gameDir, "demo.html"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &forge{root: root, code: normCode("test-code")}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.home)
	mux.HandleFunc("/login", f.login)
	mux.HandleFunc("/logout", f.logout)
	mux.HandleFunc("/primary/", f.games)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login page status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Demo Game") {
		t.Fatal("login page listed a game")
	}
	if !strings.Contains(rec.Body.String(), "Forge code") {
		t.Fatal("login page missing the code field")
	}

	req = httptest.NewRequest(http.MethodGet, "/primary/algebra/demo/demo.html", nil)
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("game redirect status %d", rec.Code)
	}

	form := url.Values{"code": {"nope"}, "next": {"https://evil.example"}}
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.8:1234"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "e=code") {
		t.Fatalf("bad code redirect %d %s", rec.Code, rec.Header().Get("Location"))
	}
	req = httptest.NewRequest(http.MethodGet, rec.Header().Get("Location"), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "not recognised") || strings.Contains(rec.Body.String(), "Demo Game") {
		t.Fatal("error page should explain the code without listing games")
	}

	form.Set("code", "TEST code")
	form.Set("next", "https://evil.example/phish")
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.8:1234"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("open redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			session = c
		}
	}
	if session == nil || !session.HttpOnly {
		t.Fatal("missing http-only session cookie")
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	page := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(page, "Demo Game") || !strings.Contains(page, "Test Lab") {
		t.Fatalf("portal missing game: %d %s", rec.Code, page)
	}
	csrf := csrfValue(t, page)

	req = httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader("csrf=nope"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad csrf status %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/primary/algebra/demo/demo.html", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("game status %d", rec.Code)
	}
	got, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(got), "Demo Game") {
		t.Fatalf("game body %s", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader("csrf="+url.QueryEscape(csrf)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout status %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/primary/algebra/demo/demo.html", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("session still valid after logout: %d", rec.Code)
	}
}

func TestLogoBackdropIsRemoved(t *testing.T) {
	pngBytes, err := prepareLogo("nekoda forge space logo.png")
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	bounds := im.Bounds()
	if bounds.Dx() >= 500 || bounds.Dy() >= 500 {
		t.Fatalf("logo was not trimmed: %dx%d", bounds.Dx(), bounds.Dy())
	}
	opaque := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := im.At(x, y).RGBA()
			if a>>8 > 200 {
				opaque++
			}
		}
	}
	if opaque < 1000 {
		t.Fatalf("opaque pixels %d", opaque)
	}
}

func csrfValue(t *testing.T, page string) string {
	t.Helper()
	const needle = `name="csrf" value="`
	i := strings.Index(page, needle)
	if i < 0 {
		t.Fatal("csrf field missing")
	}
	rest := page[i+len(needle):]
	j := strings.Index(rest, `"`)
	if j <= 0 {
		t.Fatal("csrf value missing")
	}
	return rest[:j]
}
