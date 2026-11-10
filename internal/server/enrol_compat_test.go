package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyEnrolPathsRedirectToEnroll(t *testing.T) {
	hn := newHarness(t)
	for in, want := range map[string]string{
		"/enrol":                         "/enroll",
		"/enrol?next=x":                  "/enroll?next=x",
		"/enrol/callback?code=a&state=b": "/enroll/callback?code=a&state=b",
		"/enrol/static/enroll.js":        "/enroll/static/enroll.js",
	} {
		rec := httptest.NewRecorder()
		hn.h.ServeHTTP(rec, httptest.NewRequest("GET", in, nil))
		if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != want {
			t.Errorf("GET %s: %d -> %q, want 308 -> %q", in, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}

func TestParseConfigAcceptsDeprecatedEnrolKey(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Writer())

	var cfg Config
	if err := ParseConfig([]byte(`{"enrol":{"voiceUrl":"http://v/enrol","voiceProducer":"mic"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Enroll.VoiceURL != "http://v/enrol" || cfg.Enroll.VoiceProducer != "mic" {
		t.Fatalf("old key not honored: %+v", cfg.Enroll)
	}
	if n := strings.Count(buf.String(), "deprecated"); n != 1 {
		t.Fatalf("want one deprecation line, got %d: %q", n, buf.String())
	}

	buf.Reset()
	cfg = Config{}
	if err := ParseConfig([]byte(`{"enroll":{"voiceUrl":"new"},"enrol":{"voiceUrl":"old"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Enroll.VoiceURL != "new" {
		t.Fatalf("enroll must win: %+v", cfg.Enroll)
	}
	buf.Reset()
	cfg = Config{}
	if err := ParseConfig([]byte(`{"enroll":{"voiceUrl":"new"}}`), &cfg); err != nil || buf.Len() != 0 || cfg.Enroll.VoiceURL != "new" {
		t.Fatalf("new key: err=%v log=%q cfg=%+v", err, buf.String(), cfg.Enroll)
	}
}
