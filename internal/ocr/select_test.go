package ocr

import (
	"context"
	"errors"
	"testing"
)

// fakeEngine is a scripted Engine that records the bytes it was given.
type fakeEngine struct {
	text string
	err  error
	got  []byte
	ran  bool
}

func (f *fakeEngine) OCR(_ context.Context, image []byte) (string, error) {
	f.ran = true
	f.got = image
	return f.text, f.err
}

// keyedFake records the key/session bound via WithKey.
type keyedFake struct {
	text    string
	err     error
	key     string
	session string
}

func (k *keyedFake) OCR(_ context.Context, _ []byte) (string, error) { return k.text, k.err }

func (k *keyedFake) WithKey(apiKey, sessionID string) Engine {
	return &keyedFake{text: k.text, err: k.err, key: apiKey, session: sessionID}
}

func TestFallbackEnginePrimarySuccess(t *testing.T) {
	primary := &fakeEngine{text: "primary"}
	secondary := &fakeEngine{text: "secondary"}
	f := &FallbackEngine{Primary: primary, Secondary: secondary}

	got, err := f.OCR(context.Background(), []byte("img"))
	if err != nil || got != "primary" {
		t.Fatalf("OCR = %q, %v; want primary, nil", got, err)
	}
	if secondary.ran {
		t.Error("secondary must not run when the primary succeeds")
	}
}

func TestFallbackEngineOnUnavailable(t *testing.T) {
	primary := &fakeEngine{err: ErrUnavailable}
	secondary := &fakeEngine{text: "secondary"}
	f := &FallbackEngine{Primary: primary, Secondary: secondary}

	got, err := f.OCR(context.Background(), []byte("img"))
	if err != nil || got != "secondary" {
		t.Fatalf("OCR = %q, %v; want secondary, nil", got, err)
	}
}

func TestFallbackEngineNoFallbackOnOtherErrors(t *testing.T) {
	primary := &fakeEngine{err: errors.New("blank image")}
	secondary := &fakeEngine{text: "secondary"}
	f := &FallbackEngine{Primary: primary, Secondary: secondary}

	if _, err := f.OCR(context.Background(), []byte("img")); err == nil {
		t.Fatal("expected the primary error to propagate")
	}
	if secondary.ran {
		t.Error("secondary must not run for a non-unavailability error")
	}
}

func TestFallbackEngineUnavailableWithoutSecondary(t *testing.T) {
	f := &FallbackEngine{Primary: &fakeEngine{err: ErrUnavailable}}
	if _, err := f.OCR(context.Background(), []byte("img")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestFallbackEngineWithKeyPropagates(t *testing.T) {
	f := &FallbackEngine{
		Primary:   &keyedFake{text: "p"},
		Secondary: &keyedFake{text: "s"},
	}
	bound, ok := f.WithKey("k-1", "sess-1").(*FallbackEngine)
	if !ok {
		t.Fatalf("WithKey returned %T", f.WithKey("a", "b"))
	}
	got, err := bound.OCR(context.Background(), []byte("img"))
	if err != nil || got != "p" {
		t.Fatalf("OCR = %q, %v", got, err)
	}
	if bound.Primary.(*keyedFake).key != "k-1" || bound.Primary.(*keyedFake).session != "sess-1" {
		t.Errorf("primary key not bound: %+v", bound.Primary)
	}
}

func TestSelectEngine(t *testing.T) {
	tess := &fakeEngine{text: "t"}
	vis := &fakeEngine{text: "v"}

	if got := SelectEngine("tesseract", tess, vis); got != Engine(tess) {
		t.Errorf("tesseract mode = %T", got)
	}
	if got := SelectEngine("vision", tess, vis); got != Engine(vis) {
		t.Errorf("vision mode = %T", got)
	}
	if got := SelectEngine("bogus", tess, vis); got != Engine(tess) {
		t.Errorf("unknown mode should default to tesseract, got %T", got)
	}

	// auto with both: a fallback (tess primary, vision secondary).
	fb, ok := SelectEngine("auto", tess, vis).(*FallbackEngine)
	if !ok || fb.Primary != Engine(tess) || fb.Secondary != Engine(vis) {
		t.Errorf("auto with both = %#v", SelectEngine("auto", tess, vis))
	}
	// auto with no tesseract (preflight failed): vision only.
	if got := SelectEngine("auto", nil, vis); got != Engine(vis) {
		t.Errorf("auto without tesseract = %T", got)
	}
	// auto with no vision: tesseract only.
	if got := SelectEngine("auto", tess, nil); got != Engine(tess) {
		t.Errorf("auto without vision = %T", got)
	}
	if got := SelectEngine("auto", nil, nil); got != nil {
		t.Errorf("auto with nothing = %v, want nil", got)
	}
}
