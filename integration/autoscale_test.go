//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/autoscale"
	"github.com/rationislabs/anastasis/internal/reconcile"
)

var fast = autoscale.Windows{Check: 2 * time.Second, Average: 6 * time.Second, Down: 10 * time.Second, Step: 2 * time.Second}

func autoHarness(t *testing.T) *harness {
	t.Helper()
	return startWith(t, filepath.Join(t.TempDir(), "state.json"), func(c *reconcile.Controller) { c.ShortenAutoscaleWindowsForTest(fast) })
}

func upFile(t *testing.T, h *harness, yaml string) string {
	t.Helper()
	if err := os.WriteFile(h.compose, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := h.ctrl.Up(context.Background(), h.compose, nil)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func service(h *harness, project, name string) reconcile.ServiceStatus {
	for _, s := range h.ctrl.Status(project).Services {
		if s.Service == name {
			return s
		}
	}
	return reconcile.ServiceStatus{}
}

func waitService(t *testing.T, h *harness, project, name, what string, timeout time.Duration, ok func(reconcile.ServiceStatus) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok(service(h, project, name)) {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %s; last status %+v", what, service(h, project, name))
}

const inflightCompose = `name: anastasisauto
services:
  web:
    image: docker.io/traefik/whoami:v1.10.1
    deploy: {replicas: 1}
    x-anastasis:
      expose: "18082:80"
      autoscale: {min: 1, max: 4, inflight: 2}
`

func TestAutoscaleInflight(t *testing.T) {
	h := autoHarness(t)
	t.Cleanup(func() { h.ctrl.Down(context.Background(), "anastasisauto", true); h.stop() })
	upFile(t, h, inflightCompose)
	waitService(t, h, "anastasisauto", "web", "1 ready replica", 2*time.Minute, func(s reconcile.ServiceStatus) bool { return s.Ready == 1 })

	if err := h.ctrl.Scale(context.Background(), "anastasisauto", map[string]int{"web": 3}); err == nil || !strings.Contains(err.Error(), "autoscaled") {
		t.Fatalf("scale on autoscaled service = %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if resp, err := http.Get("http://127.0.0.1:18082/?wait=1s"); err == nil {
					resp.Body.Close()
				}
			}
		}()
	}
	waitService(t, h, "anastasisauto", "web", "scale up to 4", 2*time.Minute, func(s reconcile.ServiceStatus) bool { return s.Desired == 4 && s.Ready == 4 })
	close(stop)
	wg.Wait()
	waitService(t, h, "anastasisauto", "web", "scale back down to 1", 2*time.Minute, func(s reconcile.ServiceStatus) bool { return s.Desired == 1 && s.Ready == 1 })
}

const cpuCompose = `name: anastasiscpu
services:
  burn:
    image: docker.io/library/busybox:1.36
    command: ["sh", "-c", "while :; do :; done"]
    deploy:
      replicas: 1
      resources: {limits: {cpus: "0.5"}}
    x-anastasis:
      autoscale: {min: 1, max: 3, cpu: 50}
`

func TestAutoscaleCPU(t *testing.T) {
	h := autoHarness(t)
	t.Cleanup(func() { h.ctrl.Down(context.Background(), "anastasiscpu", true); h.stop() })
	upFile(t, h, cpuCompose)
	waitService(t, h, "anastasiscpu", "burn", "scale up to 3 on cpu", 2*time.Minute, func(s reconcile.ServiceStatus) bool {
		return s.Desired == 3 && s.Ready == 3
	})
	if s := service(h, "anastasiscpu", "burn"); !strings.Contains(s.Auto, "cpu") {
		t.Fatalf("status auto = %q, want the cpu signal in it", s.Auto)
	}
	fmt.Println("cpu autoscale:", service(h, "anastasiscpu", "burn").Auto)
}
