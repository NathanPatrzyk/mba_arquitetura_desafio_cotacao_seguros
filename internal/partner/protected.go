package partner

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/sony/gobreaker/v2"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

const (
	breakerInterval     = 30 * time.Second
	breakerBucketPeriod = 3 * time.Second
	breakerOpenTimeout  = 30 * time.Second
	breakerProbes       = 3
	breakerMinFailures  = 5
)

type Caller interface {
	Quote(ctx context.Context, p platform.Partner, request any) (Quote, error)
}

type ProtectedClient struct {
	next     Caller
	breakers map[string]*gobreaker.CircuitBreaker[Quote]
}

func NewProtectedClient(next Caller, partners []platform.Partner) *ProtectedClient {
	breakers := make(map[string]*gobreaker.CircuitBreaker[Quote], len(partners))

	transitions, err := otel.Meter("quotation-api/partner").
		Int64Counter(
			"circuit_breaker.transitions",
			metric.WithDescription("Circuit breaker state transitions by partner and state"),
		)
	if err != nil {
		log.Printf("circuit breaker transitions metric unavailable: %v", err)
		transitions = nil
	}

	for _, p := range partners {
		breakers[p.Name] = gobreaker.NewCircuitBreaker[Quote](breakerSettings(p.Name, transitions))
	}
	return &ProtectedClient{next: next, breakers: breakers}
}

func breakerSettings(name string, transitions metric.Int64Counter) gobreaker.Settings {
	return gobreaker.Settings{
		Name:         name,
		MaxRequests:  breakerProbes,
		Interval:     breakerInterval,
		BucketPeriod: breakerBucketPeriod,
		Timeout:      breakerOpenTimeout,
		ReadyToTrip:  shouldTrip,
		IsExcluded:   isCanceled,
		OnStateChange: func(name string, from, to gobreaker.State) {
			log.Printf("breaker %s: %s -> %s", name, from, to)

			if transitions != nil {
				transitions.Add(
					context.Background(),
					1,
					metric.WithAttributes(
						attribute.String("partner", name),
						attribute.String("from", from.String()),
						attribute.String("to", to.String()),
					),
				)
			}
		},
	}
}

func shouldTrip(counts gobreaker.Counts) bool {
	completed := counts.TotalSuccesses + counts.TotalFailures
	return counts.TotalFailures >= breakerMinFailures && counts.TotalFailures*2 >= completed
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

func (c *ProtectedClient) State(partner string) (gobreaker.State, bool) {
	breaker, ok := c.breakers[partner]
	if !ok {
		return gobreaker.StateClosed, false
	}
	return breaker.State(), true
}

func (c *ProtectedClient) Quote(ctx context.Context, p platform.Partner, request any) (Quote, error) {
	breaker, ok := c.breakers[p.Name]
	if !ok {
		return c.next.Quote(ctx, p, request)
	}

	quote, err := breaker.Execute(func() (Quote, error) {
		return c.next.Quote(ctx, p, request)
	})

	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		state := "open"
		reason := "open_state"

		if errors.Is(err, gobreaker.ErrTooManyRequests) {
			state = "half-open"
			reason = "too_many_requests"
		}

		trace.SpanFromContext(ctx).AddEvent(
			"circuit_breaker_rejected",
			trace.WithAttributes(
				attribute.String("partner", p.Name),
				attribute.String("circuit_breaker.state", state),
				attribute.String("reason", reason),
			),
		)

		return Quote{}, &Error{
			Partner:     p.Name,
			Reason:      "circuit open",
			CircuitOpen: true,
			Cause:       err,
		}
	}
	return quote, err
}
