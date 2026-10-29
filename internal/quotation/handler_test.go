package quotation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

const validBody = `{
  "driver": {"document": "12345678901", "birth_year": 1988},
  "vehicle": {"plate": "abc1d23", "model": "Gol 1.0", "year": 2020, "value_cents": 8500000},
  "coverage": "comprehensive"
}`

func testAPI(quoter Quoter) http.Handler {
	return NewAPI(NewService(threePartners, quoter), []string{"corretora-a", "corretora-b"}).Routes()
}

func postQuotes(h http.Handler, tenant, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/quotes", strings.NewReader(body))
	if tenant != "" {
		req.Header.Set(TenantHeader, tenant)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	return response
}

func TestQuotesReturnsAggregatedQuotes(t *testing.T) {
	response := postQuotes(testAPI(&fakeQuoter{premiums: defaultPremiums()}), "corretora-a", validBody)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, expected 200: %s", response.Code, response.Body)
	}

	var body Response
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable response: %v", err)
	}
	if len(body.Quotes) != 3 {
		t.Fatalf("%d quotes, expected 3", len(body.Quotes))
	}
	if body.TenantID != "corretora-a" {
		t.Errorf("tenant_id %q, expected corretora-a", body.TenantID)
	}
}

func TestQuotesRejectsRequestWithoutTenant(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums()}
	response := postQuotes(testAPI(quoter), "", validBody)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status %d, expected 400", response.Code)
	}
	if len(quoter.calls) != 0 {
		t.Fatalf("the partners were called (%v) even without a tenant", quoter.calls)
	}
}

func TestQuotesRejectsUnknownBroker(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums()}
	response := postQuotes(testAPI(quoter), "corretora-pirata", validBody)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status %d, expected 403", response.Code)
	}
	if len(quoter.calls) != 0 {
		t.Fatalf("the partners were called (%v) for a broker that is not enabled", quoter.calls)
	}
}

func TestQuotesRejectsInvalidBody(t *testing.T) {
	cases := map[string]string{
		"broken json":      `{"driver":`,
		"missing document": `{"driver":{"birth_year":1988},"vehicle":{"plate":"ABC1D23","year":2020,"value_cents":100}}`,
		"missing plate":    `{"driver":{"document":"1","birth_year":1988},"vehicle":{"year":2020,"value_cents":100}}`,
		"zero value":       `{"driver":{"document":"1","birth_year":1988},"vehicle":{"plate":"A","year":2020,"value_cents":0}}`,
		"invalid coverage": `{"driver":{"document":"1","birth_year":1988},"vehicle":{"plate":"A","year":2020,"value_cents":1},"coverage":"vip"}`,
		"unknown field":    `{"driver":{"document":"1","birth_year":1988},"vehicle":{"plate":"A","year":2020,"value_cents":1},"discount":true}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			response := postQuotes(testAPI(&fakeQuoter{premiums: defaultPremiums()}), "corretora-a", body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status %d, expected 400: %s", response.Code, response.Body)
			}
		})
	}
}

func TestQuotesResponds502WithTheNameOfThePartnerThatWentDown(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums(), failOn: "partner-flaky"}
	response := postQuotes(testAPI(quoter), "corretora-a", validBody)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status %d, expected 502", response.Code)
	}

	var body platform.ErrorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable error: %v", err)
	}
	if body.Partner != "partner-flaky" {
		t.Errorf("partner blamed %q, expected partner-flaky", body.Partner)
	}
}

func TestHealthz(t *testing.T) {
	response := httptest.NewRecorder()
	testAPI(&fakeQuoter{premiums: defaultPremiums()}).
		ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status %d, expected 200", response.Code)
	}
}

func TestQuotesReturnsDegradedResponseWithPartialFallback(t *testing.T) {
	quoter := &fakeQuoter{
		premiums: defaultPremiums(),
		failOn:   "partner-flaky",
	}

	handler := NewAPI(
		NewService(threePartners, quoter, WithPartialFallback()),
		[]string{"corretora-a", "corretora-b"},
	).Routes()

	response := postQuotes(handler, "corretora-a", validBody)

	if response.Code != http.StatusOK {
		t.Fatalf("status %d, expected 200: %s", response.Code, response.Body)
	}

	var body Response
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable response: %v", err)
	}

	if len(body.Quotes) != 2 {
		t.Fatalf("got %d quotes, expected 2", len(body.Quotes))
	}

	if !body.Degraded {
		t.Error("response is not marked as degraded")
	}

	if len(body.Missing) != 1 || body.Missing[0] != "partner-flaky" {
		t.Errorf("missing partners %v, want [partner-flaky]", body.Missing)
	}

	if body.TenantID != "corretora-a" {
		t.Errorf("tenant_id %q, expected corretora-a", body.TenantID)
	}
}


func TestQuotesReturns503WhenAllPartnersFailWithPartialFallback(t *testing.T) {
	quoter := failingQuoter{}

	handler := NewAPI(
		NewService(threePartners, quoter, WithPartialFallback()),
		[]string{"corretora-a", "corretora-b"},
	).Routes()

	response := postQuotes(handler, "corretora-a", validBody)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, expected 503: %s", response.Code, response.Body)
	}

	var body platform.ErrorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable error: %v", err)
	}

	if body.Message != "no quotes available" {
		t.Errorf("message %q, want %q", body.Message, "no quotes available")
	}
}

type failingQuoter struct{}

func (failingQuoter) Quote(
	ctx context.Context,
	p platform.Partner,
	request any,
) (partner.Quote, error) {
	return partner.Quote{}, &partner.Error{Partner: p.Name}
}

func TestQuotesPreserves502WithoutPartialFallback(t *testing.T) {
	quoter := &fakeQuoter{
		premiums: defaultPremiums(),
		failOn:   "partner-flaky",
	}

	response := postQuotes(testAPI(quoter), "corretora-a", validBody)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status %d, expected 502: %s", response.Code, response.Body)
	}

	var body platform.ErrorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable error: %v", err)
	}

	if body.Partner != "partner-flaky" {
		t.Errorf("partner blamed %q, expected partner-flaky", body.Partner)
	}
}
