package autoproxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	"../../../httpproxy"
	"../../../storage"
	"../../filters"
)

const (
	filterName      string = "autoproxy"
	placeholderPath string = "/proxy.pac"
)

type Config struct {
	Sites   []string
	GFWList struct {
		URL      string
		File     string
		Encoding string
		Duration int
	}
}

var (
	onceUpdater sync.Once
)

type GFWList struct {
	URL      *url.URL
	Filename string
	Encoding string
	Duration time.Duration
}

type Filter struct {
	Store         storage.Store
	Sites         *httpproxy.HostMatcher
	GFWList       *GFWList
	AutoProxy2Pac *AutoProxy2Pac
	Transport     *http.Transport
	UpdateChan    chan struct{}
}

func init() {
	filename := filterName + ".json"
	config := new(Config)
	err := storage.ReadJsonConfig(filters.LookupConfigStoreURI(filterName), filename, config)
	if err != nil {
		glog.Fatalf("storage.ReadJsonConfig(%#v) failed: %s", filename, err)
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

func NewFilter(config *Config) (_ filters.Filter, err error) {
	var gfwlist GFWList

	gfwlist.Encoding = config.GFWList.Encoding
	gfwlist.Filename = config.GFWList.File
	gfwlist.Duration = time.Duration(config.GFWList.Duration) * time.Second
	gfwlist.URL, err = url.Parse(config.GFWList.URL)
	if err != nil {
		return nil, err
	}

	store, err := storage.OpenURI(filters.LookupConfigStoreURI(filterName))
	if err != nil {
		return nil, err
	}

	if _, err := store.HeadObject(gfwlist.Filename); err != nil {
		return nil, err
	}

	autoproxy2pac := &AutoProxy2Pac{
		Sites: config.Sites,
	}

	object, err := store.GetObject(gfwlist.Filename, -1, -1)
	if err != nil {
		return nil, err
	}

	rc := object.Body()
	defer rc.Close()

	var r io.Reader
	br := bufio.NewReader(rc)
	if data, err := br.Peek(20); err == nil {
		if bytes.HasPrefix(data, []byte("[AutoProxy ")) {
			r = br
		} else {
			r = base64.NewDecoder(base64.StdEncoding, br)
		}
	}

	err = autoproxy2pac.Read(r)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}

	f := &Filter{
		Store:         store,
		Sites:         httpproxy.NewHostMatcher(config.Sites),
		GFWList:       &gfwlist,
		AutoProxy2Pac: autoproxy2pac,
		Transport:     transport,
		UpdateChan:    make(chan struct{}),
	}

	go onceUpdater.Do(f.updater)

	return f, nil
}

func (f *Filter) FilterName() string {
	return filterName
}

func (f *Filter) updater() {
	glog.V(2).Infof("start updater for %#v", f.GFWList)

	ticker := time.Tick(10 * time.Minute)

	for {
		needUpdate := false

		select {
		case <-f.UpdateChan:
			glog.Infof("Begin manual gfwlist(%#v) update...", f.GFWList.URL.String())
			needUpdate = true
		case <-ticker:
			break
		}

		if !needUpdate {
			h, err := f.Store.HeadObject(f.GFWList.Filename)
			if err != nil {
				glog.Warningf("stat gfwlist(%#v) err: %v", f.GFWList.Filename, err)
				continue
			}

			lm := h.Get("Last-Modified")
			if lm == "" {
				glog.Warningf("gfwlist(%#v) header(%#v) does not contains last-modified", f.GFWList.Filename, h)
				continue
			}

			modTime, err := time.Parse(f.Store.DateFormat(), lm)
			if err != nil {
				glog.Warningf("stat gfwlist(%#v) has parse %#v error: %v", f.GFWList.Filename, lm, err)
				continue
			}

			needUpdate = time.Now().After(modTime.Add(f.GFWList.Duration))
		}

		if needUpdate {
			req, err := http.NewRequest("GET", f.GFWList.URL.String(), nil)
			if err != nil {
				glog.Warningf("NewRequest(%#v) error: %v", f.GFWList.URL.String(), err)
				continue
			}

			glog.Infof("Downloading %#v", f.GFWList.URL.String())

			resp, err := f.Transport.RoundTrip(req)
			if err != nil {
				glog.Warningf("%T.RoundTrip(%#v) error: %v", f.Transport, f.GFWList.URL.String(), err)
				continue
			}

			var r io.Reader = resp.Body
			switch f.GFWList.Encoding {
			case "base64":
				r = base64.NewDecoder(base64.StdEncoding, r)
			default:
				break
			}

			data, err := ioutil.ReadAll(r)
			if err != nil {
				glog.Warningf("ReadAll(%#v) error: %v", r, err)
				resp.Body.Close()
				continue
			}

			err = f.Store.DeleteObject(f.GFWList.Filename)
			if err != nil {
				glog.Warningf("%T.DeleteObject(%#v) error: %v", f.Store, f.GFWList.Filename, err)
				continue
			}

			err = f.Store.PutObject(f.GFWList.Filename, http.Header{}, ioutil.NopCloser(bytes.NewReader(data)))
			if err != nil {
				glog.Warningf("%T.PutObject(%#v) error: %v", f.Store, f.GFWList.Filename, err)
				continue
			}

			glog.Infof("Update %#v from %#v OK", f.GFWList.Filename, f.GFWList.URL.String())
			resp.Body.Close()
		}
	}
}

func (f *Filter) RoundTrip(ctx *filters.Context, req *http.Request) (*filters.Context, *http.Response, error) {

	if !strings.HasPrefix(req.RequestURI, placeholderPath) {
		return ctx, nil, nil
	}

	if strings.Contains(req.URL.Query().Encode(), "flush") {
		f.UpdateChan <- struct{}{}
	}

	data := f.AutoProxy2Pac.GeneratePac(req)

	resp := &http.Response{
		Status:        "200 OK",
		StatusCode:    200,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Request:       req,
		Close:         true,
		ContentLength: int64(len(data)),
		Body:          ioutil.NopCloser(bytes.NewReader([]byte(data))),
	}

	glog.Infof("%s \"AUTOPROXY %s %s %s\" %d %s", req.RemoteAddr, req.Method, req.RequestURI, req.Proto, resp.StatusCode, resp.Header.Get("Content-Length"))

	return ctx, resp, nil
}
