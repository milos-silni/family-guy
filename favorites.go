package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Favorite is an item the family buys often and can add to the list with one tap.
type Favorite struct {
	ID       int64
	Name     string
	Quantity string
}

type FavoritesData struct {
	Items []Favorite
}

var (
	// Leading "-", "*", "•" and "[ ]" / "[x]" from pasted checklists.
	bulletPattern = regexp.MustCompile(`^(?:[-*•·]\s*)?(?:\[[ xX]?\]\s*)?`)
	// A trailing amount such as "4 kom", "2L", "0,5 kg".
	quantityPattern = regexp.MustCompile(`(?i)^(.+?)\s+(\d+(?:[.,]\d+)?\s*(?:kom|kg|g|l|ml|pak|x)?\.?)$`)
)

// parseItemLines turns pasted text (one item per line) into favorites.
func parseItemLines(text string) []Favorite {
	var items []Favorite
	for _, line := range strings.Split(text, "\n") {
		// Drop invisible characters (e.g. word joiners some apps put around bullets).
		line = strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, line)
		line = strings.TrimSpace(bulletPattern.ReplaceAllString(strings.TrimSpace(line), ""))
		if line == "" {
			continue
		}
		f := Favorite{Name: line}
		if m := quantityPattern.FindStringSubmatch(line); m != nil {
			f.Name, f.Quantity = m[1], m[2]
		}
		f.Name, f.Quantity = limit(f.Name, 100), limit(f.Quantity, 20)
		items = append(items, f)
	}
	return items
}

// loadFavorites returns the family's favorites that are not already on the list
// (as a not-bought item), plus the total number of favorites.
func (a *App) loadFavorites(familyID int64) ([]Favorite, int, error) {
	var total int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM favorites WHERE family_id = ?`, familyID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := a.db.Query(`
		SELECT f.id, f.name, f.quantity FROM favorites f
		WHERE f.family_id = ? AND NOT EXISTS (
			SELECT 1 FROM shopping_items i
			WHERE i.family_id = f.family_id AND i.done = 0 AND i.name = f.name COLLATE NOCASE)
		ORDER BY f.id`, familyID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var favs []Favorite
	for rows.Next() {
		var f Favorite
		if err := rows.Scan(&f.ID, &f.Name, &f.Quantity); err != nil {
			return nil, 0, err
		}
		favs = append(favs, f)
	}
	return favs, total, rows.Err()
}

func (a *App) favoritesPage(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := a.db.Query(`SELECT id, name, quantity FROM favorites WHERE family_id = ? ORDER BY id`, u.FamilyID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer rows.Close()
	var data FavoritesData
	for rows.Next() {
		var f Favorite
		if err := rows.Scan(&f.ID, &f.Name, &f.Quantity); err != nil {
			a.serverError(w, err)
			return
		}
		data.Items = append(data.Items, f)
	}
	if err := rows.Err(); err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, http.StatusOK, "favorites", Page{Title: "Often bought", User: u, Tab: "shopping", Data: data})
}

func (a *App) addFavorites(w http.ResponseWriter, r *http.Request, u *User) {
	for _, f := range parseItemLines(r.FormValue("items")) {
		if _, err := a.db.Exec(`
			INSERT INTO favorites (family_id, name, quantity) VALUES (?, ?, ?)
			ON CONFLICT (family_id, name) DO UPDATE SET quantity = excluded.quantity`,
			u.FamilyID, f.Name, f.Quantity); err != nil {
			a.serverError(w, err)
			return
		}
	}
	http.Redirect(w, r, "/shopping/favorites", http.StatusSeeOther)
}

func (a *App) deleteFavorite(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := a.db.Exec(`DELETE FROM favorites WHERE id = ? AND family_id = ?`, id, u.FamilyID); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/shopping/favorites", http.StatusSeeOther)
}

// Copies favorites onto the shopping list, skipping ones already on it.
const copyFavoritesSQL = `
	INSERT INTO shopping_items (family_id, name, quantity, added_by)
	SELECT f.family_id, f.name, f.quantity, ? FROM favorites f
	WHERE f.family_id = ? AND NOT EXISTS (
		SELECT 1 FROM shopping_items i
		WHERE i.family_id = f.family_id AND i.done = 0 AND i.name = f.name COLLATE NOCASE)`

func (a *App) addFavoriteToList(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := a.db.Exec(copyFavoritesSQL+` AND f.id = ?`, u.ID, u.FamilyID, id); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}

func (a *App) addAllFavoritesToList(w http.ResponseWriter, r *http.Request, u *User) {
	if _, err := a.db.Exec(copyFavoritesSQL+` ORDER BY f.id`, u.ID, u.FamilyID); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}
