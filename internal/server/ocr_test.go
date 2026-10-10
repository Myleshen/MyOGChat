package server

import (
	"context"
	"errors"
	"testing"

	"myogchat/internal/ocr"
	"myogchat/internal/store"
)

// ocrRecord captures what a bound keyed engine observed.
type ocrRecord struct {
	key, session string
	calls        int
	text         string
	err          error
}

// fakeKeyedOCR is an ocr.KeyedEngine that records the bound key/session.
type fakeKeyedOCR struct {
	rec          *ocrRecord
	key, session string
}

func (f *fakeKeyedOCR) OCR(_ context.Context, _ []byte) (string, error) {
	f.rec.key = f.key
	f.rec.session = f.session
	f.rec.calls++
	return f.rec.text, f.rec.err
}

func (f *fakeKeyedOCR) WithKey(apiKey, sessionID string) ocr.Engine {
	return &fakeKeyedOCR{rec: f.rec, key: apiKey, session: sessionID}
}

func TestOCRFuncNilWhenNoEngine(t *testing.T) {
	srv, _ := newKeyTestServer(t)
	if fn := srv.ocrFunc(context.Background(), store.User{}); fn != nil {
		t.Error("ocrFunc must be nil when no engine is configured")
	}
}

func TestOCRFuncBindsPerRequestKey(t *testing.T) {
	srv, st := newKeyTestServer(t)
	rec := &ocrRecord{text: "hello"}
	srv.ocrEngine = &fakeKeyedOCR{rec: rec}
	srv.ocrEngineName = "vision"
	srv.ocrVisionAvailable = true

	u := seedKeyUser(t, st, "u-1", "alice", "user-key")

	fn := srv.ocrFunc(context.Background(), u)
	if fn == nil {
		t.Fatal("ocrFunc returned nil for a configured keyed engine")
	}
	got, err := fn([]byte("image"))
	if err != nil || got != "hello" {
		t.Fatalf("ocr = %q, %v", got, err)
	}
	if rec.key != "user-key" {
		t.Errorf("bound key = %q, want user-key", rec.key)
	}
	if rec.session == "" {
		t.Error("session id should be bound for the vision call")
	}
}

func TestOCRFuncNoKeyErrors(t *testing.T) {
	srv, st := newKeyTestServer(t)
	rec := &ocrRecord{}
	srv.ocrEngine = &fakeKeyedOCR{rec: rec}

	u := seedKeyUser(t, st, "u-2", "bob", "") // no own key, no server key
	fn := srv.ocrFunc(context.Background(), u)
	if fn == nil {
		t.Fatal("ocrFunc returned nil")
	}
	if _, err := fn([]byte("image")); !errors.Is(err, errNoKey) {
		t.Fatalf("err = %v, want errNoKey", err)
	}
	if rec.calls != 0 {
		t.Errorf("engine ran %d times without a key, want 0", rec.calls)
	}
}

func TestOCRFuncDisabledBySetting(t *testing.T) {
	srv, _ := newKeyTestServer(t)
	rec := &ocrRecord{}
	srv.ocrEngine = &fakeKeyedOCR{rec: rec}
	if err := srv.setOCREnabled(context.Background(), false); err != nil {
		t.Fatalf("setOCREnabled: %v", err)
	}
	if fn := srv.ocrFunc(context.Background(), store.User{}); fn != nil {
		t.Error("ocrFunc must be nil when ocr_enabled is off")
	}
}

func TestOCRSettingsBlock(t *testing.T) {
	srv, _ := newKeyTestServer(t)
	srv.ocrEngine = &fakeKeyedOCR{rec: &ocrRecord{}}
	srv.ocrEngineName = "auto"
	srv.ocrVisionAvailable = true

	got := srv.ocrSettings(context.Background())
	if got["ocr_enabled"] != true || got["ocr_available"] != true {
		t.Errorf("settings = %+v", got)
	}
	if got["ocr_engine"] != "auto" || got["ocr_vision_available"] != true {
		t.Errorf("settings = %+v", got)
	}
}
