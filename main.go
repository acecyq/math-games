// Nekoda Forge Space serves the tuition centre's canvas math games
// behind a shared forge code.
//
//	CGO_ENABLED=0 go run .
//
// CGO_ENABLED=0 keeps the build on Go's own linker. This Mac's dyld
// rejects the cgo-linked binary from Go 1.21.
//
// Set NEKODA_ACCESS_CODE to choose the code. If it is unset, the code is
// read from .nekoda-access in the game folder, or created there on first
// launch. PORT defaults to 8080. NEKODA_ROOT overrides the folder that
// contains primary/ and secondary/.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	cookieName  = "nekoda_forge"
	logoName    = "nekoda forge space logo.png"
	accessName  = ".nekoda-access"
	sessionLife = 14 * 24 * time.Hour
	portalCSP   = "default-src 'self'; img-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
)

//go:embed portal.html
var portalFS embed.FS

var pageTmpl = template.Must(template.ParseFS(portalFS, "portal.html"))

var titlePattern = regexp.MustCompile(`(?is)<title[^>]*>\s*(.*?)\s*</title>`)

var errNotGame = errors.New("not a game file")

var pairLabels = map[string]string{
	"area-perimeter":  "Area & Perimeter",
	"angles-polygons": "Angles & Polygons",
	"add-minus":       "Add & Minus",
	"multiply-divide": "Multiply & Divide",
}

type forge struct {
	root      string
	code      string
	hasLogo   bool
	logoPNG   []byte
	failDelay time.Duration
	sessions  sync.Map
	attempts  attemptGate
}

type session struct {
	expires time.Time
	csrf    string
}

type attemptGate struct {
	mu sync.Mutex
	by map[string]*attempt
}

type attempt struct {
	fails int
	until time.Time
}

type pageData struct {
	Authed  bool
	HasLogo bool
	Error   string
	Next    string
	CSRF    string
	Levels  []levelView
	Total   int
}

type levelView struct {
	ID     string
	Name   string
	Index  string
	Count  int
	Topics []topicView
}

type topicView struct {
	Name   string
	Groups []groupView
	Count  int
}

type groupView struct {
	Name  string
	Games []gameView
}

type gameView struct {
	Title  string
	Detail string
	Href   string
	Level  string
	Search string
	Kicker string
}

func main() {
	root, err := findRoot()
	if err != nil {
		log.Fatal(err)
	}
	norm, display, source, err := loadAccess(root)
	if err != nil {
		log.Fatal(err)
	}
	logoPNG, logoErr := prepareLogo(filepath.Join(root, logoName))
	if logoErr != nil && !os.IsNotExist(logoErr) {
		log.Printf("logo: %v", logoErr)
	}
	f := &forge{
		root:      root,
		code:      norm,
		hasLogo:   logoErr == nil,
		logoPNG:   logoPNG,
		failDelay: 300 * time.Millisecond,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", f.home)
	mux.HandleFunc("/login", f.login)
	mux.HandleFunc("/logout", f.logout)
	mux.HandleFunc("/brand/logo.png", f.logo)
	mux.HandleFunc("/favicon.ico", f.logo)
	mux.HandleFunc("/primary/", f.games)
	mux.HandleFunc("/secondary/", f.games)

	addr := listenAddr()
	levels, total, err := buildCatalog(root)
	if err != nil {
		log.Fatal(err)
	}
	_ = levels
	fmt.Printf("Nekoda Forge Space is open\n  %s\n  %d games\n", listenURL(addr), total)
	switch source {
	case "env":
		fmt.Println("  forge code: (from NEKODA_ACCESS_CODE)")
	case "new":
		fmt.Printf("  forge code: %s\n  saved to %s — share it with students\n", display, filepath.Join(root, accessName))
	default:
		fmt.Printf("  forge code: %s\n", display)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           withHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func listenAddr() string {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		return ":8080"
	}
	if strings.Contains(port, ":") {
		return port
	}
	return ":" + port
}

func listenURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr
	}
	return "http://" + addr
}

func findRoot() (string, error) {
	if env := strings.TrimSpace(os.Getenv("NEKODA_ROOT")); env != "" {
		if dirHasGames(env) {
			return filepath.Abs(env)
		}
		return "", fmt.Errorf("NEKODA_ROOT %s does not contain primary/ or secondary/", env)
	}
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	for _, c := range candidates {
		if dirHasGames(c) {
			return filepath.Abs(c)
		}
	}
	return "", errors.New("could not find primary/ and secondary/ — run from the math-games folder or set NEKODA_ROOT")
}

func dirHasGames(dir string) bool {
	for _, name := range []string{"primary", "secondary"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func loadAccess(root string) (norm, display, source string, err error) {
	if env := strings.TrimSpace(os.Getenv("NEKODA_ACCESS_CODE")); env != "" {
		n := normCode(env)
		if n == "" {
			return "", "", "", errors.New("NEKODA_ACCESS_CODE has no letters or digits")
		}
		return n, env, "env", nil
	}
	path := filepath.Join(root, accessName)
	if b, readErr := os.ReadFile(path); readErr == nil {
		display = strings.TrimSpace(string(b))
		n := normCode(display)
		if n != "" {
			return n, display, "file", nil
		}
	}
	display, norm, err = newCode()
	if err != nil {
		return "", "", "", err
	}
	if writeErr := os.WriteFile(path, []byte(display+"\n"), 0o600); writeErr != nil {
		log.Printf("could not save forge code to %s: %v", path, writeErr)
	}
	return norm, display, "new", nil
}

func newCode() (display, norm string, err error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, 8)
	var scratch [1]byte
	for i := 0; i < len(out); {
		if _, err = rand.Read(scratch[:]); err != nil {
			return "", "", err
		}
		if int(scratch[0]) >= 256-(256%len(alphabet)) {
			continue
		}
		out[i] = alphabet[int(scratch[0])%len(alphabet)]
		i++
	}
	return string(out[:4]) + "-" + string(out[4:]), string(out), nil
}

func normCode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (g *attemptGate) allow(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.by[ip]
	if a == nil {
		return true
	}
	if !a.until.IsZero() && time.Now().Before(a.until) {
		return false
	}
	if !a.until.IsZero() {
		a.fails = 0
		a.until = time.Time{}
	}
	return true
}

func (g *attemptGate) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.by == nil {
		g.by = map[string]*attempt{}
	}
	a := g.by[ip]
	if a == nil {
		a = &attempt{}
		g.by[ip] = a
	}
	a.fails++
	if a.fails >= 8 {
		a.until = time.Now().Add(time.Minute)
		a.fails = 0
	}
}

func (g *attemptGate) reset(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.by, ip)
}

func (f *forge) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	nextParam := r.URL.Query().Get("next")
	next := safeNext(nextParam)
	if sess, ok := f.current(r); ok {
		if nextParam != "" && next != "/" {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		levels, total, err := buildCatalog(f.root)
		if err != nil {
			log.Printf("catalog: %v", err)
			http.Error(w, "could not read the game library", http.StatusInternalServerError)
			return
		}
		f.render(w, http.StatusOK, pageData{
			Authed: true,
			CSRF:   sess.csrf,
			Levels: levels,
			Total:  total,
		})
		return
	}
	formNext := ""
	if next != "/" {
		formNext = next
	}
	f.render(w, http.StatusOK, pageData{Next: formNext, Error: loginError(r.URL.Query().Get("e"))})
}

func loginError(code string) string {
	switch code {
	case "code":
		return "That forge code is not recognised."
	case "locked":
		return "Too many tries. Wait a minute, then enter the forge code again."
	default:
		return ""
	}
}

func (f *forge) redirectLoginError(w http.ResponseWriter, r *http.Request, kind, next string) {
	q := url.Values{}
	q.Set("e", kind)
	if next != "" && next != "/" {
		q.Set("next", next)
	}
	http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
}

func (f *forge) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := safeNext(r.FormValue("next"))
	ip := clientIP(r)
	if !f.attempts.allow(ip) {
		f.redirectLoginError(w, r, "locked", next)
		return
	}
	got := normCode(r.FormValue("code"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(f.code)) != 1 {
		if f.failDelay > 0 {
			time.Sleep(f.failDelay)
		}
		f.attempts.fail(ip)
		f.redirectLoginError(w, r, "code", next)
		return
	}
	f.attempts.reset(ip)
	token, err := randomToken(32)
	if err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	csrf, err := randomToken(16)
	if err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	exp := time.Now().Add(sessionLife)
	f.sessions.Store(token, session{expires: exp, csrf: csrf})
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		Expires:  exp,
		MaxAge:   int(sessionLife.Seconds()),
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (f *forge) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if sess, ok := f.current(r); ok {
		given := r.FormValue("csrf")
		if subtle.ConstantTimeCompare([]byte(given), []byte(sess.csrf)) != 1 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if c, err := r.Cookie(cookieName); err == nil {
			f.sessions.Delete(c.Value)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (f *forge) games(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := f.current(r); !ok {
		if wantsPage(r) {
			http.Redirect(w, r, "/?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		http.Error(w, "forge code required", http.StatusUnauthorized)
		return
	}
	full, err := gameFile(f.root, r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeFile(w, r, full)
}

func (f *forge) logo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(f.logoPNG) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, "logo.png", time.Time{}, bytes.NewReader(f.logoPNG))
}

// prepareLogo knocks the flat navy backdrop out of the supplied mark so it
// sits on the workshop background instead of a square tile.
func prepareLogo(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := src.At(x, y).RGBA()
			rr, gg, bb, aa := int(r>>8), int(g>>8), int(b>>8), int(a>>8)
			alpha := int(knockoutAlpha(rr, gg, bb)) * aa / 255
			dst.SetNRGBA(x-bounds.Min.X, y-bounds.Min.Y, color.NRGBA{
				R: uint8(rr), G: uint8(gg), B: uint8(bb), A: uint8(alpha),
			})
		}
	}
	trimmed := trimTransparent(dst, 12)
	var buf bytes.Buffer
	if err := png.Encode(&buf, trimmed); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func trimTransparent(src *image.NRGBA, pad int) *image.NRGBA {
	b := src.Bounds()
	minX, minY := b.Max.X, b.Max.Y
	maxX, maxY := b.Min.X, b.Min.Y
	found := false
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if src.NRGBAAt(x, y).A > 16 {
				found = true
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if !found {
		return src
	}
	minX = max(b.Min.X, minX-pad)
	minY = max(b.Min.Y, minY-pad)
	maxX = min(b.Max.X-1, maxX+pad)
	maxY = min(b.Max.Y-1, maxY+pad)
	out := image.NewNRGBA(image.Rect(0, 0, maxX-minX+1, maxY-minY+1))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			out.SetNRGBA(x-minX, y-minY, src.NRGBAAt(x, y))
		}
	}
	return out
}

func knockoutAlpha(rr, gg, bb int) uint8 {
	dr, dg, db := rr-8, gg-1, bb-0x23
	dist2 := dr*dr + dg*dg + db*db
	const inner = 22 * 22
	const outer = 46 * 46
	if dist2 <= inner {
		return 0
	}
	if dist2 >= outer {
		return 255
	}
	return uint8((dist2 - inner) * 255 / (outer - inner))
}

func (f *forge) render(w http.ResponseWriter, status int, data pageData) {
	data.HasLogo = f.hasLogo
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", portalCSP)
	w.WriteHeader(status)
	if err := pageTmpl.Execute(w, data); err != nil {
		log.Printf("render: %v", err)
	}
}

func (f *forge) current(r *http.Request) (session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return session{}, false
	}
	v, ok := f.sessions.Load(c.Value)
	if !ok {
		return session{}, false
	}
	sess := v.(session)
	if time.Now().After(sess.expires) {
		f.sessions.Delete(c.Value)
		return session{}, false
	}
	return sess, true
}

func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Robots-Tag", "noindex")
		next.ServeHTTP(w, r)
	})
}

func wantsPage(r *http.Request) bool {
	if strings.HasSuffix(strings.ToLower(r.URL.Path), ".html") {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func safeNext(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/"
	}
	if strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") || strings.ContainsAny(raw, "\r\n") {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil {
		return "/"
	}
	p := path.Clean(u.Path)
	if p != "/" && !strings.HasPrefix(p, "/primary/") && !strings.HasPrefix(p, "/secondary/") {
		return "/"
	}
	out := p
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

func gameFile(root, urlPath string) (string, error) {
	if strings.Contains(urlPath, "\\") || strings.ContainsRune(urlPath, 0) {
		return "", errNotGame
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	if !strings.HasPrefix(cleaned, "/primary/") && !strings.HasPrefix(cleaned, "/secondary/") {
		return "", errNotGame
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", errNotGame
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}
	full := filepath.Join(absRoot, filepath.FromSlash(strings.TrimPrefix(cleaned, "/")))
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if !within(absRoot, absFull) {
		return "", errNotGame
	}
	info, err := os.Stat(absFull)
	if err != nil || info.IsDir() {
		return "", errNotGame
	}
	if resolved, err := filepath.EvalSymlinks(absFull); err == nil && !within(absRoot, resolved) {
		return "", errNotGame
	}
	return absFull, nil
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	top := rel
	if i := strings.IndexRune(rel, os.PathSeparator); i >= 0 {
		top = rel[:i]
	}
	return top == "primary" || top == "secondary"
}

func buildCatalog(root string) ([]levelView, int, error) {
	type key struct {
		level string
		topic string
		sub   string
	}
	buckets := map[key][]gameView{}
	total := 0
	for _, level := range []string{"primary", "secondary"} {
		dir := filepath.Join(root, level)
		err := filepath.WalkDir(dir, func(filePath string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) && filePath == dir {
					return nil
				}
				return walkErr
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") {
				if d.IsDir() && filePath != dir {
					return filepath.SkipDir
				}
				if !d.IsDir() {
					return nil
				}
			}
			if d.IsDir() || !strings.EqualFold(filepath.Ext(name), ".html") {
				return nil
			}
			rel, err := filepath.Rel(root, filePath)
			if err != nil {
				return err
			}
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) < 3 {
				return nil
			}
			topic := parts[1]
			sub := ""
			if len(parts) > 4 {
				sub = pathLabel(parts[2 : len(parts)-2])
			}
			rawTitle := readTitle(filePath)
			if rawTitle == "" {
				rawTitle = topicLabel(strings.TrimSuffix(name, filepath.Ext(name)))
			}
			title, detail := splitTitle(rawTitle)
			href := (&url.URL{Path: "/" + filepath.ToSlash(rel)}).String()
			kicker := topicLabel(topic)
			if sub != "" {
				kicker += " · " + sub
			}
			base := strings.TrimSuffix(name, filepath.Ext(name))
			search := strings.ToLower(strings.Join([]string{title, detail, kicker, base}, " "))
			k := key{level: level, topic: topic, sub: sub}
			buckets[k] = append(buckets[k], gameView{
				Title:  title,
				Detail: detail,
				Href:   href,
				Level:  level,
				Search: search,
				Kicker: kicker,
			})
			total++
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}

	var levels []levelView
	index := 0
	for _, id := range []string{"primary", "secondary"} {
		byTopic := map[string]map[string][]gameView{}
		var topicOrder []string
		count := 0
		for k, games := range buckets {
			if k.level != id {
				continue
			}
			if _, ok := byTopic[k.topic]; !ok {
				byTopic[k.topic] = map[string][]gameView{}
				topicOrder = append(topicOrder, k.topic)
			}
			sorted := append([]gameView(nil), games...)
			slices.SortFunc(sorted, func(a, b gameView) int {
				return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
			})
			byTopic[k.topic][k.sub] = sorted
			count += len(games)
		}
		if count == 0 {
			continue
		}
		slices.SortFunc(topicOrder, func(a, b string) int {
			return strings.Compare(topicLabel(a), topicLabel(b))
		})
		topics := make([]topicView, 0, len(topicOrder))
		for _, topic := range topicOrder {
			subs := byTopic[topic]
			names := make([]string, 0, len(subs))
			for name := range subs {
				names = append(names, name)
			}
			slices.SortFunc(names, func(a, b string) int {
				if a == b {
					return 0
				}
				if a == "" {
					return -1
				}
				if b == "" {
					return 1
				}
				return strings.Compare(a, b)
			})
			groups := make([]groupView, 0, len(names))
			tcount := 0
			for _, name := range names {
				groups = append(groups, groupView{Name: name, Games: subs[name]})
				tcount += len(subs[name])
			}
			topics = append(topics, topicView{Name: topicLabel(topic), Groups: groups, Count: tcount})
		}
		index++
		levels = append(levels, levelView{
			ID:     id,
			Name:   topicLabel(id),
			Index:  fmt.Sprintf("%02d", index),
			Count:  count,
			Topics: topics,
		})
	}
	return levels, total, nil
}

func topicLabel(slug string) string {
	if label, ok := pairLabels[slug]; ok {
		return label
	}
	slug = strings.ReplaceAll(slug, "_", " ")
	slug = strings.ReplaceAll(slug, "-", " ")
	parts := strings.Fields(slug)
	for i, part := range parts {
		lower := strings.ToLower(part)
		if lower == "and" {
			parts[i] = "&"
			continue
		}
		if lower == "" {
			continue
		}
		parts[i] = strings.ToUpper(lower[:1]) + lower[1:]
	}
	return strings.Join(parts, " ")
}

func pathLabel(segments []string) string {
	labels := make([]string, 0, len(segments))
	for _, seg := range segments {
		labels = append(labels, topicLabel(seg))
	}
	return strings.Join(labels, " · ")
}

func splitTitle(raw string) (string, string) {
	raw = strings.Join(strings.Fields(raw), " ")
	for _, sep := range []string{" — ", " – ", ": "} {
		if i := strings.Index(raw, sep); i >= 3 {
			return raw[:i], strings.TrimSpace(raw[i+len(sep):])
		}
	}
	return raw, ""
}

func readTitle(filePath string) string {
	f, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 12000)
	n, _ := f.Read(buf)
	m := titlePattern.FindSubmatch(buf[:n])
	if m == nil {
		return ""
	}
	return html.UnescapeString(strings.TrimSpace(string(m[1])))
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	const hexDigits = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, c := range b {
		out[i*2] = hexDigits[c>>4]
		out[i*2+1] = hexDigits[c&0x0f]
	}
	return string(out), nil
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
