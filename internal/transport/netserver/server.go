// Package netserver runs a TCP accept loop and tracks live connections so
// that the process can shut down cleanly.
package netserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Handler serves one client connection. Serve must return soon after the
// connection is closed; the server closes it when Serve returns.
type Handler interface {
	Serve(ctx context.Context, conn net.Conn)
}

// Server accepts connections on one address and hands them to a Handler.
type Server struct {
	name    string
	handler Handler
	log     *slog.Logger

	ln    net.Listener
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func New(name string, handler Handler, log *slog.Logger) *Server {
	return &Server{name: name, handler: handler, log: log, conns: make(map[net.Conn]struct{})}
}

// Listen binds the address. It is separate from Serve so that a busy port is
// reported before the process announces itself as started.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

// Addr returns the bound address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Serve accepts connections until the listener is closed by Shutdown.
func (s *Server) Serve(ctx context.Context) error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("panic in connection handler", "server", s.name, "panic", r)
				}
				conn.Close()
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()
			s.handler.Serve(ctx, conn)
		}()
	}
}

// Shutdown stops accepting, gives open sessions the grace period to finish
// and then closes whatever is left.
func (s *Server) Shutdown(grace time.Duration) {
	s.ln.Close()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(grace):
	}
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	<-done
}
