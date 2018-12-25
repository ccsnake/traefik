package apikey

import (
	"github.com/prometheus/client_golang/prometheus"
	"net/http"
	"sync"
)

type Usage struct {
	path    string `json:"path"`
}

var once sync.Once
var counter *prometheus.CounterVec

func NewUsage(path string) (*Usage, error) {
	u := &Usage{path: path}
	var err error
	once.Do(func() {
		counter = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "",
			Subsystem:   "",
			Name:        "api_usage",
			Help:        "usage of api key",
			ConstLabels: nil,
		}, []string{"host", "path", "api_key"})

		err = prometheus.Register(counter)
	})

	if err != nil {
		return nil, err
	}

	return u, nil
}

func (u *Usage) ServeHTTP(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if val := r.Header.Get(u.path); len(val) > 0 {
		counter.WithLabelValues(r.Host, r.URL.Path, val).Inc()
	}

	if next != nil {
		next(rw, r)
	}
}
