package server

import (
	"context"
	"encoding/json"
	"testing"

	"myogchat/internal/mail"
	"myogchat/internal/secret"
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

func TestMailAccountDecryptsOnlyWhenEnabled(t *testing.T) {
	srv, st := newKeyTestServer(t)
	ctx := context.Background()
	u := seedKeyUser(t, st, "u-mail", "mailer", "")

	// Not connected yet.
	if _, ok := srv.mailAccount(ctx, u.ID); ok {
		t.Fatal("mailAccount should be false before connect")
	}

	blob, _ := json.Marshal(mailCredential{AuthType: "password", Password: "s3cret-app-pass"})
	enc, err := secret.Encrypt(testEncKey(), blob)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	row := &store.MailAccount{
		UserID: u.ID, Provider: "gmail", Email: "mailer@gmail.com",
		Host: "imap.gmail.com", Port: 993, TLS: mail.TLSImplicit, Enabled: true,
		SecretEnc: enc, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := st.SetMailAccount(ctx, row); err != nil {
		t.Fatalf("SetMailAccount: %v", err)
	}
	// The password is not stored in plaintext.
	if row.SecretEnc == "s3cret-app-pass" {
		t.Fatal("credential stored in plaintext")
	}

	got, ok := srv.mailAccount(ctx, u.ID)
	if !ok {
		t.Fatal("mailAccount = false, want true")
	}
	if got.Password != "s3cret-app-pass" || got.Host != "imap.gmail.com" || got.Email != "mailer@gmail.com" {
		t.Fatalf("mailAccount = %+v", got)
	}

	// Disabling hides the account from tool registration.
	if err := st.SetMailAccountEnabled(ctx, u.ID, false, 2); err != nil {
		t.Fatalf("SetMailAccountEnabled: %v", err)
	}
	if _, ok := srv.mailAccount(ctx, u.ID); ok {
		t.Error("mailAccount should be false when disabled")
	}

	// The connector is only produced for an enabled account.
	if _, ok := srv.mailConnector(ctx, u.ID); ok {
		t.Error("mailConnector should be false when disabled")
	}
	if err := st.SetMailAccountEnabled(ctx, u.ID, true, 3); err != nil {
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
	if _, ok := reg.Get("email_search"); ok {
		t.Fatal("email tools registered without a mailbox")
	}

	blob, _ := json.Marshal(mailCredential{AuthType: "password", Password: "p"})
	enc, _ := secret.Encrypt(testEncKey(), blob)
	if err := st.SetMailAccount(ctx, &store.MailAccount{
		UserID: u.ID, Provider: "gmail", Email: "m@example.com",
		Host: "imap.gmail.com", Port: 993, TLS: mail.TLSImplicit, Enabled: true,
		SecretEnc: enc, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("SetMailAccount: %v", err)
	}
	reg = srv.toolRegistry(ctx, u)
	for _, name := range []string{"email_folders", "email_search", "email_read"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("tool %q not registered with a connected mailbox", name)
		}
	}
	if _, ok := reg.Get("email_send"); ok {
		t.Error("email_send must never be registered (read-only)")
	}
}
