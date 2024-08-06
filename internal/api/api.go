package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rocketseat-ama/server/internal/store/pgstore"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/gorilla/websocket"
)

type apiHandler struct {
	// TODO: should be abstracted using an interface
	q 			*pgstore.Queries
	// package for creating HTTP routers
	r 			*chi.Mux
	// upgrades the connection to webscoket
	upgrader 	websocket.Upgrader
	// - CancelFunc notifies my handleSubscribe func that server is
	// cancelling the connection. Server does it by cancelling the
	// request context (context propagates the information asynchronally)
	// - maps in go are not thread safe, so will create running
	// conditions by default
	subscribers	map[string]map[*websocket.Conn]context.CancelFunc
	// mutex (mutual exclusion) is used to block the accesses,
	// allowing only one access at a time
	mu 			*sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

// - every http request creates a new go routine
// - if 1000 people are using my application, at
// least 1000 calls to this endpoint will be done
// this can lead to runnning conditions
// and we need to control them
func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "room_id")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}

		slog.Warn("failed to get room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	c, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("failed to upgrade connection", "error", err)
		http.Error(w, "failed to upugrade to ws connection", http.StatusBadRequest)
		return
	}

	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())

	// blocks the access to the subscribers map
	h.mu.Lock()
	// check if roomID exists inside the map
	if _, ok := h.subscribers[rawRoomID]; !ok {
		h.subscribers[rawRoomID] = make(map[*websocket.Conn]context.CancelFunc)
		h.subscribers[rawRoomID][c] = cancel
	}
	h.subscribers[rawRoomID][c] = cancel
	slog.Info("new client connected", "room_id", rawRoomID, "client_ip", r.RemoteAddr)
	h.mu.Unlock()

	// waits until context is cancelled by server or client
	<-ctx.Done()

	h.mu.Lock()
	// remove client connection from my pool of connections
	delete(h.subscribers[rawRoomID], c)
	h.mu.Unlock()
}

func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	roomID, err := h.q.InsertRoom(r.Context(), body.Theme)
	if err != nil {
		slog.Warn("failed to insert room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: roomID.String()})

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {}
func (h apiHandler) handleCreateRoomMessages(w http.ResponseWriter, r *http.Request) {}
func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {}
func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {}
func (h apiHandler) handleReactionToMessage(w http.ResponseWriter, r *http.Request) {}
func (h apiHandler) handleRemoveReactionFromMessage(w http.ResponseWriter, r *http.Request) {}
func (h apiHandler) handleMarkMessageAsAsnwered(w http.ResponseWriter, r *http.Request) {}

func NewHandler(q *pgstore.Queries) http.Handler {
	a := apiHandler{
		q: q,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true }, // avoid cors error
		},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mu: &sync.Mutex{},
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/subscribe/{room_id}", a.handleSubscribe)

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", a.handleCreateRoom)
			r.Get("/", a.handleGetRooms)

			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Post("/", a.handleCreateRoomMessages)
				r.Get("/", a.handleGetRoomMessages)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Get("/", a.handleGetRoomMessage)
					r.Patch("/react", a.handleReactionToMessage)
					r.Delete("/react", a.handleRemoveReactionFromMessage)
					r.Patch("/answer", a.handleMarkMessageAsAsnwered)
				})
			})
		})
	})

	a.r = r
	return a
}
