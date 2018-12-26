package apikey

import (
	"fmt"
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

func buildParamExtactor(path string) KeyExtractorFunc {
	return func(r *http.Request) string {
		qs := r.URL.Query()
		if len(qs) == 0 {
			return ""
		}
		return qs.Get(path)
	}
}

func buildBodyExtactor(path string) KeyExtractorFunc {
	return func(r *http.Request) string {
		// buf, err := ioutil.ReadAll(r.Body)
		// if err!=nil{
		// 	return ""
		// }
		// rdr1 := ioutil.NopCloser(bytes.NewBuffer(buf))
		// rdr2 := ioutil.NopCloser(bytes.NewBuffer(buf))
		// r.Body = rdr2
		// gjson.GetBytes(buf,"path")
		return ""
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
		return buildParamExtactor(rpath), nil
	case "header":
		return buildHeaderExtractor(rpath), nil
	case "body":
		return buildBodyExtactor(rpath), nil
	default:
		return nil, fmt.Errorf("create key extractor failed for unspupport position %s", ss[0])
	}
}
