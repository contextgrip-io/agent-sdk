package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// metrics is a minimal hand-rolled Prometheus text-format exporter — atomic
// counters only, no client library.
type metrics struct {
	mu       sync.Mutex
	requests map[string]*atomic.Int64

	questions   atomic.Int64
	modelErrors atomic.Int64
}

func newMetrics() *metrics {
	return &metrics{requests: map[string]*atomic.Int64{}}
}

func (m *metrics) incRequest(class string) {
	m.mu.Lock()
	counter, ok := m.requests[class]
	if !ok {
		counter = &atomic.Int64{}
		m.requests[class] = counter
	}
	m.mu.Unlock()
	counter.Add(1)
}

func (m *metrics) render() string {
	var b strings.Builder
	b.WriteString("# HELP ai_chat_requests_total HTTP requests served, by route class.\n")
	b.WriteString("# TYPE ai_chat_requests_total counter\n")
	m.mu.Lock()
	classes := make([]string, 0, len(m.requests))
	for class := range m.requests {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	for _, class := range classes {
		fmt.Fprintf(&b, "ai_chat_requests_total{class=%q} %d\n", class, m.requests[class].Load())
	}
	m.mu.Unlock()
	b.WriteString("# HELP ai_chat_questions_total Questions accepted by /api/v1/ask and /api/v1/messages.\n")
	b.WriteString("# TYPE ai_chat_questions_total counter\n")
	fmt.Fprintf(&b, "ai_chat_questions_total %d\n", m.questions.Load())
	b.WriteString("# HELP ai_chat_model_errors_total Model calls that failed (generation or explanation).\n")
	b.WriteString("# TYPE ai_chat_model_errors_total counter\n")
	fmt.Fprintf(&b, "ai_chat_model_errors_total %d\n", m.modelErrors.Load())
	return b.String()
}

func routeClass(path string) string {
	switch {
	case path == "/healthz" || path == "/readyz" || path == "/metrics":
		return "system"
	case path == "/api/v1/ask" || path == "/api/v1/messages":
		return "chat"
	case path == "/api/v1/status":
		return "status"
	case strings.HasPrefix(path, "/api/v1/training") ||
		(strings.HasPrefix(path, "/api/v1/messages/") && strings.HasSuffix(path, "/eval")):
		return "training"
	case strings.HasPrefix(path, "/api/v1/conversations"):
		return "conversations"
	case strings.HasPrefix(path, "/api/v1/tokens"):
		return "tokens"
	case strings.HasPrefix(path, "/api"):
		return "api"
	default:
		return "ui"
	}
}

func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.metrics.incRequest(routeClass(r.URL.Path))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(s.metrics.render()))
}
