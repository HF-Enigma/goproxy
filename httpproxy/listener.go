package httpproxy

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/golang/glog"
)

const (
	backlog = 1024
)

type filer interface {
	File() (*os.File, error)
}

type Listener interface {
	net.Listener
	filer

	Add(net.Conn) error
}

type racer struct {
	conn net.Conn
	err  error
}

type listener struct {
	ln              net.Listener
	lane            chan racer
	keepAlivePeriod time.Duration
	stopped         bool
	once            sync.Once
	mu              sync.Mutex
}

type ListenOptions struct {
	TLSConfig       *tls.Config
	KeepAlivePeriod time.Duration
}

func ListenTCP(network, addr string, opts *ListenOptions) (Listener, error) {
	laddr, err := net.ResolveTCPAddr(network, addr)
	if err != nil {
		return nil, err
	}

	ln0, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}

	var ln net.Listener
	if opts != nil && opts.TLSConfig != nil {
		ln = tls.NewListener(ln0, opts.TLSConfig)
	} else {
		ln = ln0
	}

	var keepAlivePeriod time.Duration
	if opts != nil && opts.KeepAlivePeriod > 0 {
		keepAlivePeriod = opts.KeepAlivePeriod
	}

	l := &listener{
		ln:              ln,
		lane:            make(chan racer, backlog),
		stopped:         false,
		keepAlivePeriod: keepAlivePeriod,
	}

	return l, nil

}

func (l *listener) Accept() (c net.Conn, err error) {
	l.once.Do(func() {
		go func() {
			var tempDelay time.Duration
			for {
				conn, err := l.ln.Accept()
				l.lane <- racer{conn, err}
				if err != nil {
					if ne, ok := err.(net.Error); ok && ne.Temporary() {
						if tempDelay == 0 {
							tempDelay = 5 * time.Millisecond
						} else {
							tempDelay *= 2
						}
						if max := 1 * time.Second; tempDelay > max {
							tempDelay = max
						}
						glog.Infof("httpproxy.Listener: Accept error: %v; retrying in %v", err, tempDelay)
						time.Sleep(tempDelay)
						continue
					}
					return
				}
			}
		}()
	})

	r := <-l.lane
	if r.err != nil {
		return r.conn, r.err
	}

	if l.keepAlivePeriod > 0 {
		if tc, ok := r.conn.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(l.keepAlivePeriod)
		}
	}

	return r.conn, nil
}

func (l *listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return nil
	}
	l.stopped = true
	close(l.lane)
	return l.ln.Close()
}

func (l *listener) Addr() net.Addr {
	return l.ln.Addr()
}

func (l *listener) File() (*os.File, error) {
	if f, ok := l.ln.(filer); ok {
		return f.File()
	}
	return nil, fmt.Errorf("%T does not has func File()", l.ln)
}

func (l *listener) Add(conn net.Conn) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.stopped {
		return fmt.Errorf("%#v already closed", l)
	}

	l.lane <- racer{conn, nil}

	return nil
}
