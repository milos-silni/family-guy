package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Web Push: family members turn notifications on per device (Family tab),
// then others can ping them from the shopping list.

var pushClient = &http.Client{Timeout: 10 * time.Second}

// Push services we deliver to. Subscriptions come from browsers, but the
// endpoint is still user input, so the server only ever POSTs to these hosts.
var pushHosts = []string{
	"fcm.googleapis.com",        // Chrome, Edge, Android
	"push.apple.com",            // Safari, iPhone
	"push.services.mozilla.com", // Firefox
	"notify.windows.com",        // Edge on Windows (legacy)
	"android.googleapis.com",    // old Chrome
}

// allowedPushEndpoint is a variable so tests can point it at a local server.
var allowedPushEndpoint = func(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	for _, h := range pushHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// loadVAPIDKeys returns the server's push key pair, creating it on first run.
func loadVAPIDKeys(db *sql.DB) (public, private string, err error) {
	err = db.QueryRow(`SELECT
		(SELECT value FROM settings WHERE key = 'vapid_public'),
		(SELECT value FROM settings WHERE key = 'vapid_private')`).Scan(&public, &private)
	if err == nil {
		return public, private, nil
	}
	private, public, err = webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", err
	}
	_, err = db.Exec(`INSERT INTO settings (key, value) VALUES ('vapid_public', ?), ('vapid_private', ?)`, public, private)
	return public, private, err
}

func (a *App) pushKey(w http.ResponseWriter, r *http.Request, u *User) {
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, a.vapidPublic)
}

func (a *App) pushSubscribe(w http.ResponseWriter, r *http.Request, u *User) {
	var sub webpush.Subscription
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&sub); err != nil {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return
	}
	if !allowedPushEndpoint(sub.Endpoint) || len(sub.Endpoint) > 1000 ||
		sub.Keys.P256dh == "" || sub.Keys.Auth == "" || len(sub.Keys.P256dh) > 200 || len(sub.Keys.Auth) > 100 {
		http.Error(w, "unsupported push service", http.StatusBadRequest)
		return
	}
	// A device belongs to whoever subscribed last on it.
	if _, err := a.db.Exec(`
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth) VALUES (?, ?, ?, ?)
		ON CONFLICT (endpoint) DO UPDATE SET user_id = excluded.user_id, p256dh = excluded.p256dh, auth = excluded.auth`,
		u.ID, sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth); err != nil {
		a.serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) pushUnsubscribe(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if _, err := a.db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ? AND user_id = ?`, body.Endpoint, u.ID); err != nil {
		a.serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hasPushSQL is true when user u has notifications on in a browser or the Android app.
const hasPushSQL = `(EXISTS (SELECT 1 FROM push_subscriptions p WHERE p.user_id = u.id)
	OR EXISTS (SELECT 1 FROM fcm_tokens f WHERE f.user_id = u.id))`

type NotifyTarget struct {
	ID   int64
	Name string
}

// notifyTargets lists family members (other than u) with notifications on.
func (a *App) notifyTargets(u *User) ([]NotifyTarget, error) {
	rows, err := a.db.Query(`
		SELECT u.id, u.name FROM users u
		WHERE u.family_id = ? AND u.id != ? AND `+hasPushSQL+`
		ORDER BY u.id`, u.FamilyID, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []NotifyTarget
	for rows.Next() {
		var t NotifyTarget
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

// notifyShopping sends the current shopping list to one family member's devices.
func (a *App) notifyShopping(w http.ResponseWriter, r *http.Request, u *User) {
	targetID, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sent, err := a.sendShoppingList(u, targetID, "https://"+r.Host)
	if errors.Is(err, errNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/shopping?notified=%d&sent=%d", targetID, sent), http.StatusSeeOther)
}

var errNotFound = errors.New("not found")

// sendShoppingList pushes the open shopping list to a member of u's family and
// returns how many devices accepted it. errNotFound: no such member.
func (a *App) sendShoppingList(u *User, targetID int64, subscriber string) (int, error) {
	var targetFamily int64
	err := a.db.QueryRow(`SELECT family_id FROM users WHERE id = ?`, targetID).Scan(&targetFamily)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && targetFamily != u.FamilyID) {
		return 0, errNotFound
	} else if err != nil {
		return 0, err
	}
	names, err := a.openShoppingNames(u.FamilyID)
	if err != nil {
		return 0, err
	}
	return a.sendPush(targetID, pushMessage{
		Title: shoppingTitle(u.Name, len(names)),
		Body:  summarize(names, 140),
		URL:   "/shopping",
	}, subscriber)
}

type pushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

// sendPush delivers msg to every device of a user (browsers and Android app)
// and returns how many accepted it. Devices the push service says are gone are forgotten.
func (a *App) sendPush(userID int64, msg pushMessage, subscriber string) (int, error) {
	payload, _ := json.Marshal(msg)
	rows, err := a.db.Query(`SELECT id, endpoint, p256dh, auth FROM push_subscriptions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	type device struct {
		id  int64
		sub webpush.Subscription
	}
	var devices []device
	for rows.Next() {
		var d device
		if err := rows.Scan(&d.id, &d.sub.Endpoint, &d.sub.Keys.P256dh, &d.sub.Keys.Auth); err != nil {
			rows.Close()
			return 0, err
		}
		devices = append(devices, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, d := range devices {
		if !allowedPushEndpoint(d.sub.Endpoint) {
			continue
		}
		resp, err := webpush.SendNotification(payload, &d.sub, &webpush.Options{
			HTTPClient:      pushClient,
			Subscriber:      subscriber,
			VAPIDPublicKey:  a.vapidPublic,
			VAPIDPrivateKey: a.vapidPrivate,
			TTL:             12 * 60 * 60,
			Urgency:         webpush.UrgencyHigh,
			Topic:           "shopping", // a newer ping replaces an undelivered older one
		})
		if err != nil {
			log.Printf("push: %v", err)
			continue
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			a.db.Exec(`DELETE FROM push_subscriptions WHERE id = ?`, d.id)
		case resp.StatusCode >= 300:
			log.Printf("push: %s answered %s", d.sub.Endpoint[:min(len(d.sub.Endpoint), 40)], resp.Status)
		default:
			sent++
		}
	}

	n, err := a.sendFCM(userID, msg)
	return sent + n, err
}

func (a *App) openShoppingNames(familyID int64) ([]string, error) {
	rows, err := a.db.Query(`SELECT name FROM shopping_items WHERE family_id = ? AND done = 0 AND not_found = 0 ORDER BY id`, familyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func shoppingTitle(sender string, n int) string {
	switch n {
	case 0:
		return sender + ": the shopping list is empty"
	case 1:
		return sender + ": 1 item to buy"
	default:
		return fmt.Sprintf("%s: %d items to buy", sender, n)
	}
}

// summarize joins names until about max characters, then adds "+N more".
func summarize(names []string, max int) string {
	var b strings.Builder
	for i, n := range names {
		if i > 0 && b.Len()+len(n) > max {
			fmt.Fprintf(&b, " +%d more", len(names)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(n)
	}
	return b.String()
}

// Served at the site root so the service worker may cover the whole app.
func serveServiceWorker(w http.ResponseWriter, r *http.Request) {
	b, _ := staticFS.ReadFile("static/sw.js")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}
