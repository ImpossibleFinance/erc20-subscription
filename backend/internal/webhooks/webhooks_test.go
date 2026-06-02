package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSend_NoURL_LogsOnly(t *testing.T) {
	s := NewSender("", "secret")
	if err := s.Send(context.Background(), EventCharged, map[string]string{"a": "b"}); err != nil {
		t.Error(err)
	}
}

func TestSend_SuccessSignsBody(t *testing.T) {
	secret := "abc"
	var gotSig, gotType, gotID string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Signature")
		gotType = r.Header.Get("X-Event-Type")
		gotID = r.Header.Get("X-Event-ID")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewSender(srv.URL, secret)
	if err := s.Send(context.Background(), EventCharged, map[string]string{"user": "0xA"}); err != nil {
		t.Fatal(err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if gotSig != wantSig {
		t.Errorf("sig=%s want %s", gotSig, wantSig)
	}
	if gotType != EventCharged {
		t.Errorf("type=%s", gotType)
	}
	if !strings.HasPrefix(gotID, "evt_") {
		t.Errorf("id=%s", gotID)
	}
}

func TestSend_4xxIsTerminal(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
	}))
	defer srv.Close()

	s := NewSender(srv.URL, "secret")
	err := s.Send(context.Background(), EventCharged, map[string]string{"a": "b"})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("4xx should not retry, got %d calls", calls)
	}
}

func TestSend_5xxRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewSender(srv.URL, "secret")
	if err := s.Send(context.Background(), EventCharged, map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (2 retries), got %d", calls)
	}
}
