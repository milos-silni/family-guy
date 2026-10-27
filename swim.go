package main

import (
	"net/http"
	"strconv"
	"time"
)

const swimClassCount = 20

type SwimClass struct {
	Number int
	Done   bool
	DoneAt time.Time
	DoneBy string
	Next   bool // first class not done yet
}

type SwimData struct {
	Classes   []SwimClass
	DoneCount int
}

func (d SwimData) Total() int   { return len(d.Classes) }
func (d SwimData) Percent() int { return d.DoneCount * 100 / len(d.Classes) }

func (a *App) swimPage(w http.ResponseWriter, r *http.Request, u *User) {
	data := SwimData{Classes: make([]SwimClass, swimClassCount)}
	for i := range data.Classes {
		data.Classes[i].Number = i + 1
	}

	rows, err := a.db.Query(`
		SELECT s.number, s.done_at, COALESCE(u.name, '')
		FROM swim_classes s LEFT JOIN users u ON u.id = s.done_by
		WHERE s.family_id = ?`, u.FamilyID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		var doneAt int64
		var by string
		if err := rows.Scan(&n, &doneAt, &by); err != nil {
			a.serverError(w, err)
			return
		}
		if n < 1 || n > swimClassCount {
			continue
		}
		c := &data.Classes[n-1]
		c.Done, c.DoneAt, c.DoneBy = true, time.Unix(doneAt, 0), by
		data.DoneCount++
	}
	if err := rows.Err(); err != nil {
		a.serverError(w, err)
		return
	}
	for i := range data.Classes {
		if !data.Classes[i].Done {
			data.Classes[i].Next = true
			break
		}
	}
	a.render(w, http.StatusOK, "swim", Page{Title: "Kinder Schwimmschule", User: u, Tab: "swim", Data: data})
}

func (a *App) toggleSwimClass(w http.ResponseWriter, r *http.Request, u *User) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > swimClassCount {
		http.NotFound(w, r)
		return
	}
	res, err := a.db.Exec(`DELETE FROM swim_classes WHERE family_id = ? AND number = ?`, u.FamilyID, n)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if removed, _ := res.RowsAffected(); removed == 0 {
		if _, err := a.db.Exec(`INSERT INTO swim_classes (family_id, number, done_by) VALUES (?, ?, ?)`,
			u.FamilyID, n, u.ID); err != nil {
			a.serverError(w, err)
			return
		}
	}
	http.Redirect(w, r, "/swim", http.StatusSeeOther)
}

func (a *App) resetSwimClasses(w http.ResponseWriter, r *http.Request, u *User) {
	if _, err := a.db.Exec(`DELETE FROM swim_classes WHERE family_id = ?`, u.FamilyID); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/swim", http.StatusSeeOther)
}
