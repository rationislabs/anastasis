// Command anastasis runs Compose projects on rootless Podman with replicas,
// self-healing, load balancing and rolling updates.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/rationislabs/anastasis/internal/api"
	"github.com/rationislabs/anastasis/internal/lb"
	"github.com/rationislabs/anastasis/internal/reconcile"
	"github.com/rationislabs/anastasis/internal/runtime"
	"github.com/rationislabs/anastasis/internal/state"
)

var version = "dev" // set with -ldflags "-X main.version=..."

const usageText = `usage: anastasis [-socket path] <command>

commands:
  serve [-state path] [-runtime path]   run anastasis (the reconcile loop and the API socket)
  up [-f compose.json] [-d]             register or update a project and wait until it converges
  down <project> [--volumes]            remove a project's containers and network
  scale <project> <service>=<n> ...     set replica counts until the next up
  status [project]                      show services
  rollout restart <project> <service>   roll every replica and re-pull the image
  version                               print the version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "anastasis:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("anastasis", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	socket := global.String("socket", filepath.Join(runtimeDir(), "anastasis.sock"), "controller API socket")
	if err := global.Parse(args); err != nil {
		return err
	}
	if global.NArg() == 0 {
		global.Usage()
		return errors.New("missing command")
	}
	cmd, args := global.Arg(0), global.Args()[1:]
	cl := api.NewClient(*socket)
	switch cmd {
	case "serve":
		return serve(*socket, args)
	case "up":
		return up(cl, args)
	case "down":
		project, volumes := "", false
		for _, a := range args {
			if a == "--volumes" || a == "-volumes" {
				volumes = true
			} else {
				project = a
			}
		}
		if project == "" {
			return errors.New("usage: anastasis down <project> [--volumes]")
		}
		return cl.Down(project, volumes)
	case "scale":
		if len(args) < 2 {
			return errors.New("usage: anastasis scale <project> <service>=<n> ...")
		}
		scale, err := parseScale(args[1:])
		if err != nil {
			return err
		}
		return cl.Scale(args[0], scale)
	case "status":
		project := ""
		if len(args) > 0 {
			project = args[0]
		}
		st, err := cl.Status(project)
		if err != nil {
			return err
		}
		printStatus(os.Stdout, st)
		return nil
	case "rollout":
		if len(args) != 3 || args[0] != "restart" {
			return errors.New("usage: anastasis rollout restart <project> <service>")
		}
		return cl.RolloutRestart(args[1], args[2])
	case "version":
		fmt.Println(version)
		return nil
	}
	global.Usage()
	return fmt.Errorf("unknown command %q", cmd)
}

func serve(socket string, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	statePath := fs.String("state", defaultStatePath(), "state file")
	runtimeSock := fs.String("runtime", filepath.Join(runtimeDir(), "podman", "podman.sock"), "Podman (or Docker) API socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := state.Open(*statePath)
	if err != nil {
		return err
	}
	defer store.Close()
	sock, err := api.Listen(socket)
	if err != nil {
		return err
	}
	rt, err := runtime.New(*runtimeSock)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	balancer := lb.New("")
	defer balancer.Shutdown()
	ctrl := reconcile.New(store, rt, balancer)
	go ctrl.Run(ctx)
	slog.Info("anastasis serving", "version", version, "socket", socket, "state", *statePath, "runtime", *runtimeSock)
	// Containers keep running after serve exits; the next serve adopts them.
	return api.Serve(ctx, sock, ctrl)
}

func up(cl *api.Client, args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	file := fs.String("f", "", "Compose file (default: compose.json, else compose.yaml, in this directory)")
	detach := fs.Bool("d", false, "return without waiting for services to converge")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		f, err := defaultComposeFile(".")
		if err != nil {
			return err
		}
		*file = f
	}
	path, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	project, err := cl.Up(path, os.Environ())
	if err != nil {
		return err
	}
	if *detach {
		fmt.Println(project)
		return nil
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		st, err := cl.Status(project)
		if err != nil {
			return err
		}
		if done, err := upResult(st); done {
			printStatus(os.Stdout, st)
			return err
		}
		if time.Now().After(deadline) {
			printStatus(os.Stdout, st)
			return fmt.Errorf("project %s did not converge within 5m", project)
		}
		time.Sleep(time.Second)
	}
}

// defaultComposeFile finds the project file in dir. JSON comes first; YAML
// still works for existing Compose files.
func defaultComposeFile(dir string) (string, error) {
	names := []string{"compose.json", "compose.yaml", "compose.yml"}
	for _, n := range names {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no %s in %s; pass -f", strings.Join(names, ", "), dir)
}

// upResult reports whether up can stop waiting, and with what error.
func upResult(st reconcile.Status) (bool, error) {
	for _, s := range st.Services {
		if s.RolledBack != "" {
			return true, fmt.Errorf("%s/%s %s", s.Project, s.Service, s.RolledBack)
		}
		if s.Paused {
			return true, fmt.Errorf("%s/%s %s", s.Project, s.Service, serviceState(s))
		}
	}
	return converged(st), nil
}

func converged(st reconcile.Status) bool {
	if st.RuntimeError != "" || len(st.Services) == 0 {
		return false
	}
	for _, s := range st.Services {
		if !s.Converged() {
			return false
		}
	}
	return true
}

func parseScale(args []string) (map[string]int, error) {
	if len(args) == 0 {
		return nil, errors.New("give at least one <service>=<n>")
	}
	out := map[string]int{}
	for _, a := range args {
		name, n, ok := strings.Cut(a, "=")
		count, err := strconv.Atoi(n)
		if !ok || name == "" || err != nil {
			return nil, fmt.Errorf("bad scale %q, want <service>=<n>", a)
		}
		out[name] = count
	}
	return out, nil
}

func printStatus(w io.Writer, st reconcile.Status) {
	if st.RuntimeError != "" {
		fmt.Fprintf(w, "runtime unreachable: %s\n", st.RuntimeError)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tSERVICE\tREADY\tOUTDATED\tPORT\tAUTO\tSTATE")
	for _, s := range st.Services {
		port := "-"
		if s.Port > 0 {
			port = strconv.Itoa(s.Port)
		}
		auto := "-"
		if s.Auto != "" {
			auto = s.Auto
		}
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%d\t%s\t%s\t%s\n", s.Project, s.Service, s.Ready, s.Desired, s.Outdated, port, auto, serviceState(s))
	}
	tw.Flush()
	for _, s := range st.Services {
		for _, warn := range s.Warnings {
			fmt.Fprintf(w, "warning: %s/%s: %s\n", s.Project, s.Service, warn)
		}
	}
	for _, warn := range st.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
}

func serviceState(s reconcile.ServiceStatus) string {
	switch {
	case s.Error != "":
		return "error: " + s.Error
	case s.RolledBack != "":
		return s.RolledBack
	case s.Paused:
		if old := s.Serving - s.Ready; old > 0 {
			return fmt.Sprintf("paused: %s (%d old replicas still serving)", s.Reason, old)
		}
		return "paused: " + s.Reason
	case !s.BackoffUntil.IsZero():
		return "crash backoff until " + s.BackoffUntil.Local().Format(time.TimeOnly)
	case s.Outdated > 0:
		return "rolling out"
	case s.Ready < s.Desired:
		return "starting"
	}
	return "ok"
}

func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return fmt.Sprintf("/run/user/%d", os.Getuid())
}

func defaultStatePath() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "anastasis", "state.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "anastasis", "state.json")
}
