package apikey

import (
	"bytes"
	"fmt"
	"github.com/tidwall/gjson"
	"io/ioutil"
	"net/http"
	"strings"
)

type KeyExtractor interface {
	Extract(r *http.Request) string
}

type KeyExtractorFunc func(r *http.Request) string

func (k KeyExtractorFunc) Extract(r *http.Request) string {
	return k(r)
}

func buildHeaderExtractor(path string) KeyExtractorFunc {
	return func(r *http.Request) string {
		return r.Header.Get(path)
	}
}

func buildParamExtractor(path string) KeyExtractorFunc {
	return func(r *http.Request) string {
		qs := r.URL.Query()
		if len(qs) == 0 {
			return ""
		}
		return qs.Get(path)
	}
}

func buildBodyExtractor(path string) KeyExtractorFunc {
	return func(r *http.Request) string {
		buf, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return ""
		}
		r.Body = ioutil.NopCloser(bytes.NewBuffer(buf))

		res := gjson.GetBytes(buf, path)
		return res.String()
	}
}

func NewKeyExtractor(path string) (KeyExtractor, error) {
	ss := strings.SplitN(strings.TrimSpace(path), ".", 2)
	if len(ss) < 2 {
		return nil, fmt.Errorf("create key extractor failed for path %s", path)
	}
	pos, rpath := ss[0], ss[1]
	switch strings.ToLower(pos) {
	case "param":
		return buildParamExtractor(rpath), nil
	case "header":
		return buildHeaderExtractor(rpath), nil
	case "body":
		return buildBodyExtractor(rpath), nil
	default:
		return nil, fmt.Errorf("create key extractor failed for unspupport position %s", ss[0])
	}
}
