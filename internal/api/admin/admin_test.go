package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdayat/go-template/internal/api/handler"
	"github.com/mdayat/go-template/internal/user"
)

// Only an admin gets through; anyone else gets a 403 and the route never runs.
func TestRequireAdmin(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		admin      bool
		wantStatus int
	}{
		{"an admin", true, http.StatusOK},
		{"a user", false, http.StatusForbidden},
	} {
		ran := false
		route := RequireAdmin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
		req := handler.SignedIn(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/x", nil),
			user.User{IsAdmin: tt.admin})
		rec := httptest.NewRecorder()

		route.ServeHTTP(rec, req)

		if rec.Code != tt.wantStatus || ran != tt.admin {
			t.Errorf("%s: status %d, route ran %t; want %d, ran %t", tt.name, rec.Code, ran, tt.wantStatus, tt.admin)
		}
	}
}
