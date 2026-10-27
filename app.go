package main

import (
	"bytes"
	"database/sql"
	"html/template"
	"log"
	"net/http"
	"time"
)

type App struct {
	db         *sql.DB
	uploadsDir string
	secure     bool
	pages      map[string]*template.Template

	vapidPublic, vapidPrivate string // Web Push keys
}

// Page is the data passed to every template.
type Page struct {
	Title string
	User  *User
	Tab   string
	Error string
	Data  any
}

var templateFuncs = template.FuncMap{
	"asset":     asset,
	"photoURL":  photoURL,
	"humanSize": humanSize,
	"date":      func(t time.Time) string { return t.Format("2 Jan 2006") },
	"shortDate": func(t time.Time) string { return t.Format("2 Jan") },
	"tripDate":  func(t time.Time) string { return t.Format("Mon 2 Jan 2006, 15:04") },
}

func newApp(db *sql.DB, uploadsDir string, secure bool) (*App, error) {
	a := &App{db: db, uploadsDir: uploadsDir, secure: secure, pages: map[string]*template.Template{}}
	for _, name := range []string{"login", "register", "shopping", "favorites", "history", "todos", "documents", "swim", "family"} {
		t, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		a.pages[name] = t
	}
	var err error
	if a.vapidPublic, a.vapidPrivate, err = loadVAPIDKeys(db); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /favicon.ico", serveFavicon)
	mux.HandleFunc("GET /sw.js", serveServiceWorker)

	mux.Handle("GET /push/key", a.requireUser(a.pushKey))
	mux.Handle("POST /push/subscribe", a.requireUser(a.pushSubscribe))
	mux.Handle("POST /push/unsubscribe", a.requireUser(a.pushUnsubscribe))
	mux.Handle("POST /push/notify/{user}", a.requireUser(a.notifyShopping))

	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("GET /register", a.registerPage)
	mux.HandleFunc("POST /register", a.register)
	mux.Handle("POST /logout", a.requireUser(a.logout))

	mux.Handle("GET /{$}", a.requireUser(func(w http.ResponseWriter, r *http.Request, u *User) {
		http.Redirect(w, r, "/shopping", http.StatusSeeOther)
	}))

	mux.Handle("GET /shopping", a.requireUser(a.shoppingPage))
	mux.Handle("POST /shopping", a.requireUser(a.addShoppingItem))
	mux.Handle("POST /shopping/{id}/edit", a.requireUser(a.editShoppingItem))
	mux.Handle("POST /shopping/{id}/toggle", a.requireUser(a.toggleBought))
	mux.Handle("POST /shopping/{id}/not-found", a.requireUser(a.toggleNotFound))
	mux.Handle("POST /shopping/{id}/delete", a.requireUser(a.deleteShoppingItem))
	mux.Handle("GET /shopping/photos/{name}", a.requireUser(a.servePhoto))
	mux.Handle("GET /shopping/favorites", a.requireUser(a.favoritesPage))
	mux.Handle("POST /shopping/favorites", a.requireUser(a.addFavorites))
	mux.Handle("POST /shopping/favorites/{id}/delete", a.requireUser(a.deleteFavorite))
	mux.Handle("POST /shopping/favorites/{id}/add", a.requireUser(a.addFavoriteToList))
	mux.Handle("POST /shopping/favorites/add-all", a.requireUser(a.addAllFavoritesToList))
	mux.Handle("POST /shopping/finish", a.requireUser(a.finishShopping))
	mux.Handle("GET /shopping/history", a.requireUser(a.historyPage))
	mux.Handle("POST /shopping/history/items/{id}/add", a.requireUser(a.addTripItemToList))

	mux.Handle("GET /todos", a.requireUser(a.todosPage))
	mux.Handle("POST /todos", a.requireUser(a.addTodo))
	mux.Handle("POST /todos/{id}/toggle", a.requireUser(a.toggle("todos", "/todos")))
	mux.Handle("POST /todos/{id}/delete", a.requireUser(a.remove("todos", "/todos")))
	mux.Handle("POST /todos/clear", a.requireUser(a.clearDone("todos", "/todos")))

	mux.Handle("GET /documents", a.requireUser(a.documentsPage))
	mux.Handle("POST /documents", a.requireUser(a.uploadDocuments))
	mux.Handle("GET /documents/{id}/download", a.requireUser(a.downloadDocument))
	mux.Handle("POST /documents/{id}/rename", a.requireUser(a.renameDocument))
	mux.Handle("POST /documents/{id}/delete", a.requireUser(a.deleteDocument))

	mux.Handle("GET /swim", a.requireUser(a.swimPage))
	mux.Handle("POST /swim/{n}/toggle", a.requireUser(a.toggleSwimClass))
	mux.Handle("POST /swim/reset", a.requireUser(a.resetSwimClasses))

	mux.Handle("GET /family", a.requireUser(a.familyPage))

	// Rejects cross-site POSTs (CSRF) using Sec-Fetch-Site / Origin headers.
	return http.NewCrossOriginProtection().Handler(logRequests(mux))
}

func (a *App) render(w http.ResponseWriter, status int, name string, p Page) {
	var buf bytes.Buffer
	if err := a.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		a.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "Something went wrong. Please try again.", http.StatusInternalServerError)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Microsecond))
	})
}
