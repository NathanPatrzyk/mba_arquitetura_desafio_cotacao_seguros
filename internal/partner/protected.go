package partner

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/sony/gobreaker/v2"

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
	for _, p := range partners {
		breakers[p.Name] = gobreaker.NewCircuitBreaker[Quote](breakerSettings(p.Name))
	}
	return &ProtectedClient{next: next, breakers: breakers}
}

func breakerSettings(name string) gobreaker.Settings {
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
		return Quote{}, &Error{Partner: p.Name, Reason: "circuit open", CircuitOpen: true, Cause: err}
	}
	return quote, err
}
