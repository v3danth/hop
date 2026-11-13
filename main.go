// Command hop gives local apps and favourite websites short names like yt/
// or talk-to-db/, so nobody has to remember port numbers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"
)

// defaultAddr gives hop its own loopback address. Browsers need port 80 for
// links like yt/, and a separate address leaves 127.0.0.1:80 (localhost)
// free for other apps. The CLI and MCP server assume it unless $HOP_URL is set.
const defaultAddr = "127.0.0.2:80"

const usage = `hop: short names for websites and local apps

Usage:
  hop serve [-addr 127.0.0.2:80] [-data FILE] [-hosts /etc/hosts]
  hop ls                          list shortcuts
  hop add [-f] <name> <url|port>  e.g. hop add yt youtube.com; -f replaces an existing name
  hop rm <name>                   remove a shortcut
  hop run <name> -- <command...>  run an app on a free port (given as $PORT)
  hop port                        print a free port
  hop mcp                         MCP server for Claude Code (stdio)

The CLI talks to the server at $HOP_URL (default http://127.0.0.2).
`

var commands = map[string]func([]string) error{
	"serve": serveCmd,
	"ls":    listCmd,
	"add":   addCmd,
	"rm":    removeCmd,
	"run":   runCmd,
	"port":  portCmd,
	"mcp":   func([]string) error { return serveMCP(os.Stdin, os.Stdout, newClient()) },
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "hop:", err)
		os.Exit(1)
	}
}

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address to listen on; hosts entries point at its IP")
	data := fs.String("data", "", "registry file (default ~/.hop/registry.json)")
	hosts := fs.String("hosts", "", "hosts file to keep in sync, e.g. /etc/hosts (enables single-word names like yt/)")
	fs.Parse(args)
	// launchd daemons have no $HOME, so only look it up when -data is missing.
	if *data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("find home directory (or pass -data): %w", err)
		}
		*data = filepath.Join(home, ".hop", "registry.json")
	}

	listenHost, portText, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("parse -addr: %w", err)
	}
	hostsIP, err := loopbackIP(listenHost)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("parse -addr port: %w", err)
	}
	reg, err := loadRegistry(*data)
	if err != nil {
		return err
	}
	reg.ReapDead()
	ln, err := net.Listen("tcp", *addr)
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		return fmt.Errorf("listen on %s: macOS only enables 127.0.0.1 by default; run `sudo ifconfig lo0 alias %s up` first: %w", *addr, listenHost, err)
	}
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := newServer(reg, port, *hosts != "")
	go srv.maintain(ctx, *hosts, hostsIP)
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hs.Shutdown(shutdownCtx); err != nil {
			slog.Warn("shutdown cut open connections", "err", err)
		}
	}()

	slog.Info("hop listening", "addr", *addr, "registry", *data, "hosts", *hosts, "dashboard", srv.serviceLink("hop")+"/")
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	reg.SaveIfDirty()
	slog.Info("hop stopped")
	return nil
}

// loopbackIP is the address hosts entries point at. hop must listen on one
// loopback address, since a wildcard would expose it to the network.
func loopbackIP(host string) (string, error) {
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("-addr must use a loopback IP such as 127.0.0.2, not %q", host)
	}
	return ip.String(), nil
}

func listCmd([]string) error {
	resp, err := newClient().list()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LINK\tGOES TO\tSTATUS\tVISITS")
	for _, s := range resp.Services {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", s.Link, s.target(), s.Status, s.Hits)
	}
	return tw.Flush()
}

func addCmd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	var force bool
	fs.BoolVar(&force, "f", false, "replace the name if it already exists")
	fs.BoolVar(&force, "force", false, "same as -f")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return errors.New("usage: hop add [-f] <name> <url|port>")
	}
	name, target := fs.Arg(0), fs.Arg(1)
	svc := parseTarget(target)
	svc.Name = name
	view, err := newClient().put(svc, force)
	if isNameTaken(err) {
		return fmt.Errorf("%w\nTo replace it: hop add -f %s %s", err, name, target)
	}
	if err != nil {
		return err
	}
	fmt.Println(view.Link)
	return nil
}

func removeCmd(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: hop rm <name>")
	}
	return newClient().remove(args[0])
}

func portCmd([]string) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	fmt.Println(port)
	return nil
}

// runCmd starts an app on a free port and registers it under name for as long
// as it runs. The server also reaps the name by PID if this wrapper is killed.
func runCmd(args []string) error {
	if len(args) < 3 || args[1] != "--" {
		return errors.New("usage: hop run <name> -- <command...>")
	}
	name, argv := args[0], args[2:]
	port, err := freePort()
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "PORT="+strconv.Itoa(port), "HOP_NAME="+name)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", argv[0], err)
	}

	c := newClient()
	if view, err := c.put(Service{Name: name, Port: port, PID: cmd.Process.Pid}, true); err != nil {
		fmt.Fprintf(os.Stderr, "hop: running on port %d, but could not register %s: %v\n", port, name, err)
	} else {
		fmt.Fprintf(os.Stderr, "hop: %s -> %s (port %d)\n", name, view.Link, port)
	}

	// Ctrl-C reaches the child through the terminal's process group; the
	// wrapper only forwards SIGTERM and waits so it can unregister afterwards.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		for sig := range signals {
			if sig == syscall.SIGTERM {
				cmd.Process.Signal(sig)
			}
		}
	}()
	waitErr := cmd.Wait()
	if err := c.remove(name); err != nil {
		fmt.Fprintf(os.Stderr, "hop: could not remove %s, the server will drop it within seconds: %v\n", name, err)
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		// Shells report a signal death as 128+signal; ExitCode gives -1.
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			os.Exit(128 + int(ws.Signal()))
		}
		os.Exit(exitErr.ExitCode())
	}
	return waitErr
}
