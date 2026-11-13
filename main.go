// Command hop gives local apps and favourite websites short names like yt/
// or talk-to-db/, so nobody has to remember port numbers.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
)

// hop listens on its own loopback address. Browsers need port 80 for links
// like yt/, and a separate address leaves localhost (127.0.0.1) free for
// other apps. The CLI and MCP server assume it unless $HOP_URL is set.
const (
	defaultIP   = "127.0.0.2"
	defaultAddr = defaultIP + ":80"
)

const usage = `hop: short names for websites and local apps

Usage:
  hop ls                               list shortcuts
  hop add [-f] <name> <url|port>       e.g. hop add yt youtube.com; -f replaces an existing name
  hop rm <name>                        remove a shortcut
  hop run [-f] <name> -- <command...>  run an app on a free port (given as $PORT)
  hop port                             print a free port
  hop mcp                              MCP server for Claude Code (stdio)
  hop serve [flags]                    the server itself; see hop serve -h

The CLI talks to the server at $HOP_URL (default http://` + defaultIP + `).
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
	switch os.Args[1] {
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
		return
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
	if err := fs.Parse(args); err != nil {
		return err
	}
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
	return newClient().remove(args[0], 0)
}

func portCmd([]string) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	fmt.Println(port)
	return nil
}

// runCmd starts an app on a free port and names it for as long as it runs.
// The server also drops the name by PID if this wrapper is killed.
func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	force := fs.Bool("f", false, "use the name even if it is a permanent shortcut")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 3 || rest[1] != "--" {
		return errors.New("usage: hop run [-f] <name> -- <command...>")
	}
	name, argv := strings.ToLower(rest[0]), rest[2:]
	c := newClient()
	replace, err := claimMode(c, name, *force)
	if err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}

	// Catch signals before the child exists, so a Ctrl-C right after start
	// cannot kill this wrapper before it unregisters the name.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "PORT="+strconv.Itoa(port), "HOP_NAME="+name)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", argv[0], err)
	}
	go forwardTerminate(signals, cmd.Process)

	pid := cmd.Process.Pid
	if view, err := c.put(Service{Name: name, Port: port, PID: pid}, replace); err != nil {
		fmt.Fprintf(os.Stderr, "hop: running on port %d, but could not register %s: %v\n", port, name, err)
	} else {
		fmt.Fprintf(os.Stderr, "hop: %s -> %s (port %d)\n", name, view.Link, port)
	}
	waitErr := cmd.Wait()
	// Passing the PID removes the name only while this app still holds it,
	// not after a newer run of the app took it over.
	if err := c.remove(name, pid); err != nil && !isNotFound(err) {
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

// claimMode reports whether hop run should replace an existing name. A name
// held by an earlier run of an app (it has a PID) is taken over, since that
// is usually the same app restarting; a permanent shortcut needs -f.
func claimMode(c client, name string, force bool) (replace bool, err error) {
	if force {
		return true, nil
	}
	resp, err := c.list()
	if err != nil {
		// Without a server the app still runs; registering it will say why.
		return false, nil
	}
	for _, s := range resp.Services {
		if s.Name != name {
			continue
		}
		if s.PID > 0 {
			return true, nil
		}
		return false, fmt.Errorf("%s/ already goes to %s\nTo use the name for this app anyway: hop run -f %s -- <command>", name, s.target(), name)
	}
	return false, nil
}

// forwardTerminate passes SIGTERM on to the app. Ctrl-C needs no forwarding:
// the terminal sends it to the whole process group, the app included.
func forwardTerminate(signals <-chan os.Signal, p *os.Process) {
	for sig := range signals {
		if sig != syscall.SIGTERM {
			continue
		}
		if err := p.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
			fmt.Fprintln(os.Stderr, "hop: could not pass SIGTERM to the app:", err)
		}
	}
}
