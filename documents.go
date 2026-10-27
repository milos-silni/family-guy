package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxDocumentSize = 20 << 20  // per file
	maxUploadBody   = 100 << 20 // per request (several files at once)
)

// Allowed document types: extension -> Content-Type used for downloads.
var documentTypes = map[string]string{
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".odt":  "application/vnd.oasis.opendocument.text",
	".rtf":  "application/rtf",
	".txt":  "text/plain; charset=utf-8",
}

// Leading bytes each binary format must start with, to reject renamed files.
var documentMagic = map[string][]byte{
	".pdf":  []byte("%PDF-"),
	".doc":  {0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1},
	".docx": []byte("PK\x03\x04"),
	".odt":  []byte("PK\x03\x04"),
	".rtf":  []byte(`{\rtf`),
}

type Document struct {
	ID        int64
	Name      string
	Size      int64
	AddedBy   string
	CreatedAt time.Time
}

// Base is the name without extension, Ext the extension (".pdf").
func (d Document) Base() string { return strings.TrimSuffix(d.Name, d.Ext()) }
func (d Document) Ext() string  { return filepath.Ext(d.Name) }
func (d Document) Kind() string { return strings.ToUpper(strings.TrimPrefix(d.Ext(), ".")) }

type DocumentsData struct {
	Items  []Document
	EditID int64
}

// uploadError is a problem with the user's file, shown to them as-is.
type uploadError string

func (e uploadError) Error() string { return string(e) }

func (a *App) documentsPage(w http.ResponseWriter, r *http.Request, u *User) {
	editID, _ := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64)
	a.renderDocuments(w, http.StatusOK, u, editID, "")
}

func (a *App) renderDocuments(w http.ResponseWriter, status int, u *User, editID int64, errMsg string) {
	rows, err := a.db.Query(`
		SELECT d.id, d.name, d.size, COALESCE(u.name, ''), d.created_at
		FROM documents d LEFT JOIN users u ON u.id = d.added_by
		WHERE d.family_id = ?
		ORDER BY d.id DESC`, u.FamilyID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer rows.Close()

	data := DocumentsData{EditID: editID}
	for rows.Next() {
		var d Document
		var created int64
		if err := rows.Scan(&d.ID, &d.Name, &d.Size, &d.AddedBy, &created); err != nil {
			a.serverError(w, err)
			return
		}
		d.CreatedAt = time.Unix(created, 0)
		data.Items = append(data.Items, d)
	}
	if err := rows.Err(); err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, status, "documents", Page{Title: "Documents", User: u, Tab: "documents", Error: errMsg, Data: data})
}

func (a *App) uploadDocuments(w http.ResponseWriter, r *http.Request, u *User) {
	// Uploads from a phone can take a while; lift the server-wide read timeout.
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)

	mr, err := r.MultipartReader()
	if err != nil {
		a.renderDocuments(w, http.StatusBadRequest, u, 0, "Choose a file to upload.")
		return
	}

	uploaded := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			a.uploadFailed(w, u, err)
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		err = a.saveDocument(part, u)
		part.Close()
		if err != nil {
			a.uploadFailed(w, u, err)
			return
		}
		uploaded++
	}
	if uploaded == 0 {
		a.renderDocuments(w, http.StatusBadRequest, u, 0, "Choose a file to upload.")
		return
	}
	http.Redirect(w, r, "/documents", http.StatusSeeOther)
}

func (a *App) uploadFailed(w http.ResponseWriter, u *User, err error) {
	var ue uploadError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &ue):
		a.renderDocuments(w, http.StatusUnprocessableEntity, u, 0, ue.Error())
	case errors.As(err, &tooBig):
		a.renderDocuments(w, http.StatusRequestEntityTooLarge, u, 0, "The upload is too large (100 MB max at once).")
	default:
		log.Printf("upload: %v", err)
		a.renderDocuments(w, http.StatusBadRequest, u, 0, "The upload failed. Please try again.")
	}
}

// saveDocument streams one uploaded file to disk and records it in the database.
func (a *App) saveDocument(part *multipart.Part, u *User) error {
	name := cleanFileName(part.FileName())
	ext := strings.ToLower(filepath.Ext(name))
	if _, ok := documentTypes[ext]; !ok {
		return uploadError(name + ": only PDF, Word (DOC, DOCX), ODT, RTF and TXT files are allowed.")
	}

	br := bufio.NewReader(part)
	if magic, ok := documentMagic[ext]; ok {
		head, _ := br.Peek(len(magic))
		if !bytes.Equal(head, magic) {
			return uploadError(name + " doesn't look like a real " + strings.ToUpper(ext[1:]) + " file.")
		}
	}

	// Stored under a random name, never the user's, so it can't escape the uploads directory.
	stored := randomHex(16)
	path := filepath.Join(a.uploadsDir, stored)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	saved := false
	defer func() {
		if !saved {
			f.Close()
			os.Remove(path)
		}
	}()

	n, err := io.Copy(f, io.LimitReader(br, maxDocumentSize+1))
	if err != nil {
		return err
	}
	if n > maxDocumentSize {
		return uploadError(fmt.Sprintf("%s is larger than %d MB.", name, maxDocumentSize>>20))
	}
	if n == 0 {
		return uploadError(name + " is empty.")
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := a.db.Exec(`INSERT INTO documents (family_id, name, stored_name, size, added_by) VALUES (?, ?, ?, ?, ?)`,
		u.FamilyID, name, stored, n, u.ID); err != nil {
		return err
	}
	saved = true
	return nil
}

func (a *App) downloadDocument(w http.ResponseWriter, r *http.Request, u *User) {
	name, stored, created, ok := a.findDocument(w, r, u)
	if !ok {
		return
	}
	f, err := os.Open(filepath.Join(a.uploadsDir, stored))
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	defer f.Close()

	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))
	ctype := documentTypes[strings.ToLower(filepath.Ext(name))]
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if disposition == "" {
		disposition = "attachment"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", disposition)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name, created, f)
}

func (a *App) renameDocument(w http.ResponseWriter, r *http.Request, u *User) {
	current, _, _, ok := a.findDocument(w, r, u)
	if !ok {
		return
	}
	// The extension is kept as-is so the file still opens with the right app.
	ext := filepath.Ext(current)
	input := strings.TrimSpace(r.FormValue("name"))
	if strings.HasSuffix(strings.ToLower(input), ext) {
		input = strings.TrimSpace(input[:len(input)-len(ext)]) // user typed the extension too
	}
	if input != "" {
		base := strings.TrimSuffix(cleanFileName(input+ext), ext)
		if _, err := a.db.Exec(`UPDATE documents SET name = ? WHERE id = ? AND family_id = ?`,
			base+ext, r.PathValue("id"), u.FamilyID); err != nil {
			a.serverError(w, err)
			return
		}
	}
	http.Redirect(w, r, "/documents", http.StatusSeeOther)
}

func (a *App) deleteDocument(w http.ResponseWriter, r *http.Request, u *User) {
	_, stored, _, ok := a.findDocument(w, r, u)
	if !ok {
		return
	}
	if _, err := a.db.Exec(`DELETE FROM documents WHERE id = ? AND family_id = ?`, r.PathValue("id"), u.FamilyID); err != nil {
		a.serverError(w, err)
		return
	}
	if err := os.Remove(filepath.Join(a.uploadsDir, stored)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("delete document file: %v", err)
	}
	http.Redirect(w, r, "/documents", http.StatusSeeOther)
}

// findDocument loads the {id} document of the user's family, writing a 404 if there is none.
func (a *App) findDocument(w http.ResponseWriter, r *http.Request, u *User) (name, stored string, created time.Time, ok bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var unix int64
	err = a.db.QueryRow(`SELECT name, stored_name, created_at FROM documents WHERE id = ? AND family_id = ?`, id, u.FamilyID).
		Scan(&name, &stored, &unix)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	return name, stored, time.Unix(unix, 0), true
}

// cleanFileName drops path separators and control characters and caps the length.
func cleanFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	ext := filepath.Ext(s)
	base := limit(strings.TrimSuffix(s, ext), 120)
	if base == "" {
		base = "document"
	}
	return base + strings.ToLower(ext)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
