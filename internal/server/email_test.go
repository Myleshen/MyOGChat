package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"myogchat/internal/auth"
	"myogchat/internal/mail"
	"myogchat/internal/store"
)

func TestResolveMailAccountPresets(t *testing.T) {
	// A known preset fills in host/port/tls.
	acct, err := resolveMailAccount(mailRequest{
		Provider: "zoho_in", Email: "me@example.com", Password: "app-pass",
	})
	if err != nil {
		t.Fatalf("resolveMailAccount(zoho_in): %v", err)
	}
	if acct.Host != "imappro.zoho.in" || acct.Port != 993 || acct.TLS != mail.TLSImplicit {
		t.Fatalf("zoho_in account = %+v", acct)
	}

	// Custom requires an explicit host.
	if _, err := resolveMailAccount(mailRequest{Provider: "custom", Email: "a@b.c", Password: "p"}); err == nil {
		t.Error("custom with no host should error")
	}
	acct, err = resolveMailAccount(mailRequest{
		Provider: "custom", Email: "a@b.c", Password: "p",
		Host: "imap.example.com", Port: 143, TLS: "starttls",
	})
	if err == nil {
		t.Fatalf("starttls should be rejected, got %+v", acct)
	}

	// Unknown provider.
	if _, err := resolveMailAccount(mailRequest{Provider: "nope", Email: "a@b.c", Password: "p"}); err == nil {
		t.Error("unknown provider should error")
	}
	// Missing password.
	if _, err := resolveMailAccount(mailRequest{Provider: "gmail", Email: "a@b.c"}); err == nil {
		t.Error("missing password should error")
	}
	// Empty provider defaults to custom.
	if _, err := resolveMailAccount(mailRequest{Email: "a@b.c", Password: "p"}); err == nil {
		t.Error("empty provider with no host should error")
	}
}

// seedMailAccount inserts a mailbox with an encrypted app password.
func seedMailAccount(t *testing.T, s *Server, st *store.Store, userID, label string, isDefault bool) store.MailAccount {
	t.Helper()
	enc, err := s.encryptMailCredential("s3cret-app-pass")
	if err != nil {
		t.Fatalf("encryptMailCredential: %v", err)
	}
	row := &store.MailAccount{
		UserID: userID, Label: label, Provider: "gmail", Email: label + "@example.com",
		Host: "imap.gmail.com", Port: 993, TLS: mail.TLSImplicit, Enabled: true,
		IsDefault: isDefault, SecretEnc: enc, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := st.CreateMailAccount(context.Background(), row); err != nil {
		t.Fatalf("CreateMailAccount: %v", err)
	}
	return *row
}

func TestMailAccountDecryptsOnlyWhenEnabled(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u-mail", "mailer", "")

	// Not connected yet.
	if _, ok := srv.mailAccount(ctx, u.ID); ok {
		t.Fatal("mailAccount should be false before connect")
	}

	row := seedMailAccount(t, srv, st, u.ID, "work", true)
	// The password is not stored in plaintext.
	if row.SecretEnc == "s3cret-app-pass" {
		t.Fatal("credential stored in plaintext")
	}

	got, ok := srv.mailAccount(ctx, u.ID)
	if !ok {
		t.Fatal("mailAccount = false, want true")
	}
	if got.Password != "s3cret-app-pass" || got.Host != "imap.gmail.com" || got.Email != "work@example.com" {
		t.Fatalf("mailAccount = %+v", got)
	}

	// Resolving by id and by label both decrypt.
	if _, err := srv.mailAccountFor(ctx, u.ID, row.ID); err != nil {
		t.Errorf("mailAccountFor(id): %v", err)
	}
	if _, err := srv.mailAccountFor(ctx, u.ID, "WORK"); err != nil {
		t.Errorf("mailAccountFor(label): %v", err)
	}
	if _, err := srv.mailAccountFor(ctx, u.ID, "ghost"); err == nil {
		t.Error("mailAccountFor(unknown) should error")
	}

	// Disabling hides the account from tool registration.
	if err := st.SetMailAccountEnabled(ctx, u.ID, row.ID, false, 2); err != nil {
		t.Fatalf("SetMailAccountEnabled: %v", err)
	}
	if _, ok := srv.mailAccount(ctx, u.ID); ok {
		t.Error("mailAccount should be false when disabled")
	}
	if _, err := srv.mailAccountFor(ctx, u.ID, row.ID); err == nil {
		t.Error("mailAccountFor should reject a disabled mailbox")
	}
	if _, ok := srv.mailConnector(ctx, u.ID); ok {
		t.Error("mailConnector should be false when the only mailbox is disabled")
	}
	if err := st.SetMailAccountEnabled(ctx, u.ID, row.ID, true, 3); err != nil {
		t.Fatalf("SetMailAccountEnabled: %v", err)
	}
	if _, ok := srv.mailConnector(ctx, u.ID); !ok {
		t.Error("mailConnector should be true when enabled")
	}
}

func TestToolRegistryIncludesEmailOnlyWhenConnected(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u-mail2", "mailer2", "")

	reg := srv.toolRegistry(ctx, u)
	for _, name := range []string{"email_folders", "email_search", "email_read", "email_accounts", "email_digest"} {
		if _, ok := reg.Get(name); ok {
			t.Fatalf("tool %q registered without a mailbox", name)
		}
	}

	seedMailAccount(t, srv, st, u.ID, "work", true)
	reg = srv.toolRegistry(ctx, u)
	for _, name := range []string{"email_folders", "email_search", "email_read", "email_accounts", "email_digest"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("tool %q not registered with a connected mailbox", name)
		}
	}
	if _, ok := reg.Get("email_send"); ok {
		t.Error("email_send must never be registered (read-only)")
	}
}

// TestMailAccountEndpointsNeverLeakSecret drives the handlers directly and
// asserts the stored ciphertext is never serialized.
func TestMailAccountEndpointsNeverLeakSecret(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u-mail3", "mailer3", "")
	row := seedMailAccount(t, srv, st, u.ID, "work", true)

	// Plural list.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/me/mail/accounts", nil)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	srv.handleListMyMailAccounts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"s3cret-app-pass", "secret_enc", row.SecretEnc} {
		if strings.Contains(body, leak) {
			t.Errorf("list response leaked %q:\n%s", leak, body)
		}
	}
	if !strings.Contains(body, `"accounts"`) || !strings.Contains(body, `"presets"`) {
		t.Errorf("list response missing fields:\n%s", body)
	}

	// Singular alias.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/me/mail", nil)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	srv.handleGetMyMail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "s3cret-app-pass") || strings.Contains(rec.Body.String(), "secret_enc") {
		t.Errorf("singular response leaked the secret:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"connected":true`) {
		t.Errorf("singular GET should report the default connected:\n%s", rec.Body.String())
	}
}

// TestSingularMailAliasesFollowDefault checks the singular endpoints operate on
// the default mailbox (and that the plural delete promotes a new default).
func TestSingularMailAliasesFollowDefault(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u-mail4", "mailer4", "")
	first := seedMailAccount(t, srv, st, u.ID, "work", true)
	second := seedMailAccount(t, srv, st, u.ID, "personal", false)

	// PATCH /api/me/mail {enabled:false} toggles only the default.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/me/mail", strings.NewReader(`{"enabled":false}`))
	req = req.WithContext(auth.WithUser(req.Context(), u))
	srv.handlePatchMyMail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d", rec.Code)
	}
	if a, _ := st.GetMailAccountByID(ctx, u.ID, first.ID); a.Enabled {
		t.Error("default should be disabled")
	}
	if a, _ := st.GetMailAccountByID(ctx, u.ID, second.ID); !a.Enabled {
		t.Error("non-default should be untouched")
	}

	// DELETE /api/me/mail removes only the default; the other is promoted.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/me/mail", nil)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	srv.handleDeleteMyMail(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if _, err := st.GetMailAccountByID(ctx, u.ID, first.ID); err != store.ErrNotFound {
		t.Error("default not deleted")
	}
	if a, _ := st.GetMailAccount(ctx, u.ID); a.ID != second.ID {
		t.Errorf("default after delete = %q, want %q", a.ID, second.ID)
	}
}

// TestPluralDeleteCrossUserIs404 checks owner scoping collapses to 404.
func TestPluralDeleteCrossUserIs404(t *testing.T) {
	srv, st := newKeyTestServer(t)
	u := seedKeyUser(t, st, "u-mail5", "mailer5", "")
	other := seedKeyUser(t, st, "u-mail6", "mailer6", "")
	row := seedMailAccount(t, srv, st, other.ID, "work", true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/me/mail/accounts/"+row.ID, nil)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	req.SetPathValue("id", row.ID)
	srv.handleDeleteMyMailAccount(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user delete status = %d, want 404", rec.Code)
	}
	if _, err := st.GetMailAccountByID(context.Background(), other.ID, row.ID); err != nil {
		t.Errorf("owner's mailbox was deleted: %v", err)
	}
}

// TestEmailDigestStandardOnlyWhenRegistered confirms the summarization fragment
// is composed with a date header and that joinSystem keeps order.
func TestEmailDigestStandardOnlyWhenRegistered(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("Europe/London tz unavailable: %v", err)
	}
	frag := emailDigestStandardWithDate(time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC), loc)
	for _, want := range []string{"Action required", "Time-sensitive", "Worth knowing", "Noise", "2026-10-11"} {
		if !strings.Contains(frag, want) {
			t.Errorf("standard missing %q:\n%s", want, frag)
		}
	}
	joined := joinSystem("Project rules", frag)
	if !strings.HasPrefix(joined, "Project rules\n\n") {
		t.Errorf("project prompt should stay first:\n%s", joined)
	}
	if got := joinSystem("", frag); got != frag {
		t.Errorf("empty project prompt should yield the fragment alone")
	}
}
