package main

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestTextHandler(t *testing.T) {
	var sb strings.Builder
	log := slog.New(&textHandler{w: &sb, mu: new(sync.Mutex)}).With("client", "c1")
	log.Info("client connected", "ip", "127.0.0.1", "name", "Big Bob")
	got := sb.String()
	want := "INFO  client connected                      client=c1  ip=127.0.0.1  name=\"Big Bob\"\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("got %q\nwant suffix %q", got, want)
	}
}
