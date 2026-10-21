package partner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

const defaultTimeout = 2 * time.Second

type Quote struct {
	Partner         string `json:"partner"`
	QuoteID         string `json:"quote_id"`
	PremiumCents    int64  `json:"premium_cents"`
	Currency        string `json:"currency"`
	CoverageCents   int64  `json:"coverage_cents"`
	ValidForSeconds int64  `json:"valid_for_seconds"`
}

type Client struct {
	http    *http.Client
	timeout time.Duration
}

func NewClient() *Client {
	return &Client{
		http:    &http.Client{Transport: platform.InstrumentTransport(http.DefaultTransport)},
		timeout: defaultTimeout,
	}
}

const responseLimit = 1 << 20

func (c *Client) Quote(ctx context.Context, p platform.Partner, request any) (Quote, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body, err := json.Marshal(request)
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: fmt.Sprintf("invalid request: %v", err), Cause: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/quotes", bytes.NewReader(body))
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: err.Error(), Cause: err}
	}
	req.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(req)
	if err != nil {
		return Quote{}, c.failure(p.Name, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, responseLimit))
		return Quote{}, &Error{
			Partner: p.Name,
			Status:  response.StatusCode,
			Reason:  fmt.Sprintf("partner replied %d", response.StatusCode),
		}
	}

	var quote Quote
	if err := json.NewDecoder(io.LimitReader(response.Body, responseLimit)).Decode(&quote); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Quote{}, c.failure(p.Name, err)
		}
		return Quote{}, &Error{Partner: p.Name, Reason: fmt.Sprintf("unreadable response: %v", err), Cause: err}
	}

	quote.Partner = p.Name
	return quote, nil
}

func (c *Client) failure(partner string, err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{
			Partner: partner,
			Reason:  fmt.Sprintf("timeout after %s", c.timeout),
			Timeout: true,
			Cause:   err,
		}
	}
	return &Error{Partner: partner, Reason: err.Error(), Cause: err}
}

type Error struct {
	Partner     string
	Status      int
	Reason      string
	Timeout     bool
	CircuitOpen bool
	Cause       error
}

func (e *Error) Error() string {
	return fmt.Sprintf("partner %s: %s", e.Partner, e.Reason)
}

func (e *Error) Unwrap() error {
	return e.Cause
}
