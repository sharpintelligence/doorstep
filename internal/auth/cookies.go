package auth

import (
	"net/http"
	"time"
)

const (
	sessionCookieName  = "doorstep-session"
	loginCookieName    = "doorstep-login"
	secureCookiePrefix = "__Host-"
)

type cookies struct {
	session string
	login   string
	secure  bool
}

func newCookies(secure bool) cookies {
	c := cookies{session: sessionCookieName, login: loginCookieName, secure: secure}
	if secure {
		c.session = secureCookiePrefix + c.session
		c.login = secureCookiePrefix + c.login
	}
	return c
}

func (c cookies) set(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, c.cookie(name, value, int(ttl.Seconds())))
}

func (c cookies) clear(w http.ResponseWriter, name string) {
	http.SetCookie(w, c.cookie(name, "", -1))
}

func (c cookies) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   c.secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}
