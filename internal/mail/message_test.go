package mail

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

const multipartRaw = "From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: =?utf-8?q?H=C3=A9llo?=\r\n" +
	"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
	"Message-ID: <m1@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: multipart/alternative; boundary=\"ALT\"\r\n" +
	"\r\n" +
	"--ALT\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Plain body line.\r\n" +
	"--ALT\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>HTML body</p>\r\n" +
	"--ALT--\r\n" +
	"--BOUND\r\n" +
	"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--BOUND--\r\n"

func TestParseMessageMultipart(t *testing.T) {
	msg, err := parseMessage([]byte(multipartRaw), true)
	if err != nil {
		t.Fatalf("parseMessage: %v", err)
	}
	if msg.Subject != "Héllo" {
		t.Errorf("subject = %q, want Héllo", msg.Subject)
	}
	if msg.MessageID != "m1@example.com" {
		t.Errorf("message-id = %q", msg.MessageID)
	}
	if len(msg.From) != 1 || msg.From[0].Email != "alice@example.com" {
		t.Errorf("from = %+v", msg.From)
	}
	if !strings.Contains(msg.BodyText, "Plain body line.") {
		t.Errorf("body text = %q", msg.BodyText)
	}
	if !strings.Contains(msg.BodyHTML, "<p>HTML body</p>") {
		t.Errorf("body html = %q", msg.BodyHTML)
	}
	if msg.Date.IsZero() {
		t.Error("date not parsed")
	}
}

func TestParseMessageHTMLOmittedByDefault(t *testing.T) {
	msg, err := parseMessage([]byte(multipartRaw), false)
	if err != nil {
		t.Fatalf("parseMessage: %v", err)
	}
	if msg.BodyHTML != "" {
		t.Errorf("body html = %q, want empty when includeHTML is false", msg.BodyHTML)
	}
	if !strings.Contains(msg.BodyText, "Plain body line.") {
		t.Errorf("body text = %q", msg.BodyText)
	}
}

func TestParseMessageTruncatesBody(t *testing.T) {
	big := strings.Repeat("x", MaxBodyBytes+5000)
	raw := "Subject: big\r\nContent-Type: text/plain\r\n\r\n" + big
	msg, err := parseMessage([]byte(raw), false)
	if err != nil {
		t.Fatalf("parseMessage: %v", err)
	}
	if len(msg.BodyText) > MaxBodyBytes {
		t.Fatalf("body len = %d, want <= %d", len(msg.BodyText), MaxBodyBytes)
	}
}

func TestAttachmentsFromBodyStructure(t *testing.T) {
	bs := &imap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []imap.BodyStructure{
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain"},
			&imap.BodyStructureSinglePart{
				Type: "application", Subtype: "pdf", Size: 1234,
				Params: map[string]string{"name": "report.pdf"},
				Extended: &imap.BodyStructureSinglePartExt{
					Disposition: &imap.BodyStructureDisposition{
						Value:  "attachment",
						Params: map[string]string{"filename": "report.pdf"},
					},
				},
			},
		},
	}
	atts := attachmentsFromBodyStructure(bs)
	if len(atts) != 1 {
		t.Fatalf("attachments = %+v", atts)
	}
	if atts[0].Filename != "report.pdf" || atts[0].ContentType != "application/pdf" || atts[0].Size != 1234 {
		t.Fatalf("attachment = %+v", atts[0])
	}
}

func TestSearchCriteriaTranslation(t *testing.T) {
	since := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	crit := searchCriteria(SearchQuery{
		Query: "invoice", From: "billing@example.com", Subject: "Receipt",
		Since: since, UnreadOnly: true,
	})
	if len(crit.Header) != 2 {
		t.Fatalf("headers = %+v", crit.Header)
	}
	if len(crit.Text) != 1 || crit.Text[0] != "invoice" {
		t.Errorf("text = %+v", crit.Text)
	}
	if !crit.Since.Equal(since) {
		t.Errorf("since = %v", crit.Since)
	}
	if len(crit.NotFlag) != 1 || crit.NotFlag[0] != imap.FlagSeen {
		t.Errorf("notflag = %+v", crit.NotFlag)
	}
}

func TestPresetsIncludeZohoRegions(t *testing.T) {
	ids := map[string]bool{}
	for _, p := range Presets() {
		ids[p.ID] = true
		if p.Host == "" && p.ID != "custom" {
			t.Errorf("preset %q has empty host", p.ID)
		}
	}
	for _, want := range []string{"gmail", "icloud", "zoho_in", "zoho_com", "zoho_eu", "custom"} {
		if !ids[want] {
			t.Errorf("missing preset %q", want)
		}
	}
	p, ok := LookupProvider("GMAIL")
	if !ok || p.Host != "imap.gmail.com" {
		t.Errorf("LookupProvider(gmail) = %+v, %v", p, ok)
	}
}
