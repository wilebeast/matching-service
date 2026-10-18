package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"matching-service/internal/matching"
)

type snapshot struct {
	engine  *matching.Engine
	version uint64
}
type service struct {
	current     atomic.Pointer[snapshot]
	publish     sync.Mutex
	path, token string
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return errors.New("one JSON document required")
	}
	return nil
}
func (s *service) admin(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
		respond(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var c matching.Config
	if err := decode(w, r, &c); err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.publish.Lock()
	defer s.publish.Unlock()
	if old := s.current.Load(); old != nil && c.Version <= old.version {
		respond(w, 409, map[string]string{"error": "version must increase"})
		return
	}
	e, err := matching.New(c)
	if err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	data, err := json.Marshal(c)
	if err == nil {
		err = save(s.path, data)
	}
	if err != nil {
		log.Printf("snapshot save: %v", err)
		respond(w, 500, map[string]string{"error": "snapshot persistence failed"})
		return
	}
	s.current.Store(&snapshot{e, c.Version})
	respond(w, 200, map[string]any{"version": c.Version, "published": true})
}
func save(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func main() {
	path := os.Getenv("SNAPSHOT_PATH")
	if path == "" {
		path = "/data/snapshot.json"
	}
	token := strings.TrimSpace(os.Getenv("ADMIN_TOKEN"))
	if len(token) < 24 {
		log.Fatal("ADMIN_TOKEN must contain at least 24 bytes")
	}
	s := &service{path: path, token: token}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		seed := os.Getenv("SEED_PATH")
		if seed == "" {
			seed = "examples/config.json"
		}
		data, err = os.ReadFile(seed)
		if err == nil {
			err = save(path, data)
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	var c matching.Config
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		log.Fatal(err)
	}
	e, err := matching.New(c)
	if err != nil {
		log.Fatal(err)
	}
	s.current.Store(&snapshot{e, c.Version})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		p := s.current.Load()
		if p == nil {
			respond(w, 503, map[string]string{"status": "loading"})
			return
		}
		respond(w, 200, map[string]any{"status": "ready", "version": p.version, "shard_id": 0})
	})
	mux.HandleFunc("PUT /v1/config", s.admin)
	mux.HandleFunc("POST /v1/match", func(w http.ResponseWriter, r *http.Request) {
		var req matching.Request
		if err := decode(w, r, &req); err != nil {
			respond(w, 400, map[string]string{"error": err.Error()})
			return
		}
		result, err := s.current.Load().engine.Match(req)
		if err != nil {
			respond(w, 422, map[string]string{"error": err.Error()})
			return
		}
		respond(w, 200, result)
	})
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatal(err)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	log.Printf("matching-service listening on %s, snapshot version=%d", srv.Addr, c.Version)
	if err := serveHTTP(srv, listener, stop); err != nil {
		log.Fatal(err)
	}
}

// serveHTTP owns the listener and the entire graceful shutdown lifecycle.
func serveHTTP(srv *http.Server, listener net.Listener, stop <-chan os.Signal) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	select {
	case err := <-serveErr:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-stop:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Shutdown closes the listener before draining requests. Wait here, on the
		// main lifecycle path, so process exit cannot interrupt the drain.
		err := srv.Shutdown(ctx)
		if err != nil {
			_ = srv.Close()
		}
		<-serveErr
		return err
	}
}
