// Package admin serves admins, under /admin: every route needs a signed-in admin (RequireAdmin,
// which the router puts before them). The admin app is deployed with the API, so these routes
// change with it, renames in two releases as for the web app.
package admin

import (
	"net/http"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/apperr"
)

// RequireAdmin lets through only admins: every admin route acts on everyone's data. Anyone else
// gets a 403 (the routes are no secret).
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !handler.CurrentUser(r).IsAdmin {
			handler.WriteError(w, r, apperr.Forbidden("only admins may use this"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
