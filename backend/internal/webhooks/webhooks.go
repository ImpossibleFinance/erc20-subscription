// Package webhooks emits HMAC-signed JSON events to a configured URL. Designed
// to look enough like Stripe webhooks that integrators can reuse familiar
// patterns: a stable event envelope, X-Signature header with a hex HMAC, retry
// on 5xx with bounded backoff.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

const (
	EventCharged           = "subscription.charged"
	EventPaymentFailed     = "subscription.payment_failed"
	EventCancelled         = "subscription.cancelled"
	EventAllowanceLow      = "subscription.allowance_low"
	EventAllowanceRequired = "subscription.allowance_required"
	EventProratedCharge    = "subscription.prorated_charge"

	// Operational events — for the admin / ops team, not for end users.
	EventOperatorGasLow  = "operator.gas_low"
	EventOperatorTxStuck = "operator.tx_stuck"
)

type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

type Sender struct {
	url    string
	secret []byte
	client *http.Client
}

func NewSender(url, secret string) *Sender {
	return &Sender{
		url:    url,
		secret: []byte(secret),
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Send marshals an event and POSTs it with HMAC-SHA256 signature. If url is
// empty, logs the event and returns nil — useful for local dev. Retries 5xx
// up to 3 times with linear backoff; 4xx is terminal (your endpoint rejected
// the payload, retrying won't help).
func (s *Sender) Send(ctx context.Context, evType string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal data: %w", err)
	}
	ev := Event{
		ID:        newID(),
		Type:      evType,
		CreatedAt: time.Now().UTC(),
		Data:      raw,
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	if s.url == "" {
		log.Printf("webhooks: (no URL configured) would send %s id=%s body=%s", evType, ev.ID, body)
		return nil
	}

	mac := hmac.New(sha256.New, s.secret)
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", s.url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", sig)
		req.Header.Set("X-Event-ID", ev.ID)
		req.Header.Set("X-Event-Type", ev.Type)

		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return fmt.Errorf("webhook 4xx (terminal): %d", resp.StatusCode)
		}
		lastErr = fmt.Errorf("webhook %d", resp.StatusCode)
	}
	return lastErr
}

func newID() string {
	var buf [12]byte
	now := time.Now().UTC().UnixNano()
	for i := 0; i < 8; i++ {
		buf[i] = byte(now >> (8 * i))
	}
	return "evt_" + hex.EncodeToString(buf[:])
}
