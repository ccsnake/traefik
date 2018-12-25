package apikey

import (
	"github.com/containous/traefik/log"
	"github.com/prometheus/client_golang/prometheus"
	"net/http"
)

type Usage struct {
	path    string `json:"path"`
	counter *prometheus.CounterVec
}

func NewUsage(path string) (*Usage, error) {
	u := &Usage{path: path}
	u.counter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   "",
		Subsystem:   "",
		Name:        "api_usage",
		Help:        "usage of api key",
		ConstLabels: nil,
	}, []string{"host", "path", "api_key"})

	if err := prometheus.Register(u.counter); err != nil {
		return nil, err
	}

	return u, nil
}

func (u *Usage) ServeHTTP(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if val := r.Header.Get(u.path); len(val) > 0 {
		u.counter.WithLabelValues(r.Host, r.URL.Path, val).Inc()
	}

	if next != nil {
		next(rw, r)
	}
}
