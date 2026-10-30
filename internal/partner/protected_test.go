package partner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type stubCaller struct {
	calls   map[string]int
	outcome func(partner string, call int) error
}

func newStubCaller(outcome func(partner string, call int) error) *stubCaller {
	return &stubCaller{calls: map[string]int{}, outcome: outcome}
}

func (s *stubCaller) Quote(_ context.Context, p platform.Partner, _ any) (Quote, error) {
	s.calls[p.Name]++
	return Quote{Partner: p.Name}, s.outcome(p.Name, s.calls[p.Name])
}

func alwaysFail(err error) func(string, int) error {
	return func(string, int) error { return err }
}

func failingCalls(calls ...int) func(string, int) error {
	failing := map[int]bool{}
	for _, c := range calls {
		failing[c] = true
	}
	return func(_ string, call int) error {
		if failing[call] {
			return &Error{Partner: "partner-flaky", Status: 503}
		}
		return nil
	}
}

func callN(client *ProtectedClient, p platform.Partner, n int) {
	for i := 0; i < n; i++ {
		_, _ = client.Quote(context.Background(), p, nil)
	}
}

func expectState(t *testing.T, client *ProtectedClient, partner string, want gobreaker.State) {
	t.Helper()
	if got, _ := client.State(partner); got != want {
		t.Fatalf("%s state = %s, want %s", partner, got, want)
	}
}

func TestShouldTripNeedsFiveFailuresAndHalfOfTheCompletedCalls(t *testing.T) {
	cases := []struct {
		name      string
		successes uint32
		failures  uint32
		want      bool
	}{
		{"below the failure floor", 0, 4, false},
		{"five failures only", 0, 5, true},
		{"five failures, four successes", 4, 5, true},
		{"exactly half failing", 5, 5, true},
		{"just under half failing", 6, 5, false},
		{"large window at half", 20, 20, true},
		{"burst diluted by volume", 111, 9, false},
	}

	for _, c := range cases {
		counts := gobreaker.Counts{TotalSuccesses: c.successes, TotalFailures: c.failures}
		if got := shouldTrip(counts); got != c.want {
			t.Errorf("%s: shouldTrip(%d successes, %d failures) = %v, want %v", c.name, c.successes, c.failures, got, c.want)
		}
	}
}

func TestBreakerSettingsMatchTheDocumentedValues(t *testing.T) {
	s := breakerSettings("partner-flaky", nil)

	if s.MaxRequests != 3 {
		t.Errorf("MaxRequests = %d, want 3", s.MaxRequests)
	}
	if s.Interval != 30*time.Second || s.BucketPeriod != 3*time.Second {
		t.Errorf("window = %s in %s buckets, want 30s in 3s buckets", s.Interval, s.BucketPeriod)
	}
	if s.Timeout != 30*time.Second {
		t.Errorf("Timeout = %s, want 30s", s.Timeout)
	}
}

func TestProtectedClientOpensAfterFiveFailures(t *testing.T) {
	flaky := platform.Partner{Name: "partner-flaky"}
	stub := newStubCaller(alwaysFail(&Error{Partner: "partner-flaky", Status: 503}))
	client := NewProtectedClient(stub, []platform.Partner{flaky})

	callN(client, flaky, 4)
	expectState(t, client, "partner-flaky", gobreaker.StateClosed)

	callN(client, flaky, 1)
	expectState(t, client, "partner-flaky", gobreaker.StateOpen)
}

func TestProtectedClientShortCircuitsWhileOpen(t *testing.T) {
	flaky := platform.Partner{Name: "partner-flaky"}
	stub := newStubCaller(alwaysFail(&Error{Partner: "partner-flaky", Status: 503}))
	client := NewProtectedClient(stub, []platform.Partner{flaky})

	callN(client, flaky, 5)
	_, err := client.Quote(context.Background(), flaky, nil)

	var failure *Error
	if !errors.As(err, &failure) || !failure.CircuitOpen {
		t.Fatalf("expected a circuit-open *Error, got %v", err)
	}
	if stub.calls["partner-flaky"] != 5 {
		t.Fatalf("partner called %d times, want 5", stub.calls["partner-flaky"])
	}
}

func TestProtectedClientStaysClosedBelowHalfOfTheCalls(t *testing.T) {
	flaky := platform.Partner{Name: "partner-flaky"}
	stub := newStubCaller(failingCalls(7, 8, 9, 10, 11))
	client := NewProtectedClient(stub, []platform.Partner{flaky})

	callN(client, flaky, 11)

	expectState(t, client, "partner-flaky", gobreaker.StateClosed)
}

func TestProtectedClientIsolatesPartners(t *testing.T) {
	flaky := platform.Partner{Name: "partner-flaky"}
	slow := platform.Partner{Name: "partner-slow"}
	stub := newStubCaller(func(partner string, _ int) error {
		if partner == "partner-flaky" {
			return &Error{Partner: partner, Status: 503}
		}
		return nil
	})
	client := NewProtectedClient(stub, []platform.Partner{flaky, slow})

	callN(client, flaky, 5)

	if _, err := client.Quote(context.Background(), slow, nil); err != nil {
		t.Fatalf("healthy partner was affected: %v", err)
	}
	expectState(t, client, "partner-slow", gobreaker.StateClosed)
}

func TestProtectedClientDoesNotCountCanceledCalls(t *testing.T) {
	slow := platform.Partner{Name: "partner-slow"}
	canceled := &Error{Partner: "partner-slow", Reason: "canceled", Cause: context.Canceled}
	stub := newStubCaller(alwaysFail(canceled))
	client := NewProtectedClient(stub, []platform.Partner{slow})

	callN(client, slow, 30)

	if stub.calls["partner-slow"] != 30 {
		t.Fatalf("partner called %d times, want 30", stub.calls["partner-slow"])
	}
	expectState(t, client, "partner-slow", gobreaker.StateClosed)
}

func TestProtectedClientCountsTimeoutsAsFailures(t *testing.T) {
	degrading := platform.Partner{Name: "partner-degrading"}
	timeout := &Error{Partner: "partner-degrading", Reason: "timeout", Timeout: true, Cause: context.DeadlineExceeded}
	stub := newStubCaller(alwaysFail(timeout))
	client := NewProtectedClient(stub, []platform.Partner{degrading})

	callN(client, degrading, 5)

	expectState(t, client, "partner-degrading", gobreaker.StateOpen)
}

func TestProtectedClientPassesThroughAPartnerWithoutBreaker(t *testing.T) {
	known := platform.Partner{Name: "partner-slow"}
	unknown := platform.Partner{Name: "partner-new"}
	stub := newStubCaller(alwaysFail(&Error{Partner: "partner-new", Status: 503}))
	client := NewProtectedClient(stub, []platform.Partner{known})

	callN(client, unknown, 20)

	if stub.calls["partner-new"] != 20 {
		t.Fatalf("partner called %d times, want 20", stub.calls["partner-new"])
	}
	if _, ok := client.State("partner-new"); ok {
		t.Fatal("a partner without a breaker must not report a state")
	}
}