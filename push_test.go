package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSummarize(t *testing.T) {
	names := []string{"trulex krpe", "Mila jogurt", "bio banane", "bio borovnice", "bio jaja"}
	if got, want := summarize(names, 30), "trulex krpe, Mila jogurt +3 more"; got != want {
		t.Errorf("summarize = %q, want %q", got, want)
	}
	if got := summarize(names[:2], 140); got != "trulex krpe, Mila jogurt" {
		t.Errorf("summarize short list = %q", got)
	}
	if got := shoppingTitle("Vera", 1); got != "Vera: 1 item to buy" {
		t.Errorf("shoppingTitle = %q", got)
	}
}

func TestAllowedPushEndpoint(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/abc":           true,
		"https://web.push.apple.com/QGx":                    true,
		"https://updates.push.services.mozilla.com/wpush/2": true,
		"http://fcm.googleapis.com/fcm/send/abc":            false, // not https
		"https://127.0.0.1:8080/login":                      false,
		"https://evilpush.apple.com.example.com/x":          false,
		"https://notpush.apple.com/x":                       false,
	} {
		if got := allowedPushEndpoint(endpoint); got != want {
			t.Errorf("allowedPushEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

func TestNotifyShopping(t *testing.T) {
	// A fake push service that records what it receives.
	var mu sync.Mutex
	var got []*http.Request
	var bodies [][]byte
	status := http.StatusCreated
	push := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, b)
		code := status
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer push.Close()
	pushClient = push.Client()
	allowedPushEndpoint = func(e string) bool { return strings.HasPrefix(e, push.URL) }

	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := newApp(db, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO families (id, name, invite_code) VALUES (1, 'A', 'AAAA'), (2, 'B', 'BBBB')`)
	mustExec(`INSERT INTO users (id, email, name, password_hash, family_id) VALUES
		(1, 'milos@x', 'Miloš', '', 1), (2, 'vera@x', 'Vera', '', 1), (3, 'eve@x', 'Eve', '', 2)`)
	mustExec(`INSERT INTO shopping_items (family_id, name) VALUES (1, 'Mila jogurt'), (1, 'bio banane')`)
	milos, vera, eve := &User{ID: 1, Name: "Miloš", FamilyID: 1}, &User{ID: 2, Name: "Vera", FamilyID: 1}, &User{ID: 3, Name: "Eve", FamilyID: 2}

	// Miloš's phone subscribes, with real browser-style keys.
	key, _ := ecdh.P256().GenerateKey(rand.Reader)
	secret := make([]byte, 16)
	rand.Read(secret)
	enc := base64.RawURLEncoding.EncodeToString
	sub := `{"endpoint":"` + push.URL + `/device1","keys":{"p256dh":"` + enc(key.PublicKey().Bytes()) + `","auth":"` + enc(secret) + `"}}`
	w := httptest.NewRecorder()
	app.pushSubscribe(w, httptest.NewRequest("POST", "/push/subscribe", strings.NewReader(sub)), milos)
	if w.Code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}

	// A subscription pointing anywhere else is refused.
	w = httptest.NewRecorder()
	bad := strings.Replace(sub, push.URL, "https://127.0.0.1:9", 1)
	app.pushSubscribe(w, httptest.NewRequest("POST", "/push/subscribe", strings.NewReader(bad)), milos)
	if w.Code != http.StatusBadRequest {
		t.Errorf("subscribe to other host: got %d, want 400", w.Code)
	}

	// Vera sees a "Notify Miloš" button.
	targets, err := app.notifyTargets(vera)
	if err != nil || len(targets) != 1 || targets[0].Name != "Miloš" {
		t.Fatalf("notifyTargets = %v, %v", targets, err)
	}

	notify := func(u *User, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/push/notify/"+target, nil)
		r.Host = "family.example.com"
		r.SetPathValue("user", target)
		w := httptest.NewRecorder()
		app.notifyShopping(w, r, u)
		return w
	}

	w = notify(vera, "1")
	if loc := w.Header().Get("Location"); loc != "/shopping?notified=1&sent=1" {
		t.Fatalf("notify redirect = %q (%d %s)", loc, w.Code, w.Body)
	}
	if len(got) != 1 {
		t.Fatalf("push service got %d requests, want 1", len(got))
	}
	req := got[0]
	for h, want := range map[string]string{"Content-Encoding": "aes128gcm", "Urgency": "high", "Topic": "shopping", "TTL": "43200"} {
		if v := req.Header.Get(h); v != want {
			t.Errorf("header %s = %q, want %q", h, v, want)
		}
	}
	if a := req.Header.Get("Authorization"); !strings.HasPrefix(a, "vapid t=") || !strings.Contains(a, "k="+app.vapidPublic) {
		t.Errorf("Authorization = %q", a)
	}
	if len(bodies[0]) < 100 {
		t.Errorf("encrypted body is only %d bytes", len(bodies[0]))
	}

	// Another family can't notify Miloš.
	if w := notify(eve, "1"); w.Code != http.StatusNotFound {
		t.Errorf("other family notify: got %d, want 404", w.Code)
	}

	// The push service says the phone is gone: the subscription is removed.
	mu.Lock()
	status = http.StatusGone
	mu.Unlock()
	if loc := notify(vera, "1").Header().Get("Location"); loc != "/shopping?notified=1&sent=0" {
		t.Errorf("notify to gone device redirect = %q", loc)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM push_subscriptions`).Scan(&n)
	if n != 0 {
		t.Errorf("gone subscription still stored")
	}
	if targets, _ := app.notifyTargets(vera); len(targets) != 0 {
		t.Errorf("Miloš still listed as notify target after his device was gone")
	}
}
