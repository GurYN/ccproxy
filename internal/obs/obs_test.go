package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestInstrument_RecordsCount(t *testing.T) {
	m := NewMetrics()
	h := m.Instrument("/v1/test", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-CC-Token-Name", "n8n")
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	scrape := scrapeRegistry(t, m)
	if !strings.Contains(scrape, `ccproxy_requests_total{endpoint="/v1/test",method="POST",status="201",token_name="n8n"} 1`) {
		t.Errorf("expected request counter line in scrape, got:\n%s", scrape)
	}
}

func TestInstrument_AnonymousWhenTokenAbsent(t *testing.T) {
	m := NewMetrics()
	h := m.Instrument("/healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	scrape := scrapeRegistry(t, m)
	if !strings.Contains(scrape, `token_name="anonymous"`) {
		t.Errorf("expected anonymous token label, got:\n%s", scrape)
	}
}

func scrapeRegistry(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
