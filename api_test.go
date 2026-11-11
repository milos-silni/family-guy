package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type apiClient struct {
	t     *testing.T
	base  string
	token string
}

// do sends a request (body is JSON-encoded unless it is an io.Reader) and
// decodes the JSON answer into out when given.
func (c *apiClient) do(method, path string, body any, wantStatus int, out any) {
	c.t.Helper()
	var r io.Reader
	ctype := "application/json"
	switch b := body.(type) {
	case nil:
	case multipartBody:
		r, ctype = b.body, b.ctype
	default:
		j, _ := json.Marshal(b)
		r = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("Content-Type", ctype)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, wantStatus, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

type multipartBody struct {
	body  io.Reader
	ctype string
}

func multipartFile(field, name string, content []byte) multipartBody {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile(field, name)
	fw.Write(content)
	mw.Close()
	return multipartBody{&buf, mw.FormDataContentType()}
}

func newAPITestServer(t *testing.T) (*App, string) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := newApp(db, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)
	return app, srv.URL
}

func TestAPI(t *testing.T) {
	app, base := newAPITestServer(t)
	anon := &apiClient{t: t, base: base}

	// Not logged in.
	anon.do("GET", "/api/v1/shopping", nil, 401, nil)
	(&apiClient{t: t, base: base, token: "nope"}).do("GET", "/api/v1/me", nil, 401, nil)
	anon.do("GET", "/api/v1/nothing-here", nil, 404, nil)

	// The first account creates the family; the second needs its invite code.
	var milosSession apiSession
	anon.do("POST", "/api/v1/auth/register", map[string]string{"email": "Milos@Example.com", "name": "Miloš", "password": "secret123"}, 201, &milosSession)
	if milosSession.Token == "" || milosSession.User.Email != "milos@example.com" {
		t.Fatalf("register: %+v", milosSession)
	}
	milos := &apiClient{t: t, base: base, token: milosSession.Token}

	var errResp struct{ Error string }
	anon.do("POST", "/api/v1/auth/register", map[string]string{"email": "vera@example.com", "name": "Vera", "password": "secret123"}, 422, &errResp)
	if !strings.Contains(errResp.Error, "invite code") {
		t.Errorf("register without invite: %q", errResp.Error)
	}

	var family struct {
		InviteCode string `json:"invite_code"`
		Members    []struct {
			ID   int64
			Name string
			IsMe bool `json:"is_me"`
		}
	}
	milos.do("GET", "/api/v1/family", nil, 200, &family)
	if family.InviteCode == "" || len(family.Members) != 1 || !family.Members[0].IsMe {
		t.Fatalf("family: %+v", family)
	}
	anon.do("POST", "/api/v1/auth/register", map[string]string{"email": "vera@example.com", "name": "Vera", "password": "secret123", "invite_code": strings.ToLower(family.InviteCode)}, 201, nil)

	anon.do("POST", "/api/v1/auth/login", map[string]string{"email": "vera@example.com", "password": "wrong-pass"}, 401, nil)
	var veraSession apiSession
	anon.do("POST", "/api/v1/auth/login", map[string]string{"email": "VERA@example.com", "password": "secret123"}, 200, &veraSession)
	vera := &apiClient{t: t, base: base, token: veraSession.Token}

	// Shopping list, shared by the family.
	var item apiShoppingItem
	milos.do("POST", "/api/v1/shopping/items", map[string]string{"name": " mleko ", "quantity": "2L"}, 201, &item)
	if item.Name != "mleko" || item.Quantity != "2L" || item.AddedBy != "Miloš" || item.PhotoURL != nil {
		t.Fatalf("added item: %+v", item)
	}
	milos.do("POST", "/api/v1/shopping/items", map[string]string{"name": ""}, 422, nil)
	vera.do("POST", "/api/v1/shopping/items", map[string]string{"name": "hleb"}, 201, nil)

	var updated apiShoppingItem
	vera.do("PATCH", "/api/v1/shopping/items/"+itoa(item.ID), map[string]any{"not_found": true}, 200, &updated)
	if !updated.NotFound || updated.Bought || updated.Quantity != "2L" {
		t.Errorf("not_found: %+v", updated)
	}
	vera.do("PATCH", "/api/v1/shopping/items/"+itoa(item.ID), map[string]any{"bought": true, "quantity": "3L"}, 200, &updated)
	if !updated.Bought || updated.NotFound || updated.Quantity != "3L" || updated.Name != "mleko" {
		t.Errorf("bought: %+v", updated)
	}
	vera.do("PATCH", "/api/v1/shopping/items/999", map[string]any{"bought": true}, 404, nil)

	// A photo: a tiny PNG header is enough for the type check.
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	milos.do("PUT", "/api/v1/shopping/items/"+itoa(item.ID)+"/photo", multipartFile("photo", "p.png", png), 200, &updated)
	if updated.PhotoURL == nil {
		t.Fatal("photo_url missing after upload")
	}
	milos.do("PUT", "/api/v1/shopping/items/"+itoa(item.ID)+"/photo", multipartFile("photo", "p.png", []byte("not an image")), 422, nil)
	req, _ := http.NewRequest("GET", base+*updated.PhotoURL, nil)
	req.Header.Set("Authorization", "Bearer "+vera.token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("photo download: %v %v", resp, err)
	}

	var list struct {
		Items       []apiShoppingItem
		OpenCount   int `json:"open_count"`
		BoughtCount int `json:"bought_count"`
		NotFoundCnt int `json:"not_found_count"`
	}
	milos.do("GET", "/api/v1/shopping", nil, 200, &list)
	if len(list.Items) != 2 || list.OpenCount != 1 || list.BoughtCount != 1 {
		t.Fatalf("list: %+v", list)
	}

	var finished struct{ Bought int }
	milos.do("POST", "/api/v1/shopping/finish", nil, 200, &finished)
	if finished.Bought != 1 {
		t.Errorf("finish: %+v", finished)
	}
	var history struct {
		Trips []struct {
			FinishedBy string `json:"finished_by"`
			Bought     []struct {
				ID       int64
				Name     string
				PhotoURL *string `json:"photo_url"`
				OnList   bool    `json:"on_list"`
			}
		}
	}
	milos.do("GET", "/api/v1/shopping/history", nil, 200, &history)
	if len(history.Trips) != 1 || len(history.Trips[0].Bought) != 1 || history.Trips[0].Bought[0].Name != "mleko" ||
		history.Trips[0].Bought[0].PhotoURL == nil || history.Trips[0].FinishedBy != "Miloš" {
		t.Fatalf("history: %+v", history)
	}
	var added struct{ Added int }
	milos.do("POST", "/api/v1/shopping/history/items/"+itoa(history.Trips[0].Bought[0].ID)+"/add-to-list", nil, 200, &added)
	milos.do("POST", "/api/v1/shopping/history/items/"+itoa(history.Trips[0].Bought[0].ID)+"/add-to-list", nil, 200, &added)
	if added.Added != 0 {
		t.Errorf("adding an item already on the list again: added %d", added.Added)
	}

	// Favorites.
	var favs struct {
		Favorites []struct {
			ID       int64
			Name     string
			Quantity string
			OnList   bool `json:"on_list"`
		}
	}
	milos.do("POST", "/api/v1/shopping/favorites", map[string]any{"text": "- jaja 10 kom\nhleb", "items": []map[string]string{{"name": "sir"}}}, 200, &favs)
	if len(favs.Favorites) != 3 || favs.Favorites[0].Quantity != "10 kom" || !favs.Favorites[1].OnList {
		t.Fatalf("favorites: %+v", favs)
	}
	milos.do("POST", "/api/v1/shopping/favorites/add-all-to-list", nil, 200, &added)
	if added.Added != 2 { // hleb is already on the list
		t.Errorf("add all favorites: added %d, want 2", added.Added)
	}
	milos.do("DELETE", "/api/v1/shopping/favorites/"+itoa(favs.Favorites[0].ID), nil, 204, nil)

	// To-dos.
	var todo apiTodo
	vera.do("POST", "/api/v1/todos", map[string]string{"title": "Call the plumber"}, 201, &todo)
	vera.do("PATCH", "/api/v1/todos/"+itoa(todo.ID), map[string]any{"done": true}, 200, &todo)
	if !todo.Done || todo.Title != "Call the plumber" {
		t.Errorf("todo: %+v", todo)
	}
	var cleared struct{ Deleted int }
	milos.do("POST", "/api/v1/todos/clear-done", nil, 200, &cleared)
	if cleared.Deleted != 1 {
		t.Errorf("clear done: %+v", cleared)
	}

	// Documents.
	var uploaded struct{ Documents []apiDocument }
	milos.do("POST", "/api/v1/documents", multipartFile("file", "Report.PDF", []byte("%PDF-1.4 hello")), 201, &uploaded)
	if len(uploaded.Documents) != 1 || uploaded.Documents[0].Name != "Report.pdf" || uploaded.Documents[0].Kind != "PDF" {
		t.Fatalf("upload: %+v", uploaded)
	}
	milos.do("POST", "/api/v1/documents", multipartFile("file", "fake.pdf", []byte("hello")), 422, nil)
	doc := uploaded.Documents[0]
	var renamed apiDocument
	vera.do("PATCH", "/api/v1/documents/"+itoa(doc.ID), map[string]string{"name": "Tax 2026"}, 200, &renamed)
	if renamed.Name != "Tax 2026.pdf" {
		t.Errorf("rename: %q", renamed.Name)
	}
	req, _ = http.NewRequest("GET", base+doc.DownloadURL, nil)
	req.Header.Set("Authorization", "Bearer "+vera.token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Errorf("download: %v %v", resp, err)
	} else if b, _ := io.ReadAll(resp.Body); string(b) != "%PDF-1.4 hello" {
		t.Errorf("download body = %q", b)
	}
	vera.do("DELETE", "/api/v1/documents/"+itoa(doc.ID), nil, 204, nil)
	vera.do("GET", "/api/v1/documents", nil, 200, &uploaded)
	if len(uploaded.Documents) != 0 {
		t.Errorf("document still listed after delete")
	}

	// Swim classes.
	var swim struct {
		DoneCount int  `json:"done_count"`
		Next      *int `json:"next"`
	}
	milos.do("PUT", "/api/v1/swim/classes/1", map[string]bool{"done": true}, 200, &swim)
	milos.do("PUT", "/api/v1/swim/classes/1", map[string]bool{"done": true}, 200, &swim)
	if swim.DoneCount != 1 || swim.Next == nil || *swim.Next != 2 {
		t.Errorf("swim: %+v", swim)
	}
	milos.do("PUT", "/api/v1/swim/classes/21", map[string]bool{"done": true}, 404, nil)
	milos.do("POST", "/api/v1/swim/reset", nil, 200, &swim)
	if swim.DoneCount != 0 {
		t.Errorf("swim reset: %+v", swim)
	}

	// Another family sees none of it. (Signing up needs an invite, so Eve's
	// family is made directly in the database.)
	app.db.Exec(`INSERT INTO families (id, name, invite_code) VALUES (99, 'Other', 'OTHER999')`)
	app.db.Exec(`INSERT INTO users (id, email, name, password_hash, family_id) VALUES (99, 'eve@x', 'Eve', '', 99)`)
	eveToken, _, _ := app.newSession(99, apiTokenTTL)
	eve := &apiClient{t: t, base: base, token: eveToken}
	eve.do("GET", "/api/v1/shopping", nil, 200, &list)
	if len(list.Items) != 0 {
		t.Errorf("other family sees %d shopping items", len(list.Items))
	}
	eve.do("PATCH", "/api/v1/shopping/items/"+itoa(item.ID), map[string]any{"bought": false}, 404, nil)
	eve.do("POST", "/api/v1/shopping/notify/"+itoa(milosSession.User.ID), nil, 404, nil)
	req, _ = http.NewRequest("GET", base+*updated.PhotoURL, nil)
	req.Header.Set("Authorization", "Bearer "+eveToken)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 404 {
		t.Errorf("other family photo download: %v %v", resp, err)
	}

	// FCM isn't configured in tests.
	milos.do("POST", "/api/v1/push/fcm", map[string]string{"token": "abc"}, 503, nil)
	var info struct {
		FCMAvailable bool `json:"fcm_available"`
	}
	anon.do("GET", "/api/v1/", nil, 200, &info)

	// Logging out ends the token.
	milos.do("POST", "/api/v1/auth/logout", nil, 204, nil)
	milos.do("GET", "/api/v1/me", nil, 401, nil)
	vera.do("GET", "/api/v1/me", nil, 200, nil)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestAPINotifyFCM(t *testing.T) {
	app, base := newAPITestServer(t)

	// A fake FCM endpoint: token "gone" is unknown to it.
	var got []map[string]any
	fcm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		if strings.Contains(fmt.Sprint(body), "gone") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`)
			return
		}
		io.WriteString(w, `{"name":"projects/p/messages/1"}`)
	}))
	defer fcm.Close()
	app.fcm = &fcmSender{client: fcm.Client(), sendURL: fcm.URL}

	anon := &apiClient{t: t, base: base}
	var milosS, veraS apiSession
	anon.do("POST", "/api/v1/auth/register", map[string]string{"email": "milos@x.com", "name": "Miloš", "password": "secret123"}, 201, &milosS)
	var code string
	app.db.QueryRow(`SELECT invite_code FROM families`).Scan(&code)
	anon.do("POST", "/api/v1/auth/register", map[string]string{"email": "vera@x.com", "name": "Vera", "password": "secret123", "invite_code": code}, 201, &veraS)
	milos := &apiClient{t: t, base: base, token: milosS.Token}
	vera := &apiClient{t: t, base: base, token: veraS.Token}

	milos.do("POST", "/api/v1/push/fcm", map[string]string{"token": "phone-1"}, 204, nil)
	milos.do("POST", "/api/v1/push/fcm", map[string]string{"token": "gone"}, 204, nil)
	milos.do("POST", "/api/v1/push/fcm", map[string]string{"token": "has space"}, 400, nil)
	vera.do("POST", "/api/v1/shopping/items", map[string]string{"name": "mleko"}, 201, nil)

	var family struct {
		Members []struct {
			Name          string
			Notifications bool
		}
	}
	vera.do("GET", "/api/v1/family", nil, 200, &family)
	if !family.Members[0].Notifications || family.Members[1].Notifications {
		t.Errorf("notification flags: %+v", family.Members)
	}

	var res struct{ Sent int }
	vera.do("POST", "/api/v1/shopping/notify/"+itoa(milosS.User.ID), nil, 200, &res)
	if res.Sent != 1 || len(got) != 2 {
		t.Fatalf("sent = %d, FCM got %d requests", res.Sent, len(got))
	}
	msg := got[0]["message"].(map[string]any)
	if n := msg["notification"].(map[string]any); n["title"] != "Vera: 1 item to buy" || n["body"] != "mleko" {
		t.Errorf("notification = %v", n)
	}
	var left int
	app.db.QueryRow(`SELECT COUNT(*) FROM fcm_tokens`).Scan(&left)
	if left != 1 {
		t.Errorf("%d FCM tokens left, want 1 (the unregistered one removed)", left)
	}

	// Logging out with the token stops notifications to that phone.
	milos.do("POST", "/api/v1/auth/logout", map[string]string{"fcm_token": "phone-1"}, 204, nil)
	app.db.QueryRow(`SELECT COUNT(*) FROM fcm_tokens`).Scan(&left)
	if left != 0 {
		t.Errorf("FCM token kept after logout")
	}
}
