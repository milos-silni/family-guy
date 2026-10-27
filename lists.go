package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
)

type ShoppingItem struct {
	ID       int64
	Name     string
	Quantity string
	Done     bool
	NotFound bool // looked for it but couldn't find it
	Photo    string
	AddedBy  string
}

type ShoppingData struct {
	ListData[ShoppingItem]
	NotFoundCount  int
	Favorites      []Favorite // not currently on the list
	FavoritesTotal int

	NotifyTargets []NotifyTarget // family members with notifications on
	Notice        string         // result of the last "Notify" tap
	NoticeFailed  bool
	Finished      int // items just saved to history by "Finish shopping"
}

// OpenCount is the number of items still to buy (not bought, not marked "not found").
func (d ShoppingData) OpenCount() int {
	return len(d.Items) - d.DoneCount - d.NotFoundCount
}

type Todo struct {
	ID      int64
	Title   string
	Done    bool
	AddedBy string
}

type ListData[T any] struct {
	Items     []T
	DoneCount int
	EditID    int64 // item shown as an edit form, 0 for none
}

// OpenCount is the number of items not yet done (not bought).
func (d ListData[T]) OpenCount() int {
	return len(d.Items) - d.DoneCount
}

func (a *App) shoppingPage(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := a.db.Query(`
		SELECT i.id, i.name, i.quantity, i.done, i.not_found, i.photo, COALESCE(u.name, '')
		FROM shopping_items i LEFT JOIN users u ON u.id = i.added_by
		WHERE i.family_id = ?
		ORDER BY i.done, i.not_found, i.id`, u.FamilyID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer rows.Close()

	var data ShoppingData
	data.EditID, _ = strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64)
	for rows.Next() {
		var it ShoppingItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Quantity, &it.Done, &it.NotFound, &it.Photo, &it.AddedBy); err != nil {
			a.serverError(w, err)
			return
		}
		switch {
		case it.Done:
			data.DoneCount++
		case it.NotFound:
			data.NotFoundCount++
		}
		data.Items = append(data.Items, it)
	}
	if err := rows.Err(); err != nil {
		a.serverError(w, err)
		return
	}
	if data.Favorites, data.FavoritesTotal, err = a.loadFavorites(u.FamilyID); err != nil {
		a.serverError(w, err)
		return
	}
	if data.NotifyTargets, err = a.notifyTargets(u); err != nil {
		a.serverError(w, err)
		return
	}
	data.Finished, _ = strconv.Atoi(r.URL.Query().Get("finished"))
	if r.URL.Query().Get("photo") == "bad" {
		data.Notice, data.NoticeFailed = "Couldn't save the photo. It must be a JPEG, PNG or WebP image up to 10 MB.", true
	}
	// After a "Notify" tap: /shopping?notified=<user id>&sent=<devices reached>
	if id, err := strconv.ParseInt(r.URL.Query().Get("notified"), 10, 64); err == nil {
		var name string
		if a.db.QueryRow(`SELECT name FROM users WHERE id = ? AND family_id = ?`, id, u.FamilyID).Scan(&name) == nil {
			if r.URL.Query().Get("sent") == "0" {
				data.Notice, data.NoticeFailed = "Couldn't reach "+name+"'s phone. Ask them to turn notifications on again in the Family tab.", true
			} else {
				data.Notice = "Sent to " + name + " 🔔"
			}
		}
	}
	a.render(w, http.StatusOK, "shopping", Page{Title: "Shopping list", User: u, Tab: "shopping", Data: data})
}

func (a *App) addShoppingItem(w http.ResponseWriter, r *http.Request, u *User) {
	name := limit(r.FormValue("name"), 100)
	qty := limit(r.FormValue("quantity"), 20)
	if name != "" {
		if _, err := a.db.Exec(`INSERT INTO shopping_items (family_id, name, quantity, added_by) VALUES (?, ?, ?, ?)`,
			u.FamilyID, name, qty, u.ID); err != nil {
			a.serverError(w, err)
			return
		}
	}
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}

func (a *App) editShoppingItem(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var oldPhoto string
	err = a.db.QueryRow(`SELECT photo FROM shopping_items WHERE id = ? AND family_id = ?`, id, u.FamilyID).Scan(&oldPhoto)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}

	// The edit form is multipart when it carries a photo.
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotoSize+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		http.Redirect(w, r, "/shopping?photo=bad", http.StatusSeeOther)
		return
	}
	name := limit(r.FormValue("name"), 100)
	qty := limit(r.FormValue("quantity"), 20)
	photo := oldPhoto
	if r.FormValue("remove_photo") == "1" {
		photo = ""
	}
	if file, _, err := r.FormFile("photo"); err == nil {
		photo, err = a.savePhoto(file)
		file.Close()
		if err != nil {
			if !errors.Is(err, errBadPhoto) {
				log.Printf("save photo: %v", err)
			}
			http.Redirect(w, r, "/shopping?photo=bad", http.StatusSeeOther)
			return
		}
	}

	if name != "" || photo != oldPhoto {
		// An empty name keeps the current one.
		if _, err := a.db.Exec(`UPDATE shopping_items SET name = COALESCE(NULLIF(?, ''), name), quantity = ?, photo = ?
			WHERE id = ? AND family_id = ?`, name, qty, photo, id, u.FamilyID); err != nil {
			a.serverError(w, err)
			return
		}
	}
	if photo != oldPhoto {
		a.removePhotoIfUnused(oldPhoto)
	}
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}

func (a *App) deleteShoppingItem(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var photo string
	err = a.db.QueryRow(`DELETE FROM shopping_items WHERE id = ? AND family_id = ? RETURNING photo`, id, u.FamilyID).Scan(&photo)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		a.serverError(w, err)
		return
	}
	a.removePhotoIfUnused(photo)
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}

// Ticking an item (bought / not bought) always clears "not found".
func (a *App) toggleBought(w http.ResponseWriter, r *http.Request, u *User) {
	a.updateShoppingItem(w, r, u, `UPDATE shopping_items SET done = NOT done, not_found = 0 WHERE id = ? AND family_id = ?`)
}

func (a *App) toggleNotFound(w http.ResponseWriter, r *http.Request, u *User) {
	a.updateShoppingItem(w, r, u, `UPDATE shopping_items SET not_found = NOT not_found, done = 0 WHERE id = ? AND family_id = ?`)
}

func (a *App) updateShoppingItem(w http.ResponseWriter, r *http.Request, u *User, query string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := a.db.Exec(query, id, u.FamilyID); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}

func (a *App) todosPage(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := a.db.Query(`
		SELECT t.id, t.title, t.done, COALESCE(u.name, '')
		FROM todos t LEFT JOIN users u ON u.id = t.added_by
		WHERE t.family_id = ?
		ORDER BY t.done, t.id`, u.FamilyID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer rows.Close()

	var data ListData[Todo]
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.ID, &t.Title, &t.Done, &t.AddedBy); err != nil {
			a.serverError(w, err)
			return
		}
		if t.Done {
			data.DoneCount++
		}
		data.Items = append(data.Items, t)
	}
	if err := rows.Err(); err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, http.StatusOK, "todos", Page{Title: "To-do", User: u, Tab: "todos", Data: data})
}

func (a *App) addTodo(w http.ResponseWriter, r *http.Request, u *User) {
	title := limit(r.FormValue("title"), 200)
	if title != "" {
		if _, err := a.db.Exec(`INSERT INTO todos (family_id, title, added_by) VALUES (?, ?, ?)`,
			u.FamilyID, title, u.ID); err != nil {
			a.serverError(w, err)
			return
		}
	}
	http.Redirect(w, r, "/todos", http.StatusSeeOther)
}

// The table argument is always a constant from routes(), never user input.

func (a *App) toggle(table, back string) func(http.ResponseWriter, *http.Request, *User) {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if _, err := a.db.Exec(`UPDATE `+table+` SET done = NOT done WHERE id = ? AND family_id = ?`, id, u.FamilyID); err != nil {
			a.serverError(w, err)
			return
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

func (a *App) remove(table, back string) func(http.ResponseWriter, *http.Request, *User) {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if _, err := a.db.Exec(`DELETE FROM `+table+` WHERE id = ? AND family_id = ?`, id, u.FamilyID); err != nil {
			a.serverError(w, err)
			return
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

func (a *App) clearDone(table, back string) func(http.ResponseWriter, *http.Request, *User) {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		if _, err := a.db.Exec(`DELETE FROM `+table+` WHERE done = 1 AND family_id = ?`, u.FamilyID); err != nil {
			a.serverError(w, err)
			return
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

type Member struct {
	Name   string
	Email  string
	Notify bool // has notifications on at least one device
}

func (a *App) familyPage(w http.ResponseWriter, r *http.Request, u *User) {
	var data struct {
		Name       string
		InviteCode string
		Members    []Member
	}
	if err := a.db.QueryRow(`SELECT name, invite_code FROM families WHERE id = ?`, u.FamilyID).
		Scan(&data.Name, &data.InviteCode); err != nil {
		a.serverError(w, err)
		return
	}
	rows, err := a.db.Query(`
		SELECT name, email, EXISTS (SELECT 1 FROM push_subscriptions p WHERE p.user_id = users.id)
		FROM users WHERE family_id = ? ORDER BY id`, u.FamilyID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Name, &m.Email, &m.Notify); err != nil {
			a.serverError(w, err)
			return
		}
		data.Members = append(data.Members, m)
	}
	a.render(w, http.StatusOK, "family", Page{Title: "Family", User: u, Tab: "family", Data: data})
}

func limit(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}
