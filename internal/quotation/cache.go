package quotation

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
)

const (
	CacheTTL     = time.Hour
	keyPrefix    = "prumo:quote:v1"
	storeTimeout = 200 * time.Millisecond
)

type Store interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

type QuoteCache struct {
	store  Store
	ttl    time.Duration
	now    func() time.Time
	hits   metric.Int64Counter
	misses metric.Int64Counter
}

type cacheEntry struct {
	Quote    partner.Quote `json:"quote"`
	StoredAt time.Time     `json:"stored_at"`
}

func NewQuoteCache(store Store, ttl time.Duration) (*QuoteCache, error) {
	meter := otel.Meter("quotation-api/cache")

	hits, err := meter.Int64Counter("cache.hits", metric.WithDescription("Partner premiums served from the cache"))
	if err != nil {
		return nil, fmt.Errorf("cache.hits counter: %w", err)
	}
	misses, err := meter.Int64Counter("cache.misses", metric.WithDescription("Partner premiums not found in the cache"))
	if err != nil {
		return nil, fmt.Errorf("cache.misses counter: %w", err)
	}

	return &QuoteCache{store: store, ttl: ttl, now: time.Now, hits: hits, misses: misses}, nil
}

var keyEscaper = strings.NewReplacer("%", "%25", ":", "%3A")

func CacheKey(tenant, partnerName string, r Request) string {
	return fmt.Sprintf(
		"%s:tenant=%s:partner=%s:request.coverage=%s:driver.document=%s:driver.birth_year=%d:vehicle.plate=%s:vehicle.model=%s:vehicle.year=%d:vehicle.value_cents=%d",
		keyPrefix,
		keyEscaper.Replace(tenant),
		keyEscaper.Replace(partnerName),
		keyEscaper.Replace(r.Coverage),
		keyEscaper.Replace(r.Driver.Document),
		r.Driver.BirthYear,
		keyEscaper.Replace(r.Vehicle.Plate),
		keyEscaper.Replace(r.Vehicle.Model),
		r.Vehicle.Year,
		r.Vehicle.ValueCents,
	)
}

func (c *QuoteCache) Get(ctx context.Context, tenant, partnerName string, request Request) (partner.Quote, bool) {
	readCtx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()

	raw, found, err := c.store.Get(readCtx, CacheKey(tenant, partnerName, request))
	if err != nil {
		log.Printf("quotation-api: cache read failed for %s: %v", partnerName, err)
		c.recordMiss(ctx, tenant, partnerName)
		return partner.Quote{}, false
	}
	if !found {
		c.recordMiss(ctx, tenant, partnerName)
		return partner.Quote{}, false
	}

	var entry cacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		log.Printf("quotation-api: cache entry unreadable for %s: %v", partnerName, err)
		c.recordMiss(ctx, tenant, partnerName)
		return partner.Quote{}, false
	}

	elapsed := max(c.now().Sub(entry.StoredAt), 0)
	if elapsed >= c.ttl {
		c.recordMiss(ctx, tenant, partnerName)
		return partner.Quote{}, false
	}

	age := int64(elapsed / time.Second)
	quote := entry.Quote
	quote.Source = partner.SourceCache
	quote.CacheAgeSeconds = &age

	c.recordHit(ctx, tenant, partnerName)
	return quote, true
}

func (c *QuoteCache) Set(ctx context.Context, tenant, partnerName string, request Request, quote partner.Quote) {
	raw, err := json.Marshal(cacheEntry{Quote: quote, StoredAt: c.now().UTC()})
	if err != nil {
		log.Printf("quotation-api: cache entry not encodable for %s: %v", partnerName, err)
		return
	}

	writeCtx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()

	if err := c.store.Set(writeCtx, CacheKey(tenant, partnerName, request), raw, c.ttl); err != nil {
		log.Printf("quotation-api: cache write failed for %s: %v", partnerName, err)
	}
}

func (c *QuoteCache) recordHit(ctx context.Context, tenant, partnerName string) {
	c.hits.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant_id", tenant),
		attribute.String("partner", partnerName),
	))
	trace.SpanFromContext(ctx).AddEvent("cache_hit", trace.WithAttributes(
		attribute.String("partner", partnerName),
	))
}

func (c *QuoteCache) recordMiss(ctx context.Context, tenant, partnerName string) {
	c.misses.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant_id", tenant),
		attribute.String("partner", partnerName),
	))
}
