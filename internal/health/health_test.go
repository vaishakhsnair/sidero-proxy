package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeRunner struct {
	outputs map[string]string
	errs    map[string]error
}

func (f fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	return []byte(f.outputs[key]), nil
}

type fakeRedis struct {
	err error
}

func (f fakeRedis) Ping(_ context.Context) error { return f.err }

func TestCheckerSuccess(t *testing.T) {
	t.Parallel()

	listener := &ListenerState{}
	listener.SetReady(true)
	checker := &Checker{
		Listener: listener,
		Redis:    fakeRedis{},
		Runner: fakeRunner{outputs: map[string]string{
			"nft list tables":           "table ip mcproxy_filter\ntable ip mcproxy_intercept\ntable ip mcproxy_nat\n",
			"ip route show table local": "local 10.1.0.0/16 dev lo proto kernel scope host src 10.1.0.1\n",
		}},
		Subnet: "10.1.0.0/16",
	}
	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestServerHealthzUnhealthy(t *testing.T) {
	t.Parallel()

	listener := &ListenerState{}
	checker := &Checker{
		Listener: listener,
		Redis:    fakeRedis{err: errors.New("down")},
		Runner:   fakeRunner{},
		Subnet:   "10.1.0.0/16",
	}
	srv := NewServer(checker)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
