package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"

	"github.com/open-conduit/open-conduit/client"
)

// streamTerminal is attach through the daemon: it runs the session beside the
// run and relays its terminal here, so this machine needs nothing but a
// credential. The daemon holds the Work and releases it when this ends,
// however it ends.
func (o *operator) streamTerminal(c *client.Client, id string, keep bool) error {
	var stdin io.Reader = os.Stdin
	if o.env.Stdin != nil {
		stdin = o.env.Stdin
	}
	opts := client.TerminalOptions{Term: os.Getenv("TERM"), KeepHeld: keep}
	file, isFile := stdin.(*os.File)
	raw := isFile && term.IsTerminal(file.Fd())
	if raw {
		if w, h, err := term.GetSize(file.Fd()); err == nil && w > 0 && h > 0 {
			opts.Cols, opts.Rows = uint16(w), uint16(h)
		}
	}
	cx, cancel := ctx()
	t, err := c.OpenTerminal(cx, id, opts)
	cancel()
	if err != nil {
		return err
	}
	defer t.Detach()
	fmt.Fprintf(o.errOut, "open-conduit: attached to %s through the daemon; new events queue until you exit\r\n", id)
	if raw {
		state, err := term.MakeRaw(file.Fd())
		if err != nil {
			return err
		}
		defer func() { _ = term.Restore(file.Fd(), state) }()
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			if s != syscall.SIGWINCH {
				_ = t.Detach()
				return
			}
			if raw {
				if w, h, err := term.GetSize(file.Fd()); err == nil && w > 0 && h > 0 {
					_ = t.Resize(uint16(w), uint16(h))
				}
			}
		}
	}()
	go func() { _, _ = io.Copy(t, stdin) }()
	_, err = io.Copy(o.out, t)
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("the terminal connection was lost: %w", err)
	}
	if code, ok := t.ExitCode(); ok && code != 0 {
		return fmt.Errorf("the session exited with code %d", code)
	}
	return nil
}
