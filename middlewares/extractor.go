package middlewares

import (
	"github.com/containous/traefik/log"
	"github.com/vulcand/oxy/utils"
	"net/http"
	"strings"
)

var extractClientPath utils.ExtractorFunc = func(req *http.Request) (token string, amount int64, err error) {
	path := strings.TrimSpace(req.URL.Path)
	return path, 1, nil
}

func NewExtractor(variable string) (utils.SourceExtractor, error) {
	if strings.ToLower(variable) == "request.path" {
		log.Debugf("Creating rate limiter by request path")
		return extractClientPath, nil
	}
	return utils.NewExtractor(variable)
}
