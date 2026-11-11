package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// To-do, documents and swim endpoints of the JSON API.

// ---- to-dos ----

func (a *App) apiTodos(w http.ResponseWriter, r *http.Request, u *User) {
	todos, err := a.loadTodos(u.FamilyID, 0)
	if err != nil {
		apiServerError(w, err)
		return
	}
	out := make([]apiTodo, 0, len(todos))
	done := 0
	for _, t := range todos {
		if t.Done {
			done++
		}
		out = append(out, toAPITodo(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"todos": out, "open_count": len(todos) - done, "done_count": done})
}

func (a *App) writeTodo(w http.ResponseWriter, status int, u *User, id int64) {
	todos, err := a.loadTodos(u.FamilyID, id)
	if err != nil {
		apiServerError(w, err)
		return
	}
	if len(todos) == 0 {
		apiError(w, http.StatusNotFound, "That to-do no longer exists.")
		return
	}
	writeJSON(w, status, toAPITodo(todos[0]))
}

func (a *App) apiAddTodo(w http.ResponseWriter, r *http.Request, u *User) {
	var body struct {
		Title string `json:"title"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	title := limit(body.Title, 200)
	if title == "" {
		apiError(w, http.StatusUnprocessableEntity, "Please enter a to-do.")
		return
	}
	res, err := a.db.Exec(`INSERT INTO todos (family_id, title, added_by) VALUES (?, ?, ?)`, u.FamilyID, title, u.ID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	id, _ := res.LastInsertId()
	a.writeTodo(w, http.StatusCreated, u, id)
}

func (a *App) apiUpdateTodo(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Title *string `json:"title"`
		Done  *bool   `json:"done"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var title *string
	if body.Title != nil {
		t := limit(*body.Title, 200)
		if t == "" {
			apiError(w, http.StatusUnprocessableEntity, "The to-do can't be empty.")
			return
		}
		title = &t
	}
	res, err := a.db.Exec(`UPDATE todos SET title = COALESCE(?, title), done = COALESCE(?, done)
		WHERE id = ? AND family_id = ?`, title, body.Done, id, u.FamilyID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		apiError(w, http.StatusNotFound, "That to-do no longer exists.")
		return
	}
	a.writeTodo(w, http.StatusOK, u, id)
}

func (a *App) apiDeleteTodo(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := a.db.Exec(`DELETE FROM todos WHERE id = ? AND family_id = ?`, id, u.FamilyID); err != nil {
		apiServerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) apiClearDoneTodos(w http.ResponseWriter, r *http.Request, u *User) {
	res, err := a.db.Exec(`DELETE FROM todos WHERE done = 1 AND family_id = ?`, u.FamilyID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	n, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": n})
}

// ---- documents ----

func (a *App) apiDocuments(w http.ResponseWriter, r *http.Request, u *User) {
	docs, err := a.loadDocuments(u.FamilyID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	out := make([]apiDocument, 0, len(docs))
	for _, d := range docs {
		out = append(out, toAPIDocument(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": out})
}

// apiUploadDocuments takes a multipart form with one or more "file" fields.
// Files before a failing one stay saved; the error says which file failed.
func (a *App) apiUploadDocuments(w http.ResponseWriter, r *http.Request, u *User) {
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	mr, err := r.MultipartReader()
	if err != nil {
		apiError(w, http.StatusBadRequest, `Send the files as multipart form fields named "file".`)
		return
	}
	saved := []apiDocument{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			apiUploadFailed(w, err)
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		d, err := a.saveDocument(part, u)
		part.Close()
		if err != nil {
			apiUploadFailed(w, err)
			return
		}
		saved = append(saved, toAPIDocument(d))
	}
	if len(saved) == 0 {
		apiError(w, http.StatusBadRequest, "Choose a file to upload.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"documents": saved})
}

func apiUploadFailed(w http.ResponseWriter, err error) {
	var ue uploadError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &ue):
		apiError(w, http.StatusUnprocessableEntity, ue.Error())
	case errors.As(err, &tooBig):
		apiError(w, http.StatusRequestEntityTooLarge, "The upload is too large (100 MB max at once).")
	default:
		log.Printf("upload: %v", err)
		apiError(w, http.StatusBadRequest, "The upload failed. Please try again.")
	}
}

func (a *App) apiRenameDocument(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var current string
	if err := a.db.QueryRow(`SELECT name FROM documents WHERE id = ? AND family_id = ?`, id, u.FamilyID).Scan(&current); err != nil {
		apiError(w, http.StatusNotFound, "That document no longer exists.")
		return
	}
	if _, err := a.db.Exec(`UPDATE documents SET name = ? WHERE id = ? AND family_id = ?`,
		renamedDocument(current, body.Name), id, u.FamilyID); err != nil {
		apiServerError(w, err)
		return
	}
	docs, err := a.loadDocuments(u.FamilyID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	for _, d := range docs {
		if d.ID == id {
			writeJSON(w, http.StatusOK, toAPIDocument(d))
			return
		}
	}
	apiError(w, http.StatusNotFound, "That document no longer exists.")
}

func (a *App) apiDeleteDocument(w http.ResponseWriter, r *http.Request, u *User) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var stored string
	err := a.db.QueryRow(`DELETE FROM documents WHERE id = ? AND family_id = ? RETURNING stored_name`, id, u.FamilyID).Scan(&stored)
	if err == nil {
		if err := os.Remove(filepath.Join(a.uploadsDir, stored)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("delete document file: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- swim ----

func (a *App) apiSwim(w http.ResponseWriter, r *http.Request, u *User) {
	data, err := a.loadSwim(u.FamilyID)
	if err != nil {
		apiServerError(w, err)
		return
	}
	type class struct {
		Number int        `json:"number"`
		Done   bool       `json:"done"`
		DoneAt *time.Time `json:"done_at"`
		DoneBy string     `json:"done_by"`
	}
	classes := make([]class, 0, len(data.Classes))
	var next *int
	for _, c := range data.Classes {
		out := class{Number: c.Number, Done: c.Done, DoneBy: c.DoneBy}
		if c.Done {
			t := c.DoneAt.UTC()
			out.DoneAt = &t
		}
		if c.Next {
			n := c.Number
			next = &n
		}
		classes = append(classes, out)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": data.Total(), "done_count": data.DoneCount, "next": next, "classes": classes,
	})
}

func (a *App) apiSetSwimClass(w http.ResponseWriter, r *http.Request, u *User) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > swimClassCount {
		apiError(w, http.StatusNotFound, "No such class.")
		return
	}
	var body struct {
		Done *bool `json:"done"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Done == nil {
		apiError(w, http.StatusUnprocessableEntity, `Send {"done": true} or {"done": false}.`)
		return
	}
	if *body.Done {
		_, err = a.db.Exec(`INSERT INTO swim_classes (family_id, number, done_by) VALUES (?, ?, ?)
			ON CONFLICT (family_id, number) DO NOTHING`, u.FamilyID, n, u.ID)
	} else {
		_, err = a.db.Exec(`DELETE FROM swim_classes WHERE family_id = ? AND number = ?`, u.FamilyID, n)
	}
	if err != nil {
		apiServerError(w, err)
		return
	}
	a.apiSwim(w, r, u)
}

func (a *App) apiResetSwim(w http.ResponseWriter, r *http.Request, u *User) {
	if _, err := a.db.Exec(`DELETE FROM swim_classes WHERE family_id = ?`, u.FamilyID); err != nil {
		apiServerError(w, err)
		return
	}
	a.apiSwim(w, r, u)
}
