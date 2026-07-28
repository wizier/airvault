package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/wizier/airvault/internal/auth"

	"github.com/labstack/echo/v5"
)

type loginRequest struct {
	Token string `json:"token"`
}

// [POST] /api/session — exchange the Web UI token for a login-session cookie.
func (h *Handler) createSession(c *echo.Context) error {
	var req loginRequest
	if err := echo.BindBody(c, &req); err != nil {
		return err
	}
	if !h.auth.Valid(auth.Username, strings.TrimSpace(req.Token)) {
		return newPublicError(http.StatusUnauthorized, "authentication_required", nil)
	}
	value, expires := h.auth.IssueSession()
	c.SetCookie(sessionCookie(value, expires))
	return c.NoContent(http.StatusNoContent)
}

// [DELETE] /api/session — clear the login session.
func (h *Handler) deleteSession(c *echo.Context) error {
	c.SetCookie(sessionCookie("", time.Unix(0, 0)))
	return c.NoContent(http.StatusNoContent)
}

// sessionCookie builds the HttpOnly, same-site cookie; an empty value clears it.
func sessionCookie(value string, expires time.Time) *http.Cookie {
	cookie := &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
	if value == "" {
		cookie.MaxAge = -1
	}
	return cookie
}
