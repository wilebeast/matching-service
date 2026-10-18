package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestShutdownWaitsForActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	shutdownStarted := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "completed")
	})}
	srv.RegisterOnShutdown(func() { close(shutdownStarted) })
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- serveHTTP(srv, listener, signals) }()
	defer srv.Close()
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			defer response.Body.Close()
			var b []byte
			b, err = io.ReadAll(response.Body)
			if string(b) != "completed" && err == nil {
				err = io.ErrUnexpectedEOF
			}
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request not started")
	}
	signals <- syscall.SIGTERM
	select {
	case <-shutdownStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown not started")
	}
	var early bool
	select {
	case <-done:
		early = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if early {
		t.Fatal("server returned before active request finished")
	}
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}
