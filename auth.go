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
	id, err := a.authenticate(email, r.FormValue("password"))
	if errors.Is(err, errWrongLogin) {
		a.render(w, http.StatusUnauthorized, "login", Page{
			Title: "Log in",
			Error: "Wrong email or password.",
			Data:  map[string]string{"email": email},
		})
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}

	if err := a.startSession(w, id); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}

var errWrongLogin = errors.New("wrong email or password")

// authenticate returns the id of the user with this email and password, or errWrongLogin.
func (a *App) authenticate(email, password string) (int64, error) {
	var id int64
	var hash []byte
	err := a.db.QueryRow(`SELECT id, password_hash FROM users WHERE email = ?`, normalizeEmail(email)).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return 0, errWrongLogin
	} else if err != nil {
		return 0, err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		return 0, errWrongLogin
	}
	return id, nil
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
	invite := strings.ToUpper(strings.TrimSpace(r.FormValue("invite")))

	userID, err := a.createAccount(email, name, r.FormValue("password"), invite)
	var fe formError
	if errors.As(err, &fe) {
		first, _ := a.noUsersYet(a.db)
		a.render(w, http.StatusUnprocessableEntity, "register", Page{
			Title: "Create account",
			Error: fe.Error(),
			Data:  registerForm(email, name, invite, first),
		})
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}

	if err := a.startSession(w, userID); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/shopping", http.StatusSeeOther)
}

// formError is a problem with what the user entered, shown to them as-is.
type formError string

func (e formError) Error() string { return string(e) }

// createAccount checks the details and creates the user, joining the family
// of the invite code (or starting the first family on an empty database).
// Mistakes in the input come back as formError.
func (a *App) createAccount(email, name, password, invite string) (int64, error) {
	email = normalizeEmail(email)
	name = strings.TrimSpace(name)
	invite = strings.ToUpper(strings.TrimSpace(invite))

	switch {
	case !validEmail(email):
		return 0, formError("Please enter a valid email address.")
	case name == "" || len(name) > 50:
		return 0, formError("Please enter your name (up to 50 characters).")
	case len(password) < 8:
		return 0, formError("Password must be at least 8 characters.")
	case len(password) > 72: // bcrypt limit
		return 0, formError("Password must be at most 72 characters.")
	}

	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	first, err := a.noUsersYet(tx)
	if err != nil {
		return 0, err
	}

	// The invite is checked before anything else, so without one nobody can
	// even find out which emails have accounts.
	var familyID int64
	switch {
	case invite != "":
		err := tx.QueryRow(`SELECT id FROM families WHERE invite_code = ?`, invite).Scan(&familyID)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, formError("That invite code doesn't match any family.")
		} else if err != nil {
			return 0, err
		}
	case !first:
		return 0, formError("You need an invite code from a family member to create an account.")
	}

	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE email = ?)`, email).Scan(&exists); err != nil {
		return 0, err
	}
	if exists {
		return 0, formError("An account with this email already exists.")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}

	if familyID == 0 { // first user: start a new family
		res, err := tx.Exec(`INSERT INTO families (name, invite_code) VALUES (?, ?)`, name+"'s family", newInviteCode())
		if err != nil {
			return 0, err
		}
		familyID, _ = res.LastInsertId()
	}

	res, err := tx.Exec(`INSERT INTO users (email, name, password_hash, family_id) VALUES (?, ?, ?, ?)`,
		email, name, string(hash), familyID)
	if err != nil {
		return 0, err
	}
	userID, _ := res.LastInsertId()
	return userID, tx.Commit()
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
	token, expires, err := a.newSession(userID, sessionTTL)
	if err != nil {
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

// newSession stores a new random session token (only its hash) and returns it.
func (a *App) newSession(userID int64, ttl time.Duration) (token string, expires time.Time, err error) {
	b := make([]byte, 32)
	rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	expires = time.Now().Add(ttl)

	a.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	if _, err := a.db.Exec(`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(token), userID, expires.Unix()); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// currentUser returns the logged-in user, or nil if there is no valid session.
func (a *App) currentUser(r *http.Request) (*User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, nil
	}
	return a.sessionUser(c.Value)
}

// sessionUser returns the user a session token belongs to, or nil if it is unknown or expired.
func (a *App) sessionUser(token string) (*User, error) {
	var u User
	err := a.db.QueryRow(`
		SELECT u.id, u.email, u.name, u.family_id
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`,
		hashToken(token), time.Now().Unix(),
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
