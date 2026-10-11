package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/middleware"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/service"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

// Authenticator is what the handlers need from the auth service.
// *service.AuthService satisfies it; tests use a fake.
type Authenticator interface {
	Register(ctx context.Context, email, password string) (*models.User, error)
	Login(ctx context.Context, email, password string) (*service.LoginResult, error)
	GetUser(ctx context.Context, id uuid.UUID) (*models.User, error)
}

type AuthHandler struct {
	auth Authenticator
}

func NewAuthHandler(auth Authenticator) *AuthHandler {
	return &AuthHandler{auth: auth}
}

// POST /api/auth/register
func (h *AuthHandler) Register(c *gin.Context) {
	var req RegisterRequest
	if !bindJSON(c, &req) {
		return
	}

	user, err := h.auth.Register(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		writeServiceError(c, err)
		return
	}

	response.Created(c, gin.H{"user": toUserResponse(user)})
}

// POST /api/auth/login
func (h *AuthHandler) Login(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}

	res, err := h.auth.Login(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		writeServiceError(c, err)
		return
	}

	// A response containing a token must never be stored by caches or proxies.
	c.Header("Cache-Control", "no-store")
	response.OK(c, LoginResponse{
		Token:     res.Token,
		TokenType: "Bearer",
		ExpiresAt: res.ExpiresAt,
		User:      toUserResponse(res.User),
	})
}

// GET /api/me (protected)
func (h *AuthHandler) Me(c *gin.Context) {
	id, ok := middleware.UserIDFrom(c)
	if !ok {
		// Only possible if the route was registered without the auth
		// middleware. That's a wiring bug, so fail loudly instead of quietly.
		response.InternalError(c, errors.New("me: user id missing from context"))
		return
	}

	user, err := h.auth.GetUser(c.Request.Context(), id)
	switch {
	case errors.Is(err, models.ErrNotFound):
		// The token is valid, but the account was deleted after it was issued.
		response.Error(c, http.StatusUnauthorized, response.CodeUnauthorized, "account no longer exists")
		return
	case err != nil:
		response.InternalError(c, err)
		return
	}

	response.OK(c, gin.H{"user": toUserResponse(user)})
}

// writeServiceError is the single place where service/repository errors
// become HTTP responses. Anything not listed here is a 500.
func writeServiceError(c *gin.Context, err error) {
	var verr *service.ValidationError

	switch {
	case errors.As(err, &verr):
		response.ErrorWithDetails(c, http.StatusBadRequest, response.CodeValidation, "invalid input",
			[]response.FieldError{{Field: verr.Field, Message: verr.Message}})

	case errors.Is(err, models.ErrEmailTaken):
		response.Error(c, http.StatusConflict, response.CodeEmailTaken, "email already registered")

	case errors.Is(err, service.ErrInvalidCredentials):
		response.Error(c, http.StatusUnauthorized, response.CodeInvalidCredentials, "invalid email or password")

	default:
		response.InternalError(c, err) // logged server-side, generic message to the client
	}
}