package quotation

import (
	"context"
	"sort"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type Quoter interface {
	Quote(ctx context.Context, p platform.Partner, request any) (partner.Quote, error)
}

type Option func(*Service)

func WithCache(cache *QuoteCache) Option {
	return func(s *Service) { s.cache = cache }
}

type Service struct {
	partners []platform.Partner
	quoter   Quoter
	cache    *QuoteCache
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

	quotes := make([]partner.Quote, 0, len(s.partners))
	for _, p := range s.partners {
		quote, err := s.quoteFrom(ctx, tenant, p, request, forPartner)
		if err != nil {
			return Response{}, err
		}
		quotes = append(quotes, quote)
	}

	sort.Slice(quotes, func(i, j int) bool {
		return quotes[i].PremiumCents < quotes[j].PremiumCents
	})

	return Response{
		TenantID:  tenant,
		Quotes:    quotes,
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
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
