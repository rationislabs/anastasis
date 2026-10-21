package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rationislabs/anastasis/internal/reconcile"
)

func TestParseScale(t *testing.T) {
	got, err := parseScale([]string{"web=5", "api=0"})
	if err != nil || !reflect.DeepEqual(got, map[string]int{"web": 5, "api": 0}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range [][]string{nil, {"web"}, {"web=x"}, {"=3"}} {
		if _, err := parseScale(bad); err == nil {
			t.Errorf("parseScale(%q) accepted", bad)
		}
	}
}

func TestUpStopsOnRollback(t *testing.T) {
	st := reconcile.Status{Services: []reconcile.ServiceStatus{{Project: "p", Service: "web", Desired: 1, Ready: 1, RolledBack: "rolled back to the previous version: new replicas failed during rollout"}}}
	done, err := upResult(st)
	if !done || err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("upResult = %v, %v; want done with a rollback error", done, err)
	}
	ok := reconcile.Status{Services: []reconcile.ServiceStatus{{Project: "p", Service: "web", Desired: 1, Ready: 1}}}
	if done, err := upResult(ok); !done || err != nil {
		t.Fatalf("converged status: %v, %v", done, err)
	}
	if done, _ := upResult(reconcile.Status{Services: []reconcile.ServiceStatus{{Desired: 2, Ready: 1}}}); done {
		t.Fatal("not converged but done")
	}
}

// A paused rollout never converges on its own; up must say so instead of
// waiting out its deadline.
func TestUpStopsOnPause(t *testing.T) {
	st := reconcile.Status{Services: []reconcile.ServiceStatus{{Project: "p", Service: "web", Desired: 2, Outdated: 2, Paused: true, Reason: "new replicas failed during rollout"}}}
	done, err := upResult(st)
	if !done || err == nil || !strings.Contains(err.Error(), "new replicas failed during rollout") {
		t.Fatalf("upResult = %v, %v; want done with the pause reason", done, err)
	}
}

func TestServiceStatePausedSaysWhoServes(t *testing.T) {
	got := serviceState(reconcile.ServiceStatus{Desired: 2, Outdated: 2, Serving: 2, Paused: true, Reason: "new replicas failed during rollout"})
	if want := "paused: new replicas failed during rollout (2 old replicas still serving)"; got != want {
		t.Fatalf("serviceState = %q, want %q", got, want)
	}
}

func TestDefaultComposeFilePrefersJSON(t *testing.T) {
	dir := t.TempDir()
	if _, err := defaultComposeFile(dir); err == nil || !strings.Contains(err.Error(), "compose.json") {
		t.Fatalf("empty dir: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("{}"), 0o600)
	if got, _ := defaultComposeFile(dir); filepath.Base(got) != "compose.yaml" {
		t.Fatalf("only yaml: %q", got)
	}
	os.WriteFile(filepath.Join(dir, "compose.json"), []byte("{}"), 0o600)
	if got, _ := defaultComposeFile(dir); filepath.Base(got) != "compose.json" {
		t.Fatalf("json and yaml: %q, want json first", got)
	}
}
