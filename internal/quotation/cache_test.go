package quotation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type memStore struct {
	mu      sync.Mutex
	data    map[string][]byte
	lastTTL time.Duration
	failGet bool
	failSet bool
}

func newMemStore() *memStore {
	return &memStore{data: map[string][]byte{}}
}

func (m *memStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet {
		return nil, false, errors.New("redis down")
	}
	value, ok := m.data[key]
	return value, ok, nil
}

func (m *memStore) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSet {
		return errors.New("redis down")
	}
	m.data[key] = value
	m.lastTTL = ttl
	return nil
}

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

func (c *fixedClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type countingQuoter struct {
	mu    sync.Mutex
	calls map[string]int
}

func newCountingQuoter() *countingQuoter {
	return &countingQuoter{calls: map[string]int{}}
}

func (q *countingQuoter) Quote(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls[p.Name]++
	return partner.Quote{
		Partner:         p.Name,
		QuoteID:         p.Name + "-id",
		PremiumCents:    1000 + int64(len(p.Name)),
		Currency:        "BRL",
		CoverageCents:   5000000,
		ValidForSeconds: 300,
	}, nil
}

func (q *countingQuoter) total() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	sum := 0
	for _, n := range q.calls {
		sum += n
	}
	return sum
}

func cacheTestRequest() Request {
	return Request{
		Driver:   Driver{Document: "12345678900", BirthYear: 1985},
		Vehicle:  Vehicle{Plate: "ABC1D23", Model: "Onix", Year: 2020, ValueCents: 6500000},
		Coverage: "comprehensive",
	}
}

func cacheTestPartners() []platform.Partner {
	return []platform.Partner{
		{Name: "partner-slow", BaseURL: "http://slow"},
		{Name: "partner-flaky", BaseURL: "http://flaky"},
		{Name: "partner-degrading", BaseURL: "http://degrading"},
	}
}

func newTestCache(t *testing.T, store Store, clock *fixedClock) *QuoteCache {
	t.Helper()
	cache, err := NewQuoteCache(store, CacheTTL)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	cache.now = clock.Now
	return cache
}

func TestCacheKeyIsExactlyTheDocumentedFormat(t *testing.T) {
	got := CacheKey("corretora-a", "partner-slow", cacheTestRequest())
	want := "prumo:quote:v1:tenant=corretora-a:partner=partner-slow:request.coverage=comprehensive:driver.document=12345678900:driver.birth_year=1985:vehicle.plate=ABC1D23:vehicle.model=Onix:vehicle.year=2020:vehicle.value_cents=6500000"
	if got != want {
		t.Fatalf("key = %s\nwant  %s", got, want)
	}
}

func TestCacheKeyDiffersByTenantPartnerAndRisk(t *testing.T) {
	base := CacheKey("corretora-a", "partner-slow", cacheTestRequest())

	otherTenant := CacheKey("corretora-b", "partner-slow", cacheTestRequest())
	otherPartner := CacheKey("corretora-a", "partner-flaky", cacheTestRequest())
	changed := cacheTestRequest()
	changed.Vehicle.ValueCents++
	otherRisk := CacheKey("corretora-a", "partner-slow", changed)

	for name, key := range map[string]string{"tenant": otherTenant, "partner": otherPartner, "risk": otherRisk} {
		if key == base {
			t.Fatalf("key must differ by %s", name)
		}
	}
}

func TestCacheKeyKeepsSeparatorsUnambiguous(t *testing.T) {
	request := cacheTestRequest()
	request.Vehicle.Model = "Onix:vehicle.year=1999:%"

	key := CacheKey("corretora-a", "partner-slow", request)
	if got := strings.Count(key, ":"); got != 11 {
		t.Fatalf("key has %d separators, want 11: %s", got, key)
	}
}

func TestCacheReturnsStoredQuoteWithOriginAndAge(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	cache := newTestCache(t, newMemStore(), clock)
	ctx := context.Background()

	stored := partner.Quote{Partner: "partner-slow", QuoteID: "partner-slow-abc", PremiumCents: 84210, Currency: "BRL"}
	cache.Set(ctx, "corretora-a", "partner-slow", cacheTestRequest(), stored)

	clock.Advance(942 * time.Second)
	got, ok := cache.Get(ctx, "corretora-a", "partner-slow", cacheTestRequest())
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if got.Source != partner.SourceCache || got.CacheAgeSeconds == nil || *got.CacheAgeSeconds != 942 {
		t.Fatalf("unexpected origin or age: %+v", got)
	}
	if got.QuoteID != "partner-slow-abc" || got.PremiumCents != 84210 {
		t.Fatalf("original quote was not preserved: %+v", got)
	}
}

func TestCacheNeverServesAnotherTenant(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	cache := newTestCache(t, newMemStore(), clock)
	ctx := context.Background()

	cache.Set(ctx, "corretora-a", "partner-slow", cacheTestRequest(), partner.Quote{Partner: "partner-slow"})

	if _, ok := cache.Get(ctx, "corretora-b", "partner-slow", cacheTestRequest()); ok {
		t.Fatal("tenant corretora-b received an entry that belongs to corretora-a")
	}
}

func TestCacheWritesWithTheConfiguredTTL(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	store := newMemStore()
	cache := newTestCache(t, store, clock)

	cache.Set(context.Background(), "corretora-a", "partner-slow", cacheTestRequest(), partner.Quote{})

	if store.lastTTL != time.Hour {
		t.Fatalf("ttl = %s, want 1h", store.lastTTL)
	}
}

func TestCacheDoesNotServeExpiredEntry(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	cache := newTestCache(t, newMemStore(), clock)
	ctx := context.Background()

	cache.Set(ctx, "corretora-a", "partner-slow", cacheTestRequest(), partner.Quote{})
	clock.Advance(CacheTTL)

	if _, ok := cache.Get(ctx, "corretora-a", "partner-slow", cacheTestRequest()); ok {
		t.Fatal("entry older than the TTL must be a miss")
	}
}

func TestCacheFailsOpenWhenStoreIsDown(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	store := newMemStore()
	store.failGet = true
	store.failSet = true
	cache := newTestCache(t, store, clock)
	ctx := context.Background()

	cache.Set(ctx, "corretora-a", "partner-slow", cacheTestRequest(), partner.Quote{})
	if _, ok := cache.Get(ctx, "corretora-a", "partner-slow", cacheTestRequest()); ok {
		t.Fatal("a failing store must behave as a miss")
	}
}

func TestServiceServesRepeatedQuoteFromCache(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	cache := newTestCache(t, newMemStore(), clock)
	quoter := newCountingQuoter()
	service := NewService(cacheTestPartners(), quoter, WithCache(cache))
	ctx := context.Background()

	first, err := service.Quote(ctx, "corretora-a", cacheTestRequest())
	if err != nil {
		t.Fatalf("first quote: %v", err)
	}
	if len(first.Quotes) != 3 || quoter.total() != 3 {
		t.Fatalf("first quote: %d proposals, %d partner calls", len(first.Quotes), quoter.total())
	}
	for _, q := range first.Quotes {
		if q.Source != partner.SourcePartner || q.CacheAgeSeconds != nil {
			t.Fatalf("fresh proposal must come from the partner: %+v", q)
		}
	}

	clock.Advance(90 * time.Second)
	second, err := service.Quote(ctx, "corretora-a", cacheTestRequest())
	if err != nil {
		t.Fatalf("second quote: %v", err)
	}
	if quoter.total() != 3 {
		t.Fatalf("partners were called again: %d calls", quoter.total())
	}
	for _, q := range second.Quotes {
		if q.Source != partner.SourceCache || q.CacheAgeSeconds == nil || *q.CacheAgeSeconds != 90 {
			t.Fatalf("proposal must come from the cache with age 90: %+v", q)
		}
		if q.QuoteID != q.Partner+"-id" {
			t.Fatalf("original quote_id was not preserved: %+v", q)
		}
	}
}

func TestServiceBuysAgainForAnotherTenant(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	cache := newTestCache(t, newMemStore(), clock)
	quoter := newCountingQuoter()
	service := NewService(cacheTestPartners(), quoter, WithCache(cache))
	ctx := context.Background()

	if _, err := service.Quote(ctx, "corretora-a", cacheTestRequest()); err != nil {
		t.Fatal(err)
	}
	other, err := service.Quote(ctx, "corretora-b", cacheTestRequest())
	if err != nil {
		t.Fatal(err)
	}

	if quoter.total() != 6 {
		t.Fatalf("partner calls = %d, want 6", quoter.total())
	}
	for _, q := range other.Quotes {
		if q.Source != partner.SourcePartner {
			t.Fatalf("corretora-b must not be served from corretora-a's cache: %+v", q)
		}
	}
}

func TestServiceKeepsWorkingWhenCacheIsDown(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	store := newMemStore()
	store.failGet = true
	store.failSet = true
	quoter := newCountingQuoter()
	service := NewService(cacheTestPartners(), quoter, WithCache(newTestCache(t, store, clock)))

	for i := 0; i < 2; i++ {
		response, err := service.Quote(context.Background(), "corretora-a", cacheTestRequest())
		if err != nil || len(response.Quotes) != 3 {
			t.Fatalf("quote %d: err=%v proposals=%d", i, err, len(response.Quotes))
		}
	}
	if quoter.total() != 6 {
		t.Fatalf("partner calls = %d, want 6", quoter.total())
	}
}

func TestCacheTreatsAnUnreadableEntryAsAMiss(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	store := newMemStore()
	cache := newTestCache(t, store, clock)
	store.data[CacheKey("corretora-a", "partner-slow", cacheTestRequest())] = []byte("not json")

	if _, ok := cache.Get(context.Background(), "corretora-a", "partner-slow", cacheTestRequest()); ok {
		t.Fatal("an entry that cannot be decoded must be a miss")
	}
}

func TestCacheReportsZeroAgeWhenTheClockGoesBack(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	cache := newTestCache(t, newMemStore(), clock)
	ctx := context.Background()

	cache.Set(ctx, "corretora-a", "partner-slow", cacheTestRequest(), partner.Quote{Partner: "partner-slow"})
	clock.Advance(-30 * time.Second)

	got, ok := cache.Get(ctx, "corretora-a", "partner-slow", cacheTestRequest())
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if got.CacheAgeSeconds == nil || *got.CacheAgeSeconds != 0 {
		t.Fatalf("age must never be negative: %+v", got.CacheAgeSeconds)
	}
}