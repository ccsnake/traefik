package apikey

import (
	"github.com/prometheus/client_golang/prometheus"
	"net/http"
	"strings"
	"sync"
)

type position int

const (
	Param = position(iota)
	Header
	Body
)

type Usage struct {
	extractor []KeyExtractor
}

var once sync.Once
var counter *prometheus.CounterVec

func NewUsage(path string) (*Usage, error) {
	u := &Usage{}
	ss := strings.Split(path, ";")

	for _, p := range ss {
		extractor, err := NewKeyExtractor(p)
		if err != nil {
			return nil, err
		}
		u.extractor = append(u.extractor, extractor)
	}

	var err error
	once.Do(func() {
		counter = prometheus.NewCounterVec(prometheus.CounterOpts{
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
	for _, extractor := range u.extractor {
		if val := extractor.Extract(r); len(val) > 0 {
			counter.WithLabelValues(r.Host, r.URL.Path, val).Inc()
			break
		}
	}

	if next != nil {
		next(rw, r)
	}
}
