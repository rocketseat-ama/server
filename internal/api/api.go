package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const (
	MessageKindMessageCreated = "message_created"
)

type MessageMessageCreated struct {
	ID		string	`json:"id"`
	Message string	`json:"message"`
}

type Message struct {
	Kind 	string 	`json:"kind"`
	Value 	any 	`json:"value"`
	RoomID 	string 	`json:"-"`
}

func (h apiHandler) getUUIDParam (r *http.Request, param string ) (uuid.UUID, error) {
	rawValue := chi.URLParam(r, param)
	value, err := uuid.Parse(rawValue)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid %s param", param)
	}
	return value, nil
}

func (h apiHandler) sendResponse (w http.ResponseWriter, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		slog.Error("failed to marshal response", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

func (h apiHandler) notifyClients(msg Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subscribers, ok := h.subscribers[msg.RoomID]
	if !ok || len(subscribers) == 0 {
		return
	}

	for conn, cancel := range subscribers {
		if err := conn.WriteJSON(msg); err != nil {
			slog.Error("failed to send message to client", "error", err)
			cancel()
		}
	}
}

// - every http request creates a new go routine
// - if 1000 people are using my application, at
// least 1000 calls to this endpoint will be done
// this can lead to runnning conditions
// and we need to control them
func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	roomID, err := h.getUUIDParam(r, "room_id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}

		slog.Error("failed to get room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	c, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("failed to upgrade connection", "error", err)
		http.Error(w, "failed to upugrade to ws connection", http.StatusBadRequest)
		return
	}

	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())
	rawRoomID := roomID.String()

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
		slog.Error("failed to insert room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	h.sendResponse(w, response{ID: roomID.String()})
}

func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := h.q.GetRooms(r.Context())
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("failed to get rooms", "error", err)
			http.Error(w, "something went wrong", http.StatusInternalServerError)
			return
		}
		
		rooms = []pgstore.Room{}
	}

	type response struct {
		Rooms []pgstore.Room `json:"rooms"`
	}

	h.sendResponse(w, response{Rooms: rooms})
}

func (h apiHandler) handleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	roomID, err := h.getUUIDParam(r, "room_id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}

		slog.Error("failed to get room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type _body struct {
		Message string `json:"message"`
	}

	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	messageID, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{RoomID: roomID, Message: body.Message})
	if err != nil {
		slog.Error("failed to insert message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	h.sendResponse(w, response{ID: messageID.String()})

	go h.notifyClients(Message{
		Kind: MessageKindMessageCreated,
		RoomID: roomID.String(),
		Value: MessageMessageCreated{
			ID: 	 messageID.String(),
			Message: body.Message,
		},
	})
}

func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	roomID, err := h.getUUIDParam(r, "room_id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	messages, err := h.q.GetRoomMessages(r.Context(), roomID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("failed to get room messages", "error", err)
			http.Error(w, "something went wrong", http.StatusInternalServerError)
			return
		}
		
		messages = []pgstore.Message{}
	}

	type response struct {
		Messages []pgstore.Message `json:"messages"`
	}

	h.sendResponse(w, response{Messages: messages})
}

func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {
	messageID, err := h.getUUIDParam(r, "message_id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	message, err := h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}

		slog.Error("failed to get room message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		Message pgstore.Message `json:"message"`
	}

	h.sendResponse(w, response{Message: message})
}

func (h apiHandler) handleReactionToMessage(w http.ResponseWriter, r *http.Request) {
	messageID, err := h.getUUIDParam(r, "message_id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}

		slog.Error("failed to get room message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	reactionCount, err := h.q.ReactToMessage(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to react to message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ReactionCount int64 `json:"reactionCount"`
	}

	h.sendResponse(w, response{ReactionCount: reactionCount})
}

func (h apiHandler) handleRemoveReactionFromMessage(w http.ResponseWriter, r *http.Request) {
	messageID, err := h.getUUIDParam(r, "message_id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}

		slog.Error("failed to get room message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	reactionCount, err := h.q.RemoveReactionFromMessage(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to remove reaction from message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ReactionCount int64 `json:"reactionCount"`
	}

	h.sendResponse(w, response{ReactionCount: reactionCount})
}

func (h apiHandler) handleMarkMessageAsAnswered(w http.ResponseWriter, r *http.Request) {
	messageID, err := h.getUUIDParam(r, "message_id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err = h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}

		slog.Error("failed to get room message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	err = h.q.MarkMessageAsAnswered(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to mark message as answered", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

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
				r.Post("/", a.handleCreateRoomMessage)
				r.Get("/", a.handleGetRoomMessages)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Get("/", a.handleGetRoomMessage)
					r.Patch("/react", a.handleReactionToMessage)
					r.Delete("/react", a.handleRemoveReactionFromMessage)
					r.Patch("/answer", a.handleMarkMessageAsAnswered)
				})
			})
		})
	})

	a.r = r
	return a
}
