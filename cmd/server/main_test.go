package main

import (
	"bytes"
	"encoding/json"
	"matching-service/internal/matching"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishAtomicAndRestart(t *testing.T) {
	b, err := os.ReadFile("../../examples/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var c matching.Config
	if err = json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	e, err := matching.New(c)
	if err != nil {
		t.Fatal(err)
	}
	s := &service{path: filepath.Join(t.TempDir(), "snapshot.json"), token: "a-long-test-admin-token-123"}
	s.current.Store(&snapshot{e, 1})
	publish := func(c matching.Config, auth bool) int {
		b, _ := json.Marshal(c)
		r := httptest.NewRequest(http.MethodPut, "/v1/config", bytes.NewReader(b))
		if auth {
			r.Header.Set("Authorization", "Bearer "+s.token)
		}
		w := httptest.NewRecorder()
		s.admin(w, r)
		return w.Code
	}
	c.Version = 2
	if got := publish(c, false); got != 401 {
		t.Fatal(got)
	}
	if got := publish(c, true); got != 200 {
		t.Fatal(got)
	}
	if got := publish(c, true); got != 409 {
		t.Fatal(got)
	}
	c.Version = 3
	c.Candidates[0].ID = "invalid"
	if got := publish(c, true); got != 400 {
		t.Fatal(got)
	}
	if s.current.Load().version != 2 {
		t.Fatal("invalid config replaced active snapshot")
	}
	saved, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	var restored matching.Config
	if err = json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	if _, err = matching.New(restored); err != nil || restored.Version != 2 {
		t.Fatalf("restart: %v", err)
	}
	s.path = "/dev/null/not-writable"
	restored.Version = 3
	if got := publish(restored, true); got != 500 {
		t.Fatal(got)
	}
	if s.current.Load().version != 2 {
		t.Fatal("failed persistence changed active snapshot")
	}
}
