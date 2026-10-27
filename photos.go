package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Optional photos of shopping items. Files live in <uploads>/photos under
// random names; shopping_items.photo and shopping_trip_items.photo refer to them.

const maxPhotoSize = 10 << 20 // phones shrink photos before upload; this is for the no-JS fallback

var photoName = regexp.MustCompile(`^[0-9a-f]{32}\.(jpg|png|webp)$`)

var photoTypes = map[string]string{".jpg": "image/jpeg", ".png": "image/png", ".webp": "image/webp"}

var errBadPhoto = errors.New("the photo must be a JPEG, PNG or WebP image up to 10 MB")

func (a *App) photosDir() string { return filepath.Join(a.uploadsDir, "photos") }

// savePhoto stores an uploaded image and returns its file name.
func (a *App) savePhoto(src io.Reader) (string, error) {
	br := bufio.NewReader(src)
	head, _ := br.Peek(12)
	var ext string
	switch {
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		ext = ".jpg"
	case bytes.HasPrefix(head, []byte("\x89PNG")):
		ext = ".png"
	case len(head) == 12 && bytes.HasPrefix(head, []byte("RIFF")) && string(head[8:12]) == "WEBP":
		ext = ".webp"
	default:
		return "", errBadPhoto
	}

	if err := os.MkdirAll(a.photosDir(), 0o700); err != nil {
		return "", err
	}
	name := randomHex(16) + ext
	path := filepath.Join(a.photosDir(), name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(br, maxPhotoSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxPhotoSize {
		err = errBadPhoto
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return name, nil
}

// removePhotoIfUnused deletes a photo file once no item or history entry uses it.
func (a *App) removePhotoIfUnused(name string) {
	if name == "" || !photoName.MatchString(name) {
		return
	}
	var used bool
	if err := a.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM shopping_items WHERE photo = ?)
		OR EXISTS (SELECT 1 FROM shopping_trip_items WHERE photo = ?)`, name, name).Scan(&used); err != nil || used {
		return
	}
	if err := os.Remove(filepath.Join(a.photosDir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("remove photo: %v", err)
	}
}

func (a *App) servePhoto(w http.ResponseWriter, r *http.Request, u *User) {
	name := r.PathValue("name")
	if !photoName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	// Only photos of the user's own family.
	var ok bool
	if err := a.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM shopping_items WHERE photo = ? AND family_id = ?)
		OR EXISTS (SELECT 1 FROM shopping_trip_items i JOIN shopping_trips t ON t.id = i.trip_id
		           WHERE i.photo = ? AND t.family_id = ?)`, name, u.FamilyID, name, u.FamilyID).Scan(&ok); err != nil {
		a.serverError(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(a.photosDir(), name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", photoTypes[filepath.Ext(name)])
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "private, max-age=604800, immutable") // names are random and never reused
	http.ServeContent(w, r, "", time.Time{}, f)
}

func photoURL(name string) string { return "/shopping/photos/" + name }
