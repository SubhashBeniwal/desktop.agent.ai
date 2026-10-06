package config

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	in := Default()
	in.ServerURL = "wss://relay.example.com/agent"
	in.Token = "secret"
	in.Workspaces = []string{t.TempDir()}
	in.HeartbeatInterval = Duration(45 * time.Second)

	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestHasDaemonID(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.yaml")
	if HasDaemonID(missing) {
		t.Fatal("missing file reported a daemon id")
	}
	cfg := Default()
	if err := Save(missing, cfg); err != nil {
		t.Fatal(err)
	}
	if !HasDaemonID(missing) {
		t.Fatal("saved config has no daemon id")
	}
}
