// Package auth implements the shared-token, cookie-based auth of contract §1.
// The token itself is never logged and never stored — only its SHA-256.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CookieName is the session cookie name (contract §1).
const CookieName = "myogchat"

// cookieAge is the 30-day cookie lifetime (contract §1).
const cookieAge = 30 * 24 * time.Hour

// Auth holds the derived key for login comparison and cookie signing.
type Auth struct {
	key []byte // sha256(MYOGCHAT_AUTH_TOKEN)
}

// New derives the signing key from the shared token.
func New(token string) *Auth {
	sum := sha256.Sum256([]byte(token))
	return &Auth{key: sum[:]}
}

// Login handles POST /api/login.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sum := sha256.Sum256([]byte(req.Token))
	if subtle.ConstantTimeCompare(sum[:], a.key) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	a.setCookie(w, r, time.Now().Add(cookieAge))
	w.WriteHeader(http.StatusNoContent)
}

// Logout handles POST /api/logout by clearing the cookie.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Secure:   isSecure(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// Me handles GET /api/me. The middleware has already enforced auth.
func (a *Auth) Me(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// Middleware protects every /api/* route except /api/login (contract §1).
// Static files stay public; the SPA shows its own login screen on 401.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/login" && !a.valid(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// setCookie writes the signed session cookie:
//
//	base64url(expiryUnix|hex(hmacSHA256(expiryUnix, sha256(token))))
func (a *Auth) setCookie(w http.ResponseWriter, r *http.Request, expires time.Time) {
	expiry := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(expiry))
	value := base64.RawURLEncoding.EncodeToString(
		[]byte(expiry + "|" + hex.EncodeToString(mac.Sum(nil))))
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		MaxAge:   int(cookieAge.Seconds()),
		Secure:   isSecure(r),
	})
}

// valid reports whether the request carries a well-formed, unexpired cookie.
func (a *Auth) valid(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return false
	}
	expiry, sig, ok := strings.Cut(string(raw), "|")
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(expiry))
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

// isSecure reports whether the request arrived over HTTPS (directly or via a
// trusted proxy header), in which case the cookie gets the Secure attribute.
func isSecure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
