// Package mail is the read-only IMAP client for MyOGChat (feature 3.2).
//
// It is read-only by construction, in layers:
//
//  1. The Client type exposes only read methods (Folders/Search/Read) — there
//     is no write path in the package.
//  2. Mailboxes are opened with EXAMINE (imap.SelectOptions{ReadOnly: true}),
//     never SELECT, so opening a folder never clears \Recent or otherwise
//     mutates it.
//  3. Bodies are fetched with BODY.PEEK[] so reading never sets \Seen.
//  4. Every byte the client writes to the socket passes through a transport
//     command allowlist (commandFilter) that rejects any command outside a
//     small read-only set. Even a bug in the layers above cannot mutate the
//     mailbox — this is the key guardrail, because app-specific passwords are
//     full-access credentials and scoping must be ours.
//
// The package also never returns credentials to callers; the Account password
// only ever travels into the TLS connection.
package mail

import "strings"

// TLS modes understood by the client. Only implicit TLS (port 993) is
// supported: it is what Gmail, iCloud and Zoho all require, and it keeps the
// transport command allowlist free of STARTTLS.
const (
	TLSImplicit = "implicit"
)

// Provider is a connect preset. Zoho is region-aware (India/Global/Europe) and
// has separate hosts for paid (custom-domain) and personal (@zoho*.in) accounts.
type Provider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
	TLS  string `json:"tls"`
}

// presets is the ordered list surfaced to the Settings UI. "custom" lets a user
// supply their own host/port.
var presets = []Provider{
	{ID: "gmail", Name: "Gmail", Host: "imap.gmail.com", Port: 993, TLS: TLSImplicit},
	{ID: "icloud", Name: "iCloud Mail", Host: "imap.mail.me.com", Port: 993, TLS: TLSImplicit},
	{ID: "zoho_in", Name: "Zoho Mail (India, Pro/domain)", Host: "imappro.zoho.in", Port: 993, TLS: TLSImplicit},
	{ID: "zoho_in_personal", Name: "Zoho Mail (India, personal)", Host: "imap.zoho.in", Port: 993, TLS: TLSImplicit},
	{ID: "zoho_com", Name: "Zoho Mail (Global, Pro/domain)", Host: "imappro.zoho.com", Port: 993, TLS: TLSImplicit},
	{ID: "zoho_com_personal", Name: "Zoho Mail (Global, personal)", Host: "imap.zoho.com", Port: 993, TLS: TLSImplicit},
	{ID: "zoho_eu", Name: "Zoho Mail (Europe, Pro/domain)", Host: "imappro.zoho.eu", Port: 993, TLS: TLSImplicit},
	{ID: "zoho_eu_personal", Name: "Zoho Mail (Europe, personal)", Host: "imap.zoho.eu", Port: 993, TLS: TLSImplicit},
	{ID: "custom", Name: "Custom IMAP", Host: "", Port: 993, TLS: TLSImplicit},
}

// Presets returns a copy of the supported provider presets.
func Presets() []Provider {
	out := make([]Provider, len(presets))
	copy(out, presets)
	return out
}

// LookupProvider finds a preset by id (case-insensitive). The second result is
// false when the id is unknown.
func LookupProvider(id string) (Provider, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range presets {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}
