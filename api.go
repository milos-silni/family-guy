package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// JSON API for the Android app, under /api/v1. See API.md.
//
// Clients log in with POST /api/v1/auth/login and send the returned token as
// "Authorization: Bearer <token>". Tokens are rows in the sessions table, like
// web sessions, and stay valid for apiTokenTTL after they were last used.

const (
	apiTokenTTL  = 90 * 24 * time.Hour
	maxJSONBody  = 64 << 10
	apiVersion   = 1
	apiPrefix    = "/api/v1"
	tokenRefresh = 24 * time.Hour // expiry is pushed back at most once a day
)

func (a *App) apiRoutes(mux *http.ServeMux) {
	p := apiPrefix
	mux.HandleFunc("GET "+p+"/{$}", a.apiInfo)

	mux.HandleFunc("POST "+p+"/auth/login", a.apiLogin)
	mux.HandleFunc("POST "+p+"/auth/register", a.apiRegister)
	mux.Handle("POST "+p+"/auth/logout", a.api(a.apiLogout))
	mux.Handle("GET "+p+"/me", a.api(a.apiMe))
	mux.Handle("GET "+p+"/family", a.api(a.apiFamily))

	mux.Handle("GET "+p+"/shopping", a.api(a.apiShoppingList))
	mux.Handle("POST "+p+"/shopping/items", a.api(a.apiAddShoppingItem))
	mux.Handle("PATCH "+p+"/shopping/items/{id}", a.api(a.apiUpdateShoppingItem))
	mux.Handle("DELETE "+p+"/shopping/items/{id}", a.api(a.apiDeleteShoppingItem))
	mux.Handle("PUT "+p+"/shopping/items/{id}/photo", a.api(a.apiSetPhoto))
	mux.Handle("DELETE "+p+"/shopping/items/{id}/photo", a.api(a.apiRemovePhoto))
	mux.Handle("GET "+p+"/shopping/photos/{name}", a.api(a.servePhoto))
	mux.Handle("POST "+p+"/shopping/finish", a.api(a.apiFinishShopping))
	mux.Handle("POST "+p+"/shopping/notify/{user}", a.api(a.apiNotify))

	mux.Handle("GET "+p+"/shopping/favorites", a.api(a.apiFavorites))
	mux.Handle("POST "+p+"/shopping/favorites", a.api(a.apiAddFavorites))
	mux.Handle("DELETE "+p+"/shopping/favorites/{id}", a.api(a.apiDeleteFavorite))
	mux.Handle("POST "+p+"/shopping/favorites/{id}/add-to-list", a.api(a.apiFavoriteToList))
	mux.Handle("POST "+p+"/shopping/favorites/add-all-to-list", a.api(a.apiAllFavoritesToList))

	mux.Handle("GET "+p+"/shopping/history", a.api(a.apiHistory))
	mux.Handle("POST "+p+"/shopping/history/items/{id}/add-to-list", a.api(a.apiTripItemToList))

	mux.Handle("GET "+p+"/todos", a.api(a.apiTodos))
	mux.Handle("POST "+p+"/todos", a.api(a.apiAddTodo))
	mux.Handle("PATCH "+p+"/todos/{id}", a.api(a.apiUpdateTodo))
	mux.Handle("DELETE "+p+"/todos/{id}", a.api(a.apiDeleteTodo))
	mux.Handle("POST "+p+"/todos/clear-done", a.api(a.apiClearDoneTodos))

	mux.Handle("GET "+p+"/documents", a.api(a.apiDocuments))
	mux.Handle("POST "+p+"/documents", a.api(a.apiUploadDocuments))
	mux.Handle("GET "+p+"/documents/{id}/download", a.api(a.downloadDocument))
	mux.Handle("PATCH "+p+"/documents/{id}", a.api(a.apiRenameDocument))
	mux.Handle("DELETE "+p+"/documents/{id}", a.api(a.apiDeleteDocument))

	mux.Handle("GET "+p+"/swim", a.api(a.apiSwim))
	mux.Handle("PUT "+p+"/swim/classes/{n}", a.api(a.apiSetSwimClass))
	mux.Handle("POST "+p+"/swim/reset", a.api(a.apiResetSwim))

	mux.Handle("POST "+p+"/push/fcm", a.api(a.apiRegisterFCM))
	mux.Handle("DELETE "+p+"/push/fcm", a.api(a.apiUnregisterFCM))

	// Anything else under /api/ answers in JSON too.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		apiError(w, http.StatusNotFound, "Not found.")
	})
}

// ---- plumbing ----

// api wraps a handler that needs a logged-in user, taken from the bearer token.
func (a *App) api(h func(http.ResponseWriter, *http.Request, *User)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		var u *User
		var err error
		if token != "" {
			u, err = a.sessionUser(token)
		}
		if err != nil {
			apiServerError(w, err)
			return
		}
		if u == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="family-guy"`)
			apiError(w, http.StatusUnauthorized, "Please log in again.")
			return
		}
		// Sliding expiry: a token in use doesn't run out.
		now := time.Now()
		a.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token_hash = ? AND expires_at < ?`,
			now.Add(apiTokenTTL).Unix(), hashToken(token), now.Add(apiTokenTTL-tokenRefresh).Unix())
		h(w, r, u)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// apiError answers {"error": msg}; msg is meant to be shown to the user.
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func apiServerError(w http.ResponseWriter, err error) {
	log.Printf("api error: %v", err)
	apiError(w, http.StatusInternalServerError, "Something went wrong. Please try again.")
}

// readJSON decodes the request body into dst, answering 400 when it can't.
func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(dst)
	if err != nil && !errors.Is(err, io.EOF) {
		apiError(w, http.StatusBadRequest, "The request body is not valid JSON.")
		return false
	}
	return true
}

// pathID parses the {id} path value, answering 404 when it isn't a number.
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusNotFound, "Not found.")
		return 0, false
	}
	return id, true
}

func (a *App) apiInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":          "family-guy",
		"api_version":   apiVersion,
		"fcm_available": a.fcm != nil,
	})
}

// ---- JSON shapes ----

type apiUser struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	FamilyID int64  `json:"family_id"`
}

func toAPIUser(u *User) apiUser {
	return apiUser{ID: u.ID, Email: u.Email, Name: u.Name, FamilyID: u.FamilyID}
}

type apiSession struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      apiUser   `json:"user"`
}

type apiShoppingItem struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Quantity  string    `json:"quantity"`
	Bought    bool      `json:"bought"`
	NotFound  bool      `json:"not_found"`
	PhotoURL  *string   `json:"photo_url"`
	AddedBy   string    `json:"added_by"`
	CreatedAt time.Time `json:"created_at"`
}

func apiPhotoURL(name string) *string {
	if name == "" {
		return nil
	}
	s := apiPrefix + "/shopping/photos/" + name
	return &s
}

func toAPIShoppingItem(it ShoppingItem) apiShoppingItem {
	return apiShoppingItem{
		ID: it.ID, Name: it.Name, Quantity: it.Quantity, Bought: it.Done, NotFound: it.NotFound,
		PhotoURL: apiPhotoURL(it.Photo), AddedBy: it.AddedBy, CreatedAt: it.Created.UTC(),
	}
}

type apiTodo struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	AddedBy   string    `json:"added_by"`
	CreatedAt time.Time `json:"created_at"`
}

func toAPITodo(t Todo) apiTodo {
	return apiTodo{ID: t.ID, Title: t.Title, Done: t.Done, AddedBy: t.AddedBy, CreatedAt: t.Created.UTC()}
}

type apiDocument struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Size        int64     `json:"size"`
	AddedBy     string    `json:"added_by"`
	CreatedAt   time.Time `json:"created_at"`
	DownloadURL string    `json:"download_url"`
}

func toAPIDocument(d Document) apiDocument {
	return apiDocument{
		ID: d.ID, Name: d.Name, Kind: d.Kind(), Size: d.Size, AddedBy: d.AddedBy, CreatedAt: d.CreatedAt.UTC(),
		DownloadURL: apiPrefix + "/documents/" + strconv.FormatInt(d.ID, 10) + "/download",
	}
}

// ---- auth ----

func (a *App) apiLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	id, err := a.authenticate(body.Email, body.Password)
	if errors.Is(err, errWrongLogin) {
		apiError(w, http.StatusUnauthorized, "Wrong email or password.")
		return
	} else if err != nil {
		apiServerError(w, err)
		return
	}
	a.apiStartSession(w, http.StatusOK, id)
}

func (a *App) apiRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email      string `json:"email"`
		Name       string `json:"name"`
		Password   string `json:"password"`
		InviteCode string `json:"invite_code"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	id, err := a.createAccount(body.Email, body.Name, body.Password, body.InviteCode)
	var fe formError
	if errors.As(err, &fe) {
		apiError(w, http.StatusUnprocessableEntity, fe.Error())
		return
	} else if err != nil {
		apiServerError(w, err)
		return
	}
	a.apiStartSession(w, http.StatusCreated, id)
}

func (a *App) apiStartSession(w http.ResponseWriter, status int, userID int64) {
	token, expires, err := a.newSession(userID, apiTokenTTL)
	if err != nil {
		apiServerError(w, err)
		return
	}
	u, err := a.sessionUser(token)
	if err != nil || u == nil {
		apiServerError(w, errors.Join(err, errors.New("new session not found")))
		return
	}
	writeJSON(w, status, apiSession{Token: token, ExpiresAt: expires.UTC().Truncate(time.Second), User: toAPIUser(u)})
}

// apiLogout ends the session. An optional {"fcm_token": "..."} also stops
// notifications to this device.
func (a *App) apiLogout(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		FCMToken string `json:"fcm_token"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.FCMToken != "" {
		a.db.Exec(`DELETE FROM fcm_tokens WHERE token = ? AND user_id = ?`, body.FCMToken, u.ID)
	}
	if _, err := a.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(bearerToken(r))); err != nil {
		apiServerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) apiMe(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, map[string]any{"user": toAPIUser(u)})
}

func (a *App) apiFamily(w http.ResponseWriter, r *http.Request, u *User) {
	f, err := a.loadFamily(u.FamilyID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	type member struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		Email         string `json:"email"`
		Notifications bool   `json:"notifications"`
		IsMe          bool   `json:"is_me"`
	}
	members := []member{}
	for _, m := range f.Members {
		members = append(members, member{ID: m.ID, Name: m.Name, Email: m.Email, Notifications: m.Notify, IsMe: m.ID == u.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": f.Name, "invite_code": f.InviteCode, "members": members})
}
