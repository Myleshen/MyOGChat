// Package auth implements multi-user, password-based authentication
// (contract §9): PBKDF2 password hashing, signed session cookies, first-run
// admin setup, self-registration, and login.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"myogchat/internal/store"
)

// CookieName is the session cookie name (§9.3).
const CookieName = "myogchat"

const (
	cookieAge    = 30 * 24 * time.Hour
	pbkdf2Iter   = 600000
	pbkdf2KeyLen = 32
	pbkdf2Salt   = 16
	minPassword  = 8
)

// SettingAllowRegistration gates self-registration (§9.1).
const SettingAllowRegistration = "allow_registration"

var (
	usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`)
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// Auth holds the store and the cookie-signing secret.
type Auth struct {
	st     *store.Store
	secret []byte
}

// New returns an Auth using secret (from the DB-backed session_secret setting).
func New(st *store.Store, secret []byte) *Auth {
	return &Auth{st: st, secret: secret}
}

// ---------- password hashing ----------

// HashPassword derives a versioned PBKDF2-HMAC-SHA256 hash (§9.2).
func HashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2Salt)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2$sha256$%d$%s$%s", pbkdf2Iter,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the encoded hash.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---------- request context ----------

type ctxKey struct{}

// UserFrom returns the authenticated user stored by the middleware.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(store.User)
	return u, ok
}

// WithUser returns a copy of ctx carrying u as the authenticated user. It is
// primarily for tests that exercise handlers without the full middleware.
func WithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// RequireAdmin writes 403 and returns false when u is not an admin.
func RequireAdmin(w http.ResponseWriter, u store.User) bool {
	if !u.IsAdmin {
		writeError(w, http.StatusForbidden, "admin required")
		return false
	}
	return true
}

// ---------- validation ----------

type credentials struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// ValidateCredentials checks a username, email, and password, returning a
// user-facing message when invalid.
func ValidateCredentials(username, email, password string) (string, bool) {
	if !usernameRe.MatchString(username) {
		return "username must be 3-32 characters of letters, digits, _ . or -", false
	}
	if len(email) > 254 || !emailRe.MatchString(email) {
		return "a valid email address is required", false
	}
	if len(password) < minPassword {
		return "password must be at least 8 characters", false
	}
	return "", true
}

// validate normalizes and checks credentials.
func (c *credentials) validate() (string, bool) {
	c.Username = strings.TrimSpace(c.Username)
	c.Email = strings.TrimSpace(c.Email)
	return ValidateCredentials(c.Username, c.Email, c.Password)
}

// ---------- endpoints ----------

// Status reports first-run and registration state (public).
func (a *Auth) Status(w http.ResponseWriter, r *http.Request) {
	n, err := a.st.CountUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read users")
		return
	}
	open, err := a.registrationOpen(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{
		"needs_setup":       n == 0,
		"registration_open": open,
	})
}

// Setup creates the first admin account, allowed only while no users exist.
func (a *Auth) Setup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n, err := a.st.CountUsers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read users")
		return
	}
	if n > 0 {
		writeError(w, http.StatusForbidden, "setup already completed")
		return
	}
	var c credentials
	if err := decode(w, r, &c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if msg, ok := c.validate(); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := HashPassword(c.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	now := time.Now().UnixMilli()
	u := store.User{
		ID: newID(), Username: c.Username, Email: c.Email, PasswordHash: hash,
		IsAdmin: true, Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := a.st.InsertUser(ctx, &u); err != nil {
		if isDuplicate(err) {
			writeError(w, http.StatusConflict, "username or email already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	// The first admin inherits any pre-existing (ownerless) chats/projects.
	if err := a.st.ClaimLegacyData(ctx, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to claim existing data")
		return
	}
	a.setCookie(w, r, u.ID, time.Now().Add(cookieAge))
	writeJSON(w, http.StatusCreated, publicUser(u))
}

// Register creates a non-admin user when self-registration is open.
func (a *Auth) Register(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	open, err := a.registrationOpen(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read settings")
		return
	}
	if !open {
		writeError(w, http.StatusForbidden, "registration is disabled")
		return
	}
	var c credentials
	if err := decode(w, r, &c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if msg, ok := c.validate(); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := HashPassword(c.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	now := time.Now().UnixMilli()
	u := store.User{
		ID: newID(), Username: c.Username, Email: c.Email, PasswordHash: hash,
		IsAdmin: false, Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
	if err := a.st.InsertUser(ctx, &u); err != nil {
		if isDuplicate(err) {
			writeError(w, http.StatusConflict, "username or email already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	// Registration creates a pending account; no session until an admin
	// approves it (§9.3). This keeps the service safe from drive-by signups.
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "pending",
		"message": "Your account is awaiting admin approval.",
	})
}

// Login authenticates by username or email.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Identifier string `json:"identifier"`
		Password   string `json:"password"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ident := strings.TrimSpace(req.Identifier)
	u, err := a.st.GetUserByIdentifier(r.Context(), ident)
	if err != nil || !VerifyPassword(u.PasswordHash, req.Password) {
		// Same response whether the user is unknown or the password is wrong.
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	switch u.Status {
	case "pending":
		writeError(w, http.StatusForbidden, "account pending approval")
		return
	case "disabled":
		writeError(w, http.StatusForbidden, "account disabled")
		return
	}
	a.setCookie(w, r, u.ID, time.Now().Add(cookieAge))
	w.WriteHeader(http.StatusNoContent)
}

// Logout clears the session cookie.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1, Secure: isSecure(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// Me returns the authenticated user.
func (a *Auth) Me(w http.ResponseWriter, r *http.Request) {
	u, ok := UserFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, publicUser(u))
}

// Middleware protects every /api/* route except the public auth endpoints and
// attaches the authenticated user to the request context.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		u, ok := a.currentUser(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

// isPublicPath lists the auth endpoints reachable without a session (§9.3).
func isPublicPath(p string) bool {
	switch p {
	case "/api/auth/status", "/api/auth/setup", "/api/auth/register", "/api/auth/login":
		return true
	}
	return false
}

// ---------- session cookie ----------

func (a *Auth) setCookie(w http.ResponseWriter, r *http.Request, userID string, expires time.Time) {
	expiry := strconv.FormatInt(expires.Unix(), 10)
	payload := userID + "|" + expiry
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	value := base64.RawURLEncoding.EncodeToString(
		[]byte(payload + "|" + hex.EncodeToString(mac.Sum(nil))))
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: value, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Expires: expires,
		MaxAge: int(cookieAge.Seconds()), Secure: isSecure(r),
	})
}

// currentUser validates the session cookie and loads the user.
func (a *Auth) currentUser(r *http.Request) (store.User, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return store.User{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return store.User{}, false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return store.User{}, false
	}
	userID, expiry, sig := parts[0], parts[1], parts[2]
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(userID + "|" + expiry))
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return store.User{}, false
	}
	exp, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return store.User{}, false
	}
	u, err := a.st.GetUserByID(r.Context(), userID)
	if err != nil {
		return store.User{}, false
	}
	return u, true
}

// registrationOpen reads the allow_registration setting (default open).
func (a *Auth) registrationOpen(ctx context.Context) (bool, error) {
	v, ok, err := a.st.GetSetting(ctx, SettingAllowRegistration)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	return v == "1", nil
}

// ---------- helpers ----------

func publicUser(u store.User) map[string]any {
	return map[string]any{
		"id": u.ID, "username": u.Username, "email": u.Email,
		"is_admin": u.IsAdmin, "status": u.Status,
	}
}

func isSecure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func isDuplicate(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}

// newID returns a random RFC 4122 v4 UUID.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
