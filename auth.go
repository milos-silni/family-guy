package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "fg_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type User struct {
	ID       int64
	Email    string
	Name     string
	FamilyID int64
}

// Used to keep login timing the same whether or not the email exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.DefaultCost)

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if u, _ := a.currentUser(r); u != nil {
		http.Redirect(w, r, "/shopping", http.StatusSeeOther)
		return
	}
	a.render(w, http.StatusOK, "login", Page{Title: "Log in", Data: map[string]string{}})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	email := normalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")

	var id int64
	var hash []byte
	err := a.db.QueryRow(`SELECT id, password_hash FROM users WHERE email = ?`, email).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	if err != nil || bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		a.render(w, http.StatusUnauthorized, "login", Page{
			Title: "Log in",
			Error: "Wrong email or password.",
			Data:  map[string]string{"email": email},
		})
		return
	}

	if err := a.startSession(w, id); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}

func (a *App) registerPage(w http.ResponseWriter, r *http.Request) {
	if u, _ := a.currentUser(r); u != nil {
		http.Redirect(w, r, "/shopping", http.StatusSeeOther)
		return
	}
	first, err := a.noUsersYet(a.db)
	if err != nil {
		a.serverError(w, err)
		return
	}
	// Invite links look like /register?invite=ABCD2345.
	a.render(w, http.StatusOK, "register", Page{
		Title: "Create account",
		Data:  registerForm("", "", strings.ToUpper(r.URL.Query().Get("invite")), first),
	})
}

func registerForm(email, name, invite string, first bool) map[string]string {
	f := map[string]string{"email": email, "name": name, "invite": invite}
	if first {
		f["first"] = "1"
	}
	return f
}

// noUsersYet reports whether the database is empty. Only then may someone
// register without an invite code, to create the first family.
func (a *App) noUsersYet(q interface {
	QueryRow(string, ...any) *sql.Row
}) (bool, error) {
	var found bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM users)`).Scan(&found)
	return !found, err
}

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	email := normalizeEmail(r.FormValue("email"))
	name := strings.TrimSpace(r.FormValue("name"))
	password := r.FormValue("password")
	invite := strings.ToUpper(strings.TrimSpace(r.FormValue("invite")))

	first := false
	fail := func(msg string) {
		a.render(w, http.StatusUnprocessableEntity, "register", Page{
			Title: "Create account",
			Error: msg,
			Data:  registerForm(email, name, invite, first),
		})
	}

	switch {
	case !validEmail(email):
		fail("Please enter a valid email address.")
		return
	case name == "" || len(name) > 50:
		fail("Please enter your name (up to 50 characters).")
		return
	case len(password) < 8:
		fail("Password must be at least 8 characters.")
		return
	case len(password) > 72: // bcrypt limit
		fail("Password must be at most 72 characters.")
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer tx.Rollback()

	if first, err = a.noUsersYet(tx); err != nil {
		a.serverError(w, err)
		return
	}

	// The invite is checked before anything else, so without one nobody can
	// even find out which emails have accounts.
	var familyID int64
	switch {
	case invite != "":
		err := tx.QueryRow(`SELECT id FROM families WHERE invite_code = ?`, invite).Scan(&familyID)
		if errors.Is(err, sql.ErrNoRows) {
			fail("That invite code doesn't match any family.")
			return
		} else if err != nil {
			a.serverError(w, err)
			return
		}
	case !first:
		fail("You need an invite code from a family member to create an account.")
		return
	}

	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE email = ?)`, email).Scan(&exists); err != nil {
		a.serverError(w, err)
		return
	}
	if exists {
		fail("An account with this email already exists.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		a.serverError(w, err)
		return
	}

	if familyID == 0 { // first user: start a new family
		res, err := tx.Exec(`INSERT INTO families (name, invite_code) VALUES (?, ?)`, name+"'s family", newInviteCode())
		if err != nil {
			a.serverError(w, err)
			return
		}
		familyID, _ = res.LastInsertId()
	}

	res, err := tx.Exec(`INSERT INTO users (email, name, password_hash, family_id) VALUES (?, ?, ?, ?)`,
		email, name, string(hash), familyID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	userID, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		a.serverError(w, err)
		return
	}

	if err := a.startSession(w, userID); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request, u *User) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// startSession stores a random token (hashed) and sets it as a cookie.
func (a *App) startSession(w http.ResponseWriter, userID int64) error {
	b := make([]byte, 32)
	rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	expires := time.Now().Add(sessionTTL)

	a.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	if _, err := a.db.Exec(`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(token), userID, expires.Unix()); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// currentUser returns the logged-in user, or nil if there is no valid session.
func (a *App) currentUser(r *http.Request) (*User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, nil
	}
	var u User
	err = a.db.QueryRow(`
		SELECT u.id, u.email, u.name, u.family_id
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`,
		hashToken(c.Value), time.Now().Unix(),
	).Scan(&u.ID, &u.Email, &u.Name, &u.FamilyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (a *App) requireUser(h func(http.ResponseWriter, *http.Request, *User)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.currentUser(r)
		if err != nil {
			a.serverError(w, err)
			return
		}
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r, u)
	})
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func validEmail(s string) bool {
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s && len(s) <= 254
}

// newInviteCode returns 8 characters without look-alikes (0/O, 1/I).
func newInviteCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
