package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// Shopping list, favorites ("Often bought") and history endpoints of the JSON API.

func (a *App) apiShoppingList(w http.ResponseWriter, r *http.Request, u *User) {
	items, err := a.loadShoppingItems(u.FamilyID, 0)
	if err != nil {
		apiServerError(w, err)
		return
	}
	out := make([]apiShoppingItem, 0, len(items))
	var bought, notFound int
	for _, it := range items {
		switch {
		case it.Done:
			bought++
		case it.NotFound:
			notFound++
		}
		out = append(out, toAPIShoppingItem(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":           out,
		"open_count":      len(items) - bought - notFound,
		"bought_count":    bought,
		"not_found_count": notFound,
	})
}

// writeShoppingItem answers with item id, or 404 if the family has no such item.
func (a *App) writeShoppingItem(w http.ResponseWriter, status int, u *User, id int64) {
	items, err := a.loadShoppingItems(u.FamilyID, id)
	if err != nil {
		apiServerError(w, err)
		return
	}
	if len(items) == 0 {
		apiError(w, http.StatusNotFound, "That item is no longer on the list.")
		return
	}
	writeJSON(w, status, toAPIShoppingItem(items[0]))
}

func (a *App) apiAddShoppingItem(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Name     string `json:"name"`
		Quantity string `json:"quantity"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name, qty := limit(body.Name, 100), limit(body.Quantity, 20)
	if name == "" {
		apiError(w, http.StatusUnprocessableEntity, "Please enter what to buy.")
		return
	}
	res, err := a.db.Exec(`INSERT INTO shopping_items (family_id, name, quantity, added_by) VALUES (?, ?, ?, ?)`,
		u.FamilyID, name, qty, u.ID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	id, _ := res.LastInsertId()
	a.writeShoppingItem(w, http.StatusCreated, u, id)
}

// apiUpdateShoppingItem changes only the fields present in the body.
// Setting bought to true clears not_found and the other way round.
func (a *App) apiUpdateShoppingItem(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Name     *string `json:"name"`
		Quantity *string `json:"quantity"`
		Bought   *bool   `json:"bought"`
		NotFound *bool   `json:"not_found"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var name, qty *string
	if body.Name != nil {
		n := limit(*body.Name, 100)
		if n == "" {
			apiError(w, http.StatusUnprocessableEntity, "The name can't be empty.")
			return
		}
		name = &n
	}
	if body.Quantity != nil {
		q := limit(*body.Quantity, 20)
		qty = &q
	}
	if body.Bought != nil && body.NotFound != nil && *body.Bought && *body.NotFound {
		apiError(w, http.StatusUnprocessableEntity, "An item can't be both bought and not found.")
		return
	}
	var done, notFound *bool
	f := false
	if body.Bought != nil {
		done = body.Bought
		if *body.Bought {
			notFound = &f
		}
	}
	if body.NotFound != nil {
		notFound = body.NotFound
		if *body.NotFound {
			done = &f
		}
	}
	res, err := a.db.Exec(`UPDATE shopping_items SET
		name = COALESCE(?, name), quantity = COALESCE(?, quantity),
		done = COALESCE(?, done), not_found = COALESCE(?, not_found)
		WHERE id = ? AND family_id = ?`, name, qty, done, notFound, id, u.FamilyID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		apiError(w, http.StatusNotFound, "That item is no longer on the list.")
		return
	}
	a.writeShoppingItem(w, http.StatusOK, u, id)
}

func (a *App) apiDeleteShoppingItem(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var photo string
	err := a.db.QueryRow(`DELETE FROM shopping_items WHERE id = ? AND family_id = ? RETURNING photo`, id, u.FamilyID).Scan(&photo)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		apiServerError(w, err)
		return
	}
	a.removePhotoIfUnused(photo)
	w.WriteHeader(http.StatusNoContent)
}

// apiSetPhoto takes a multipart form with the image in the "photo" field.
func (a *App) apiSetPhoto(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	old, ok := a.itemPhoto(w, u, id)
	if !ok {
		return
	}
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotoSize+1<<20)
	file, _, err := r.FormFile("photo")
	if err != nil {
		apiError(w, http.StatusBadRequest, `Send the image as multipart form field "photo".`)
		return
	}
	photo, err := a.savePhoto(file)
	file.Close()
	if err != nil {
		if !errors.Is(err, errBadPhoto) {
			log.Printf("save photo: %v", err)
		}
		apiError(w, http.StatusUnprocessableEntity, "Couldn't save the photo. It must be a JPEG, PNG or WebP image up to 10 MB.")
		return
	}
	a.replacePhoto(w, u, id, old, photo)
}

func (a *App) apiRemovePhoto(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	old, ok := a.itemPhoto(w, u, id)
	if !ok {
		return
	}
	a.replacePhoto(w, u, id, old, "")
}

// itemPhoto returns the current photo of an item, answering 404 if there is no such item.
func (a *App) itemPhoto(w http.ResponseWriter, u *User, id int64) (string, bool) {
	var photo string
	err := a.db.QueryRow(`SELECT photo FROM shopping_items WHERE id = ? AND family_id = ?`, id, u.FamilyID).Scan(&photo)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, http.StatusNotFound, "That item is no longer on the list.")
		return "", false
	} else if err != nil {
		apiServerError(w, err)
		return "", false
	}
	return photo, true
}

func (a *App) replacePhoto(w http.ResponseWriter, u *User, id int64, old, photo string) {
	if _, err := a.db.Exec(`UPDATE shopping_items SET photo = ? WHERE id = ? AND family_id = ?`, photo, id, u.FamilyID); err != nil {
		a.removePhotoIfUnused(photo)
		apiServerError(w, err)
		return
	}
	if old != photo {
		a.removePhotoIfUnused(old)
	}
	a.writeShoppingItem(w, http.StatusOK, u, id)
}

func (a *App) apiFinishShopping(w http.ResponseWriter, r *http.Request, u *User) {
	bought, err := a.finishTrip(u)
	if err != nil {
		apiServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"bought": bought})
}

func (a *App) apiNotify(w http.ResponseWriter, r *http.Request, u *User) {
	target, ok := pathID(w, r, "user")
	if !ok {
		return
	}
	sent, err := a.sendShoppingList(u, target, "https://"+r.Host)
	if errors.Is(err, errNotFound) {
		apiError(w, http.StatusNotFound, "No such family member.")
		return
	} else if err != nil {
		apiServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": sent})
}

// ---- favorites ----

func (a *App) apiFavorites(w http.ResponseWriter, r *http.Request, u *User) {
	type favorite struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Quantity string `json:"quantity"`
		OnList   bool   `json:"on_list"` // on the shopping list, not bought yet
	}
	rows, err := a.db.Query(`
		SELECT f.id, f.name, f.quantity, EXISTS (
			SELECT 1 FROM shopping_items i
			WHERE i.family_id = f.family_id AND i.done = 0 AND i.name = f.name COLLATE NOCASE)
		FROM favorites f WHERE f.family_id = ? ORDER BY f.id`, u.FamilyID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	defer rows.Close()
	favs := []favorite{}
	for rows.Next() {
		var f favorite
		if err := rows.Scan(&f.ID, &f.Name, &f.Quantity, &f.OnList); err != nil {
			apiServerError(w, err)
			return
		}
		favs = append(favs, f)
	}
	if err := rows.Err(); err != nil {
		apiServerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"favorites": favs})
}

// apiAddFavorites takes {"items": [{"name", "quantity"}]} and/or {"text": "..."}
// (pasted lines, parsed like the web form), then answers with all favorites.
func (a *App) apiAddFavorites(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Items []struct {
			Name     string `json:"name"`
			Quantity string `json:"quantity"`
		} `json:"items"`
		Text string `json:"text"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	favs := parseItemLines(body.Text)
	for _, it := range body.Items {
		if name := limit(it.Name, 100); name != "" {
			favs = append(favs, Favorite{Name: name, Quantity: limit(it.Quantity, 20)})
		}
	}
	if len(favs) == 0 {
		apiError(w, http.StatusUnprocessableEntity, "Please enter at least one item.")
		return
	}
	if err := a.saveFavorites(u.FamilyID, favs); err != nil {
		apiServerError(w, err)
		return
	}
	a.apiFavorites(w, r, u)
}

func (a *App) apiDeleteFavorite(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := a.db.Exec(`DELETE FROM favorites WHERE id = ? AND family_id = ?`, id, u.FamilyID); err != nil {
		apiServerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) apiFavoriteToList(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	a.copyFavorites(w, u, ` AND f.id = ?`, id)
}

func (a *App) apiAllFavoritesToList(w http.ResponseWriter, r *http.Request, u *User) {
	a.copyFavorites(w, u, ` ORDER BY f.id`)
}

func (a *App) copyFavorites(w http.ResponseWriter, u *User, extra string, args ...any) {
	res, err := a.db.Exec(copyFavoritesSQL+extra, append([]any{u.ID, u.FamilyID}, args...)...)
	if err != nil {
		apiServerError(w, err)
		return
	}
	n, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]int64{"added": n})
}

// ---- history ----

func (a *App) apiHistory(w http.ResponseWriter, r *http.Request, u *User) {
	trips, err := a.loadTrips(u.FamilyID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	type tripItem struct {
		ID       int64   `json:"id"`
		Name     string  `json:"name"`
		Quantity string  `json:"quantity"`
		PhotoURL *string `json:"photo_url"`
		OnList   bool    `json:"on_list"`
	}
	type trip struct {
		ID         int64      `json:"id"`
		FinishedAt time.Time  `json:"finished_at"`
		FinishedBy string     `json:"finished_by"`
		Bought     []tripItem `json:"bought"`
		NotFound   []tripItem `json:"not_found"`
	}
	conv := func(items []TripItem) []tripItem {
		out := make([]tripItem, 0, len(items))
		for _, it := range items {
			out = append(out, tripItem{ID: it.ID, Name: it.Name, Quantity: it.Quantity, PhotoURL: apiPhotoURL(it.Photo), OnList: it.OnList})
		}
		return out
	}
	out := make([]trip, 0, len(trips))
	for _, t := range trips {
		out = append(out, trip{ID: t.ID, FinishedAt: t.FinishedAt.UTC(), FinishedBy: t.FinishedBy, Bought: conv(t.Bought), NotFound: conv(t.NotFound)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"trips": out})
}

func (a *App) apiTripItemToList(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	added, err := a.copyTripItemToList(u, id)
	if errors.Is(err, errNotFound) {
		apiError(w, http.StatusNotFound, "Not found.")
		return
	} else if err != nil {
		apiServerError(w, err)
		return
	}
	n := 0
	if added {
		n = 1
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": n})
}

// ---- Android push registration ----

func (a *App) apiRegisterFCM(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if a.fcm == nil {
		apiError(w, http.StatusServiceUnavailable, "Notifications for the app are not set up on the server.")
		return
	}
	if body.Token == "" || len(body.Token) > 4096 || strings.ContainsAny(body.Token, " \t\r\n") {
		apiError(w, http.StatusBadRequest, "Invalid FCM token.")
		return
	}
	// A device belongs to whoever registered last on it.
	if _, err := a.db.Exec(`INSERT INTO fcm_tokens (user_id, token) VALUES (?, ?)
		ON CONFLICT (token) DO UPDATE SET user_id = excluded.user_id`, u.ID, body.Token); err != nil {
		apiServerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) apiUnregisterFCM(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if _, err := a.db.Exec(`DELETE FROM fcm_tokens WHERE token = ? AND user_id = ?`, body.Token, u.ID); err != nil {
		apiServerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
