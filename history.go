package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// A shopping trip is saved when someone taps "Finish shopping": the bought
// items move from the list into the trip.

type Trip struct {
	ID         int64
	FinishedAt time.Time
	FinishedBy string
	Bought     []TripItem
	NotFound   []TripItem
}

type TripItem struct {
	ID       int64
	Name     string
	Quantity string
	Photo    string
	OnList   bool // already on the shopping list (not bought yet)
}

const historyTrips = 50 // how many recent trips the history page shows

func (a *App) finishShopping(w http.ResponseWriter, r *http.Request, u *User) {
	bought, err := a.finishTrip(u)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if bought == 0 {
		http.Redirect(w, r, "/shopping", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/shopping?finished=%d", bought), http.StatusSeeOther)
}

// finishTrip saves bought and not-found items as a trip and returns how many
// were bought. Nothing happens when nothing is bought.
func (a *App) finishTrip(u *User) (bought int, err error) {
	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if err := tx.QueryRow(`SELECT COUNT(*) FROM shopping_items WHERE family_id = ? AND done = 1`, u.FamilyID).Scan(&bought); err != nil {
		return 0, err
	}
	if bought == 0 {
		return 0, nil
	}

	res, err := tx.Exec(`INSERT INTO shopping_trips (family_id, finished_by) VALUES (?, ?)`, u.FamilyID, u.ID)
	if err != nil {
		return 0, err
	}
	tripID, _ := res.LastInsertId()

	// Bought and not-found items are recorded in the trip.
	_, err = tx.Exec(`
		INSERT INTO shopping_trip_items (trip_id, name, quantity, status, photo)
		SELECT ?, name, quantity, CASE WHEN done = 1 THEN 'bought' ELSE 'not_found' END, photo
		FROM shopping_items WHERE family_id = ? AND (done = 1 OR not_found = 1) ORDER BY id`, tripID, u.FamilyID)
	if err == nil {
		// Bought items leave the list.
		_, err = tx.Exec(`DELETE FROM shopping_items WHERE family_id = ? AND done = 1`, u.FamilyID)
	}
	if err == nil {
		// Not-found items stay, to be looked for again next time.
		_, err = tx.Exec(`UPDATE shopping_items SET not_found = 0 WHERE family_id = ? AND not_found = 1`, u.FamilyID)
	}
	if err != nil {
		return 0, err
	}
	return bought, tx.Commit()
}

func (a *App) historyPage(w http.ResponseWriter, r *http.Request, u *User) {
	trips, err := a.loadTrips(u.FamilyID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, http.StatusOK, "history", Page{Title: "Shopping history", User: u, Tab: "shopping", Data: trips})
}

func (a *App) loadTrips(familyID int64) ([]*Trip, error) {
	rows, err := a.db.Query(`
		SELECT t.id, t.finished_at, COALESCE(u.name, ''),
		       i.id, i.name, i.quantity, i.status, i.photo,
		       EXISTS (SELECT 1 FROM shopping_items s
		               WHERE s.family_id = t.family_id AND s.done = 0 AND s.name = i.name COLLATE NOCASE)
		FROM (SELECT * FROM shopping_trips WHERE family_id = ? ORDER BY id DESC LIMIT ?) t
		LEFT JOIN users u ON u.id = t.finished_by
		JOIN shopping_trip_items i ON i.trip_id = t.id
		ORDER BY t.id DESC, i.id`, familyID, historyTrips)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trips []*Trip
	for rows.Next() {
		var tripID, finished int64
		var by, status string
		var it TripItem
		if err := rows.Scan(&tripID, &finished, &by, &it.ID, &it.Name, &it.Quantity, &status, &it.Photo, &it.OnList); err != nil {
			return nil, err
		}
		if len(trips) == 0 || trips[len(trips)-1].ID != tripID {
			trips = append(trips, &Trip{ID: tripID, FinishedAt: time.Unix(finished, 0), FinishedBy: by})
		}
		t := trips[len(trips)-1]
		if status == "bought" {
			t.Bought = append(t.Bought, it)
		} else {
			t.NotFound = append(t.NotFound, it)
		}
	}
	return trips, rows.Err()
}

// addTripItemToList puts an item from an earlier trip back on the shopping list.
func (a *App) addTripItemToList(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := a.copyTripItemToList(u, id); errors.Is(err, errNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/shopping/history", http.StatusSeeOther)
}

// copyTripItemToList adds trip item id to the list unless it is already on it
// (not bought yet). It reports whether it was added; errNotFound: no such item.
func (a *App) copyTripItemToList(u *User, id int64) (bool, error) {
	var name, qty, photo string
	err := a.db.QueryRow(`
		SELECT i.name, i.quantity, i.photo FROM shopping_trip_items i JOIN shopping_trips t ON t.id = i.trip_id
		WHERE i.id = ? AND t.family_id = ?`, id, u.FamilyID).Scan(&name, &qty, &photo)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errNotFound
	} else if err != nil {
		return false, err
	}
	res, err := a.db.Exec(`
		INSERT INTO shopping_items (family_id, name, quantity, photo, added_by)
		SELECT ?, ?, ?, ?, ? WHERE NOT EXISTS (
			SELECT 1 FROM shopping_items WHERE family_id = ? AND done = 0 AND name = ? COLLATE NOCASE)`,
		u.FamilyID, name, qty, photo, u.ID, u.FamilyID, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
