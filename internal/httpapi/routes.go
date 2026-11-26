package httpapi

import (
	"net/http"
	"strings"
	"sync"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

type server struct {
	accounts    auth.AccountStore
	rooms       live.RoomStore
	recentChats *sync.Map
}

func Register(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore) {
	s := server{accounts: accounts, rooms: rooms, recentChats: &sync.Map{}}

	mux.HandleFunc("GET /healthchecker", health)
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /status", status)
	mux.HandleFunc("POST /api/auth/signup", s.signUpCompat)
	mux.HandleFunc("POST /api/auth/signin", s.signInCompat)
	mux.HandleFunc("GET /api/auth/session", s.authenticated(s.session))
	mux.HandleFunc("POST /api/auth/signout", s.authenticated(s.signOut))
	mux.HandleFunc("GET /api/user/{user_id}", s.authenticated(s.userProfile))
	mux.HandleFunc("POST /api/user/set_language", s.authenticated(s.setLanguage))

	mux.HandleFunc("POST /api/live/room/start", s.authenticated(s.startRoom))
	mux.HandleFunc("POST /api/live/room/setting", s.authenticated(s.updateRoomSettings))
	mux.HandleFunc("POST /api/live/room/list", s.authenticated(s.listRooms))
	mux.HandleFunc("GET /api/live/room/info/{room}", s.authenticated(s.infoRoom))
	mux.HandleFunc("GET /api/live/room/audiences/{room}", s.authenticated(s.audiences))
	mux.HandleFunc("POST /api/live/room/audiences/{room}", s.authenticated(s.audiencesPage))
	mux.HandleFunc("GET /api/live/room/admins/{room}", s.authenticated(s.admins))
	mux.HandleFunc("GET /api/live/room/bans/{room}", s.authenticated(s.bans))
	mux.HandleFunc("GET /api/live/room/kicked/{room}/{uid}", s.authenticated(s.kicked))
	mux.HandleFunc("GET /api/live/room/banned/{room}/{uid}", s.authenticated(s.banned))
	mux.HandleFunc("GET /api/live/room/limited/{room}", s.authenticated(s.limited))
	mux.HandleFunc("GET /api/live/room/audience/{room}/{uid}", s.authenticated(s.audience))
	mux.HandleFunc("GET /api/live/room/online", s.authenticated(s.online))
	mux.HandleFunc("GET /api/live/room/check_password/{room_id}", s.authenticated(s.checkPassword))
	mux.HandleFunc("POST /api/live/room/join/{room}", s.authenticated(s.joinRoom))
	mux.HandleFunc("POST /api/live/room/leave/{room}", s.authenticated(roomAction(rooms.Leave)))
	mux.HandleFunc("POST /api/live/room/kicked/{room}/{uid}", s.authenticated(targetAction(rooms.Kick)))
	mux.HandleFunc("POST /api/live/room/ban/{room}/{uid}", s.authenticated(s.setMute))
	mux.HandleFunc("DELETE /api/live/room/ban/{room}/{uid}", s.authenticated(s.setMute))
	mux.HandleFunc("POST /api/live/room/add_admin/{room}/{uid}", s.authenticated(s.setAdmin))
	mux.HandleFunc("DELETE /api/live/room/add_admin/{room}/{uid}", s.authenticated(s.setAdmin))
	mux.HandleFunc("POST /api/live/room/chat", s.authenticated(s.chat))
	mux.HandleFunc("POST /api/live/room/messages", s.authenticated(s.chatHistory))
}

func health(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200", "message": "open_live_platform"})
}

func status(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200", "data": "active"})
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Account  string `json:"account"`
}

func (s server) signUp(w http.ResponseWriter, r *http.Request) {
	var input credentials
	if httpx.Decode(w, r, &input) {
		session, err := s.accounts.SignUp(r.Context(), input.Email, input.Password, input.Account)
		sessionResponse(w, http.StatusCreated, session, err)
	}
}

func (s server) signIn(w http.ResponseWriter, r *http.Request) {
	var input credentials
	if httpx.Decode(w, r, &input) {
		session, err := s.accounts.SignIn(r.Context(), input.Email, input.Password)
		sessionResponse(w, http.StatusOK, session, err)
	}
}

func (s server) session(w http.ResponseWriter, r *http.Request, _ int64) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200", "token": token})
}

func (s server) signOut(w http.ResponseWriter, r *http.Request, _ int64) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err := s.accounts.Revoke(r.Context(), token); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not sign out")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func sessionResponse(w http.ResponseWriter, status int, session auth.Session, err error) {
	switch err {
	case nil:
		httpx.JSON(w, status, session)
	case auth.ErrMissingFields:
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case auth.ErrEmailInUse:
		httpx.Error(w, http.StatusConflict, err.Error())
	case auth.ErrInvalidCredentials:
		httpx.Error(w, http.StatusUnauthorized, err.Error())
	default:
		httpx.Error(w, http.StatusInternalServerError, "could not issue session")
	}
}

type userHandler func(http.ResponseWriter, *http.Request, int64)

func (s server) authenticated(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok, err := s.accounts.Bearer(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not authenticate request")
			return
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.Error(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next(w, r, user.ID)
	}
}
