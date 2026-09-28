package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
	echoMiddleware "github.com/labstack/echo/v5/middleware"
	"github.com/wizier/airvault/internal/auth"
)

// No WWW-Authenticate: browsers get the SPA login page instead of the native
// dialog, and `curl -u` still authenticates preemptively.
func apiAuthMiddleware(credentials *auth.Credentials) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if authenticated(c, credentials) {
				return next(c)
			}
			return echo.ErrUnauthorized
		}
	}
}

func authenticated(c *echo.Context, credentials *auth.Credentials) bool {
	if cookie, err := c.Cookie(auth.SessionCookie); err == nil && credentials.ValidSession(cookie.Value) {
		return true
	}
	if user, token, ok := c.Request().BasicAuth(); ok && credentials.Valid(user, token) {
		return true
	}
	return false
}

func csrfMiddleware() echo.MiddlewareFunc {
	return echoMiddleware.CSRFWithConfig(echoMiddleware.CSRFConfig{
		Skipper: func(c *echo.Context) bool {
			return c.Request().URL.Path == "/healthz"
		},
		TokenLookup:    "header:X-CSRF-Token",
		CookieName:     "_csrf",
		CookiePath:     "/",
		CookieHTTPOnly: false, // the SPA mirrors the token into X-CSRF-Token
		CookieSameSite: http.SameSiteStrictMode,
	})
}
