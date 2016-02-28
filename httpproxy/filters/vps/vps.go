package vps

import (
	// "fmt"
	"math/rand"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/golang/glog"
	"github.com/phuslu/http2"

	"../../../httpproxy"
	"../../filters"
)

const (
	filterName string = "vps"
)

type Filter struct {
	FetchServers []*FetchServer
	Sites        *httpproxy.HostMatcher
}

func init() {
	filename := filterName + ".json"
	config, err := NewConfig(filters.LookupConfigStoreURI(filterName), filename)
	if err != nil {
		glog.Fatalf("NewConfig(%#v) failed: %s", filename, err)
	}

	err = filters.Register(filterName, &filters.RegisteredFilter{
		New: func() (filters.Filter, error) {
			return NewFilter(config)
		},
	})

	if err != nil {
		glog.Fatalf("Register(%#v) error: %s", filterName, err)
	}
}

func NewFilter(config *Config) (filters.Filter, error) {
	fetchServers := make([]*FetchServer, 0)
	for _, fs := range config.FetchServers {
		u, err := url.Parse(fs.URL)
		if err != nil {
			return nil, err
		}

		transport := &http2.Transport{
			InsecureTLSDial: true,
			Proxy: func(req *http.Request) (*url.URL, error) {
				return u, nil
			},
		}

		fs := &FetchServer{
			URL:       u,
			Username:  fs.Username,
			Password:  fs.Password,
			SSLVerify: fs.SSLVerify,
			Transport: transport,
		}

		fetchServers = append(fetchServers, fs)
	}

	return &Filter{
		FetchServers: fetchServers,
		Sites:        httpproxy.NewHostMatcher(config.Sites),
	}, nil
}

func (p *Filter) FilterName() string {
	return filterName
}

func (f *Filter) RoundTrip(ctx *filters.Context, req *http.Request) (*filters.Context, *http.Response, error) {
	if !f.Sites.Match(req.Host) {
		return ctx, nil, nil
	}

	i := 0
	switch path.Ext(req.URL.Path) {
	case ".jpg", ".png", ".webp", ".bmp", ".gif", ".flv", ".mp4":
		i = rand.Intn(len(f.FetchServers))
	case "":
		name := path.Base(req.URL.Path)
		if strings.Contains(name, "play") ||
			strings.Contains(name, "video") {
			i = rand.Intn(len(f.FetchServers))
		}
	default:
		if strings.Contains(req.URL.Host, "img.") ||
			strings.Contains(req.URL.Host, "cache.") ||
			strings.Contains(req.URL.Host, "video.") ||
			strings.Contains(req.URL.Host, "static.") ||
			strings.HasPrefix(req.URL.Host, "img") ||
			strings.HasPrefix(req.URL.Path, "/static") ||
			strings.HasPrefix(req.URL.Path, "/asset") ||
			strings.Contains(req.URL.Path, "min.js") ||
			strings.Contains(req.URL.Path, "static") ||
			strings.Contains(req.URL.Path, "asset") ||
			strings.Contains(req.URL.Path, "/cache/") {
			i = rand.Intn(len(f.FetchServers))
		}
	}

	fetchServer := f.FetchServers[i]

	// if req.Method == "CONNECT" {
	// 	rconn, err := fetchServer.Transport.Connect(req)
	// 	if err != nil {
	// 		return ctx, nil, err
	// 	}
	// 	defer rconn.Close()

	// 	rw := ctx.GetResponseWriter()

	// 	hijacker, ok := rw.(http.Hijacker)
	// 	if !ok {
	// 		return ctx, nil, fmt.Errorf("http.ResponseWriter(%#v) does not implments http.Hijacker", rw)
	// 	}

	// 	flusher, ok := rw.(http.Flusher)
	// 	if !ok {
	// 		return ctx, nil, fmt.Errorf("http.ResponseWriter(%#v) does not implments http.Flusher", rw)
	// 	}

	// 	rw.WriteHeader(http.StatusOK)
	// 	flusher.Flush()

	// 	lconn, _, err := hijacker.Hijack()
	// 	if err != nil {
	// 		return ctx, nil, fmt.Errorf("%#v.Hijack() error: %v", hijacker, err)
	// 	}
	// 	defer lconn.Close()

	// 	go httpproxy.IoCopy(rconn, lconn)
	// 	httpproxy.IoCopy(lconn, rconn)

	// 	ctx.SetHijacked(true)
	// 	return ctx, nil, nil
	// }
	resp, err := fetchServer.RoundTrip(req)
	if err != nil {
		return ctx, nil, err
	} else {
		glog.Infof("%s \"VPS %s %s %s\" %d %s", req.RemoteAddr, req.Method, req.URL.String(), req.Proto, resp.StatusCode, resp.Header.Get("Content-Length"))
	}
	return ctx, resp, err
}
