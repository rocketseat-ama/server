package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rocketseat-ama/server/internal/store/pgstore"
)

type apiHandler struct {
	q *pgstore.Queries // TODO: should be abstracted using an interface
	r *chi.Mux         // package for creating HTTP routers
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	a := apiHandler{
		q: q,
		r: chi.NewRouter(),
	}

	return a
}
