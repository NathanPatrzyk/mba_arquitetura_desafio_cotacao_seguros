package quotation

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

var threePartners = []platform.Partner{
	{Name: "partner-slow", BaseURL: "http://slow"},
	{Name: "partner-flaky", BaseURL: "http://flaky"},
	{Name: "partner-degrading", BaseURL: "http://degrading"},
}

func validRequest() Request {
	r := Request{
		Driver:  Driver{Document: "12345678901", BirthYear: 1988},
		Vehicle: Vehicle{Plate: "ABC1D23", Model: "Gol 1.0", Year: 2020, ValueCents: 8500000},
	}
	if err := r.Normalize(); err != nil {
		panic(err)
	}
	return r
}

type fakeQuoter struct {
	delay      time.Duration
	premiums   map[string]int64
	failOn     string
	calls      []string
	requests   []any
	concurrent atomic.Int32
	peak       atomic.Int32
}

func (f *fakeQuoter) Quote(_ context.Context, p platform.Partner, request any) (partner.Quote, error) {
	if now := f.concurrent.Add(1); now > f.peak.Load() {
		f.peak.Store(now)
	}
	defer f.concurrent.Add(-1)

	f.calls = append(f.calls, p.Name)
	f.requests = append(f.requests, request)
	time.Sleep(f.delay)

	if p.Name == f.failOn {
		return partner.Quote{}, &partner.Error{Partner: p.Name, Status: 503, Reason: "partner answered 503"}
	}
	return partner.Quote{Partner: p.Name, PremiumCents: f.premiums[p.Name], Currency: "BRL"}, nil
}

func defaultPremiums() map[string]int64 {
	return map[string]int64{"partner-slow": 180000, "partner-flaky": 90000, "partner-degrading": 120000}
}

func TestQuoteAggregatesTheThreePartnersSortedByPremium(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums()}
	response, err := NewService(threePartners, quoter).Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if len(response.Quotes) != 3 {
		t.Fatalf("%d quotes, expected 3", len(response.Quotes))
	}
	expected := []string{"partner-flaky", "partner-degrading", "partner-slow"}
	for i, name := range expected {
		if response.Quotes[i].Partner != name {
			t.Errorf("quote %d is from %q, expected from %q", i, response.Quotes[i].Partner, name)
		}
	}
	if response.TenantID != "corretora-a" {
		t.Errorf("tenant_id %q, expected corretora-a", response.TenantID)
	}
}

func TestQuoteCallsThePartnersSerially(t *testing.T) {
	quoter := &fakeQuoter{delay: 40 * time.Millisecond, premiums: defaultPremiums()}

	start := time.Now()
	if _, err := NewService(threePartners, quoter).Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatalf("Quote: %v", err)
	}
	elapsed := time.Since(start)

	if minimum := 3 * quoter.delay; elapsed < minimum {
		t.Fatalf("aggregation took %s; serially it should take at least %s", elapsed, minimum)
	}
	if peak := quoter.peak.Load(); peak != 1 {
		t.Fatalf("%d concurrent calls at the peak, expected 1 (serial calls)", peak)
	}
}

func TestOnePartnerDownBringsDownTheWholeRequest(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums(), failOn: "partner-flaky"}

	_, err := NewService(threePartners, quoter).Quote(context.Background(), "corretora-a", validRequest())
	if err == nil {
		t.Fatal("one partner down and the request answered success — there is a fallback where there should be none")
	}

	var failure *partner.Error
	if !errors.As(err, &failure) || failure.Partner != "partner-flaky" {
		t.Fatalf("error %v does not identify the partner that failed", err)
	}

	if last := quoter.calls[len(quoter.calls)-1]; last != "partner-flaky" {
		t.Errorf("last partner called was %q, expected partner-flaky", last)
	}
}

func TestBrokerGoesInThePartnerRequest(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums()}
	service := NewService(threePartners[:1], quoter)

	if _, err := service.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if _, err := service.Quote(context.Background(), "corretora-b", validRequest()); err != nil {
		t.Fatalf("Quote: %v", err)
	}

	toA, toB := serialize(t, quoter.requests[0]), serialize(t, quoter.requests[1])
	if toA == toB {
		t.Fatalf("different brokers produced the same request to the partner: %s", toA)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(toA), &body); err != nil {
		t.Fatalf("request to the partner is not JSON: %v", err)
	}
	if body["broker"] != "corretora-a" {
		t.Errorf("broker %v, expected corretora-a", body["broker"])
	}
	if _, ok := body["vehicle"]; !ok {
		t.Errorf("request to the partner does not carry the vehicle: %s", toA)
	}
}

func serialize(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

type scriptedQuoter struct {
	failing map[string]error
}

func (q *scriptedQuoter) Quote(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
	if err := q.failing[p.Name]; err != nil {
		return partner.Quote{}, err
	}
	return partner.Quote{
		Partner:      p.Name,
		QuoteID:      p.Name + "-id",
		PremiumCents: defaultPremiums()[p.Name],
		Currency:     "BRL",
	}, nil
}

func unavailableError(name string) error {
	return &partner.Error{Partner: name, Status: 503, Reason: "partner answered 503"}
}

func allPartnersUnavailable() map[string]error {
	failing := make(map[string]error, len(threePartners))
	for _, p := range threePartners {
		failing[p.Name] = unavailableError(p.Name)
	}
	return failing
}

func TestPartialFallbackDropsTheFailingPartnerAndMarksTheResponseDegraded(t *testing.T) {
	quoter := &scriptedQuoter{failing: map[string]error{"partner-flaky": unavailableError("partner-flaky")}}
	service := NewService(threePartners, quoter, WithPartialFallback())

	response, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("a missing partner must not fail the quote: %v", err)
	}

	if !response.Degraded {
		t.Fatal("a response without one partner must be degraded")
	}
	if len(response.Missing) != 1 || response.Missing[0] != "partner-flaky" {
		t.Fatalf("missing = %v, expected [partner-flaky]", response.Missing)
	}
	if len(response.Quotes) != 2 {
		t.Fatalf("%d quotes, expected 2", len(response.Quotes))
	}
	if response.Quotes[0].Partner != "partner-degrading" || response.Quotes[1].Partner != "partner-slow" {
		t.Fatalf("quotes are not sorted by premium: %+v", response.Quotes)
	}
}

func TestPartialFallbackIsNotDegradedWhenEveryPartnerAnswers(t *testing.T) {
	quoter := &scriptedQuoter{failing: map[string]error{}}
	service := NewService(threePartners, quoter, WithPartialFallback())

	response, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if response.Degraded {
		t.Fatal("a complete response must not be degraded")
	}
	if response.Missing == nil || len(response.Missing) != 0 {
		t.Fatalf("missing must be an empty list, got %#v", response.Missing)
	}
}

func TestPartialFallbackReturnsNoQuotesWhenEveryPartnerFails(t *testing.T) {
	quoter := &scriptedQuoter{failing: allPartnersUnavailable()}
	service := NewService(threePartners, quoter, WithPartialFallback())

	_, err := service.Quote(context.Background(), "corretora-a", validRequest())

	if !errors.Is(err, ErrNoQuotes) {
		t.Fatalf("err = %v, expected ErrNoQuotes", err)
	}
}

func TestPartialFallbackKeepsServingFromCacheWhenPartnersGoDown(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	quoter := &scriptedQuoter{failing: map[string]error{}}
	service := NewService(threePartners, quoter, WithCache(newTestCache(t, newMemStore(), clock)), WithPartialFallback())
	ctx := context.Background()

	if _, err := service.Quote(ctx, "corretora-a", validRequest()); err != nil {
		t.Fatalf("first quote: %v", err)
	}

	quoter.failing = allPartnersUnavailable()
	clock.Advance(120 * time.Second)

	response, err := service.Quote(ctx, "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("cached quote: %v", err)
	}
	if response.Degraded || len(response.Quotes) != 3 {
		t.Fatalf("a fully cached answer is a normal answer: degraded=%v quotes=%d", response.Degraded, len(response.Quotes))
	}
	for _, q := range response.Quotes {
		if q.Source != partner.SourceCache {
			t.Fatalf("quote must come from the cache: %+v", q)
		}
	}
}

func TestPartialFallbackCompletesAPartialAnswerWithCachedPartners(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	quoter := &scriptedQuoter{failing: map[string]error{"partner-flaky": unavailableError("partner-flaky")}}
	service := NewService(threePartners, quoter, WithCache(newTestCache(t, newMemStore(), clock)), WithPartialFallback())
	ctx := context.Background()

	if _, err := service.Quote(ctx, "corretora-a", validRequest()); err != nil {
		t.Fatalf("first quote: %v", err)
	}

	quoter.failing = allPartnersUnavailable()
	clock.Advance(120 * time.Second)

	response, err := service.Quote(ctx, "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("second quote: %v", err)
	}
	if !response.Degraded || len(response.Missing) != 1 || response.Missing[0] != "partner-flaky" {
		t.Fatalf("only partner-flaky has neither answer nor cache: degraded=%v missing=%v", response.Degraded, response.Missing)
	}
	if len(response.Quotes) != 2 {
		t.Fatalf("%d quotes, expected 2", len(response.Quotes))
	}
	for _, q := range response.Quotes {
		if q.Source != partner.SourceCache {
			t.Fatalf("quote must come from the cache: %+v", q)
		}
	}
}

func TestPartialFallbackStopsWhenTheRequestIsCanceled(t *testing.T) {
	quoter := &scriptedQuoter{failing: map[string]error{
		"partner-slow":      context.Canceled,
		"partner-flaky":     context.Canceled,
		"partner-degrading": context.Canceled,
	}}
	service := NewService(threePartners, quoter, WithPartialFallback())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := service.Quote(ctx, "corretora-a", validRequest())

	if err == nil || errors.Is(err, ErrNoQuotes) {
		t.Fatalf("a canceled request must fail without being reported as no quotes, got %v", err)
	}
}

func parallelQuote(name string) partner.Quote {
	return partner.Quote{
		Partner:      name,
		QuoteID:      name + "-id",
		PremiumCents: defaultPremiums()[name],
		Currency:     "BRL",
	}
}

type barrierQuoter struct {
	parties int
	mu      sync.Mutex
	arrived int
	release chan struct{}
}

func newBarrierQuoter(parties int) *barrierQuoter {
	return &barrierQuoter{parties: parties, release: make(chan struct{})}
}

func (q *barrierQuoter) Quote(ctx context.Context, p platform.Partner, _ any) (partner.Quote, error) {
	q.mu.Lock()
	q.arrived++
	if q.arrived == q.parties {
		close(q.release)
	}
	q.mu.Unlock()

	select {
	case <-q.release:
		return parallelQuote(p.Name), nil
	case <-ctx.Done():
		return partner.Quote{}, ctx.Err()
	case <-time.After(time.Second):
		return partner.Quote{}, &partner.Error{Partner: p.Name, Reason: "partners were not called together"}
	}
}

type isolationQuoter struct {
	failing string
	failed  chan struct{}
}

func newIsolationQuoter(failing string) *isolationQuoter {
	return &isolationQuoter{failing: failing, failed: make(chan struct{})}
}

func (q *isolationQuoter) Quote(ctx context.Context, p platform.Partner, _ any) (partner.Quote, error) {
	if p.Name == q.failing {
		close(q.failed)
		return partner.Quote{}, &partner.Error{Partner: p.Name, Reason: "timeout after 2s", Timeout: true, Cause: context.DeadlineExceeded}
	}

	select {
	case <-q.failed:
	case <-time.After(time.Second):
		return partner.Quote{}, &partner.Error{Partner: p.Name, Reason: "the failing partner was not called together"}
	}

	select {
	case <-ctx.Done():
		return partner.Quote{}, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return parallelQuote(p.Name), nil
	}
}

type panickingQuoter struct {
	panicOn string
}

func (q *panickingQuoter) Quote(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
	if p.Name == q.panicOn {
		panic("boom")
	}
	return parallelQuote(p.Name), nil
}

func TestParallelQuotesCallsAllPartnersTogether(t *testing.T) {
	service := NewService(threePartners, newBarrierQuoter(3), WithParallelQuotes())

	response, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("the three partners must be in flight together: %v", err)
	}
	if len(response.Quotes) != 3 {
		t.Fatalf("%d quotes, expected 3", len(response.Quotes))
	}
}

func TestParallelQuotesKeepsTheQuotesSortedByPremium(t *testing.T) {
	service := NewService(threePartners, newBarrierQuoter(3), WithParallelQuotes())

	response, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	expected := []string{"partner-flaky", "partner-degrading", "partner-slow"}
	for i, name := range expected {
		if response.Quotes[i].Partner != name {
			t.Errorf("quote %d is from %q, expected from %q", i, response.Quotes[i].Partner, name)
		}
	}
	if response.Degraded || len(response.Missing) != 0 {
		t.Errorf("a complete response must not be degraded: degraded=%v missing=%v", response.Degraded, response.Missing)
	}
}

func TestParallelQuotesTimeoutOfOnePartnerDoesNotCancelTheOthers(t *testing.T) {
	service := NewService(threePartners, newIsolationQuoter("partner-flaky"), WithParallelQuotes(), WithPartialFallback())

	response, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("the other partners must still answer: %v", err)
	}
	if !response.Degraded || len(response.Missing) != 1 || response.Missing[0] != "partner-flaky" {
		t.Fatalf("degraded=%v missing=%v, expected only partner-flaky missing", response.Degraded, response.Missing)
	}
	if len(response.Quotes) != 2 {
		t.Fatalf("%d quotes, expected 2", len(response.Quotes))
	}
}

func TestParallelQuotesTwoPartnersFailingLeavesTheThird(t *testing.T) {
	quoter := &scriptedQuoter{failing: map[string]error{
		"partner-slow":  unavailableError("partner-slow"),
		"partner-flaky": unavailableError("partner-flaky"),
	}}
	service := NewService(threePartners, quoter, WithParallelQuotes(), WithPartialFallback())

	response, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("one partner answering is enough: %v", err)
	}
	if len(response.Quotes) != 1 || response.Quotes[0].Partner != "partner-degrading" {
		t.Fatalf("quotes = %+v, expected only partner-degrading", response.Quotes)
	}
	if len(response.Missing) != 2 || response.Missing[0] != "partner-slow" || response.Missing[1] != "partner-flaky" {
		t.Fatalf("missing = %v, expected [partner-slow partner-flaky] in partner order", response.Missing)
	}
}

func TestParallelQuotesReturnsNoQuotesWhenEveryPartnerFails(t *testing.T) {
	quoter := &scriptedQuoter{failing: allPartnersUnavailable()}
	service := NewService(threePartners, quoter, WithParallelQuotes(), WithPartialFallback())

	_, err := service.Quote(context.Background(), "corretora-a", validRequest())

	if !errors.Is(err, ErrNoQuotes) {
		t.Fatalf("err = %v, expected ErrNoQuotes", err)
	}
}

func TestParallelQuotesWithoutFallbackReturnsTheFirstErrorInPartnerOrder(t *testing.T) {
	quoter := &scriptedQuoter{failing: map[string]error{
		"partner-slow":      unavailableError("partner-slow"),
		"partner-degrading": unavailableError("partner-degrading"),
	}}
	service := NewService(threePartners, quoter, WithParallelQuotes())

	_, err := service.Quote(context.Background(), "corretora-a", validRequest())

	var failure *partner.Error
	if !errors.As(err, &failure) || failure.Partner != "partner-slow" {
		t.Fatalf("err = %v, expected the error of partner-slow, the first in partner order", err)
	}
}

func TestParallelQuotesAnOpenBreakerDoesNotBlockTheOtherPartners(t *testing.T) {
	inner := &scriptedQuoter{failing: map[string]error{"partner-flaky": unavailableError("partner-flaky")}}
	protected := partner.NewProtectedClient(inner, threePartners)
	service := NewService(threePartners, protected, WithParallelQuotes(), WithPartialFallback())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := service.Quote(ctx, "corretora-a", validRequest()); err != nil {
			t.Fatalf("quote %d: %v", i, err)
		}
	}
	if state, _ := protected.State("partner-flaky"); state != gobreaker.StateOpen {
		t.Fatalf("breaker state = %s, expected open", state)
	}

	response, err := service.Quote(ctx, "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("quote with the breaker open: %v", err)
	}
	if !response.Degraded || len(response.Missing) != 1 || response.Missing[0] != "partner-flaky" {
		t.Fatalf("degraded=%v missing=%v, expected only partner-flaky missing", response.Degraded, response.Missing)
	}
	if len(response.Quotes) != 2 {
		t.Fatalf("%d quotes, expected 2", len(response.Quotes))
	}
}

func TestParallelQuotesCacheHitStillAvoidsThePartnerCall(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	quoter := newCountingQuoter()
	service := NewService(threePartners, quoter, WithCache(newTestCache(t, newMemStore(), clock)), WithParallelQuotes())
	ctx := context.Background()

	if _, err := service.Quote(ctx, "corretora-a", validRequest()); err != nil {
		t.Fatalf("first quote: %v", err)
	}
	if quoter.total() != 3 {
		t.Fatalf("first quote made %d partner calls, expected 3", quoter.total())
	}

	clock.Advance(90 * time.Second)
	response, err := service.Quote(ctx, "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("second quote: %v", err)
	}
	if quoter.total() != 3 {
		t.Fatalf("second quote called the partners again: %d calls", quoter.total())
	}
	for _, q := range response.Quotes {
		if q.Source != partner.SourceCache {
			t.Fatalf("quote must come from the cache: %+v", q)
		}
	}
}

func TestParallelQuotesAPanicInOnePartnerBecomesAMissingPartner(t *testing.T) {
	service := NewService(threePartners, &panickingQuoter{panicOn: "partner-degrading"}, WithParallelQuotes(), WithPartialFallback())

	response, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("a panic in one partner must not fail the quote: %v", err)
	}
	if !response.Degraded || len(response.Missing) != 1 || response.Missing[0] != "partner-degrading" {
		t.Fatalf("degraded=%v missing=%v, expected only partner-degrading missing", response.Degraded, response.Missing)
	}
	if len(response.Quotes) != 2 {
		t.Fatalf("%d quotes, expected 2", len(response.Quotes))
	}
}

func TestParallelQuotesAPanicWithoutFallbackIsAPartnerError(t *testing.T) {
	service := NewService(threePartners, &panickingQuoter{panicOn: "partner-degrading"}, WithParallelQuotes())

	_, err := service.Quote(context.Background(), "corretora-a", validRequest())

	var failure *partner.Error
	if !errors.As(err, &failure) || failure.Partner != "partner-degrading" {
		t.Fatalf("err = %v, expected a partner error naming partner-degrading", err)
	}
}

func TestParallelQuotesStopsWhenTheRequestIsCanceled(t *testing.T) {
	quoter := &scriptedQuoter{failing: map[string]error{
		"partner-slow":      context.Canceled,
		"partner-flaky":     context.Canceled,
		"partner-degrading": context.Canceled,
	}}
	service := NewService(threePartners, quoter, WithParallelQuotes(), WithPartialFallback())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := service.Quote(ctx, "corretora-a", validRequest())

	if err == nil || errors.Is(err, ErrNoQuotes) {
		t.Fatalf("a canceled request must fail without being reported as no quotes, got %v", err)
	}
}
