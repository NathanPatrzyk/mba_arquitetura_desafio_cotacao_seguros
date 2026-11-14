package quotation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

var ErrNoQuotes = errors.New("no quotes available")

type Quoter interface {
	Quote(ctx context.Context, p platform.Partner, request any) (partner.Quote, error)
}

type Option func(*Service)

func WithCache(cache *QuoteCache) Option {
	return func(s *Service) { s.cache = cache }
}

func WithPartialFallback() Option {
	return func(s *Service) { s.partial = true }
}

func WithParallelQuotes() Option {
	return func(s *Service) { s.parallel = true }
}

type Service struct {
	partners []platform.Partner
	quoter   Quoter
	cache    *QuoteCache
	partial  bool
	parallel bool
}

type partnerResult struct {
	quote partner.Quote
	err   error
}

func NewService(partners []platform.Partner, quoter Quoter, options ...Option) *Service {
	s := &Service{partners: partners, quoter: quoter}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Service) Quote(ctx context.Context, tenant string, request Request) (Response, error) {
	start := time.Now()
	forPartner := partnerRequest{Broker: tenant, Request: request}

	var results []partnerResult
	if s.parallel {
		results = s.collectInParallel(ctx, tenant, request, forPartner)
	} else {
		results = s.collectInSeries(ctx, tenant, request, forPartner)
	}

	quotes, missing, err := s.assemble(ctx, results)
	if err != nil {
		return Response{}, err
	}
	if len(quotes) == 0 {
		return Response{}, ErrNoQuotes
	}

	sort.Slice(quotes, func(i, j int) bool {
		return quotes[i].PremiumCents < quotes[j].PremiumCents
	})

	return Response{
		TenantID:  tenant,
		Quotes:    quotes,
		Degraded:  len(missing) > 0,
		Missing:   missing,
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
}

func (s *Service) collectInSeries(ctx context.Context, tenant string, request Request, forPartner partnerRequest) []partnerResult {
	results := make([]partnerResult, 0, len(s.partners))
	
	for _, p := range s.partners {
		quote, err := s.quoteFrom(ctx, tenant, p, request, forPartner)
		results = append(results, partnerResult{quote: quote, err: err})
		if err != nil && (!s.partial || ctx.Err() != nil) {
			break
		}
	}
	
	return results
}

func (s *Service) collectInParallel(ctx context.Context, tenant string, request Request, forPartner partnerRequest) []partnerResult {
	results := make([]partnerResult, len(s.partners))

	var wg sync.WaitGroup
	for i, p := range s.partners {
		wg.Go(func() {
			results[i] = s.quoteGuarded(ctx, tenant, p, request, forPartner)
		})
	}
	wg.Wait()

	return results
}

func (s *Service) quoteGuarded(ctx context.Context, tenant string, p platform.Partner, request Request, forPartner partnerRequest) (result partnerResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = partnerResult{err: &partner.Error{Partner: p.Name, Reason: fmt.Sprintf("panic: %v", recovered)}}
		}
	}()

	quote, err := s.quoteFrom(ctx, tenant, p, request, forPartner)
	return partnerResult{quote: quote, err: err}
}

func (s *Service) assemble(ctx context.Context, results []partnerResult) ([]partner.Quote, []string, error) {
	quotes := make([]partner.Quote, 0, len(results))
	missing := make([]string, 0, len(results))

	for i, result := range results {
		if result.err == nil {
			quotes = append(quotes, result.quote)
			continue
		}
		if !s.partial {
			return nil, nil, result.err
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		missing = append(missing, s.partners[i].Name)
	}
	
	return quotes, missing, nil
}

func (s *Service) quoteFrom(ctx context.Context, tenant string, p platform.Partner, request Request, forPartner partnerRequest) (partner.Quote, error) {
	if s.cache != nil {
		if cached, ok := s.cache.Get(ctx, tenant, p.Name, request); ok {
			return cached, nil
		}
	}

	quote, err := s.quoter.Quote(ctx, p, forPartner)
	if err != nil {
		return partner.Quote{}, err
	}
	quote.Source = partner.SourcePartner

	if s.cache != nil {
		s.cache.Set(ctx, tenant, p.Name, request, quote)
	}
	return quote, nil
}
