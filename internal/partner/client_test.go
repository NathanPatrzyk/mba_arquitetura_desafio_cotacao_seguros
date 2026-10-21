package partner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

func testPartner(h http.Handler) (platform.Partner, func()) {
	server := httptest.NewServer(h)
	return platform.Partner{Name: "partner-flaky", BaseURL: server.URL}, server.Close
}

func TestQuoteReadsThePartnerResponse(t *testing.T) {
	var path, contentType string
	var received map[string]any

	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, contentType = r.URL.Path, r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"partner":"ignored","quote_id":"q-1","premium_cents":123456,` +
			`"currency":"BRL","coverage_cents":5000000,"valid_for_seconds":300}`))
	}))
	defer closeServer()

	quote, err := NewClient().Quote(context.Background(), p, map[string]string{"broker": "corretora-a"})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if path != "/quotes" {
		t.Errorf("path %q, want /quotes", path)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type %q, want application/json", contentType)
	}
	if received["broker"] != "corretora-a" {
		t.Errorf("partner received broker %v, want corretora-a", received["broker"])
	}
	if quote.PremiumCents != 123456 || quote.QuoteID != "q-1" {
		t.Errorf("quote read incorrectly: %+v", quote)
	}
	if quote.Partner != "partner-flaky" {
		t.Errorf("partner %q, want partner-flaky", quote.Partner)
	}
}

func TestQuoteIdentifiesThePartnerThatFailed(t *testing.T) {
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"partner unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer closeServer()

	_, err := NewClient().Quote(context.Background(), p, map[string]string{})
	if err == nil {
		t.Fatal("a 503 from the partner was treated as success")
	}

	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a *partner.Error", err)
	}
	if failure.Partner != "partner-flaky" || failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("error does not identify the failure: %+v", failure)
	}
}

func TestQuoteFailsOnUnreadableResponse(t *testing.T) {
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer closeServer()

	if _, err := NewClient().Quote(context.Background(), p, map[string]string{}); err == nil {
		t.Fatal("unreadable response was accepted")
	}
}

func TestQuoteHonoursCancellation(t *testing.T) {
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer closeServer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewClient().Quote(ctx, p, map[string]string{}); err == nil {
		t.Fatal("cancelled context did not interrupt the call")
	}
}

func clientWithTimeout(timeout time.Duration) *Client {
	client := NewClient()
	client.timeout = timeout
	return client
}

func TestQuoteReportsTimeoutAsPartnerError(t *testing.T) {
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer closeServer()

	_, err := clientWithTimeout(50*time.Millisecond).Quote(context.Background(), p, map[string]string{})

	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a *partner.Error", err)
	}
	if !failure.Timeout {
		t.Fatalf("timeout was not flagged: %+v", failure)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error does not unwrap to context.DeadlineExceeded: %v", err)
	}
}

func TestQuoteDoesNotFlagCancellationAsTimeout(t *testing.T) {
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer closeServer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := clientWithTimeout(time.Second).Quote(ctx, p, map[string]string{})

	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a *partner.Error", err)
	}
	if failure.Timeout {
		t.Fatalf("cancellation must not be flagged as timeout: %+v", failure)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not unwrap to context.Canceled: %v", err)
	}
}

func TestQuoteWithinTimeoutIsNotAnError(t *testing.T) {
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quote_id":"q-1","premium_cents":1000,"currency":"BRL","coverage_cents":5000,"valid_for_seconds":300}`))
	}))
	defer closeServer()

	quote, err := clientWithTimeout(time.Second).Quote(context.Background(), p, map[string]string{})
	if err != nil {
		t.Fatalf("a call within the timeout failed: %v", err)
	}
	if quote.QuoteID != "q-1" || quote.Partner != "partner-flaky" {
		t.Fatalf("unexpected quote: %+v", quote)
	}
}

func TestNewClientUsesTheTwoSecondTimeout(t *testing.T) {
	if got := NewClient().timeout; got != 2*time.Second {
		t.Fatalf("default timeout = %s, want 2s", got)
	}
}
