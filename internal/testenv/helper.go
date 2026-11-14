package testenv

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// helperMain is a stand-in for the servers rov runs. Flags:
//
//	--listen ADDR      listen on ADDR (exit 98 if it's taken) and print
//	                   "listening on <port>"
//	--child            don't do the work: run a child helper with the other
//	                   flags and wait for it (just → go run → binary)
//	--setsid           with --child: the child calls setsid, leaving the
//	                   pane's process group
//	--parent-exit D    with --child --setsid: the parent exits 0 after D,
//	                   leaving the child behind (a wrapper that daemonises)
//	--color            print a few coloured lines first
//	--spam N           print N numbered lines
//	--crash CODE       exit with CODE …
//	--after D          … after D (default 0)
//	--term-delay D     on SIGTERM, wait D before exiting
//	--ignore-term      ignore SIGTERM and SIGHUP (only SIGKILL stops it)
//	--echo             echo stdin lines as "got: <line>"
//	--life D           exit 0 after D at the latest (default 2m: no strays)
func helperMain(argv []string) int {
	fs := flag.NewFlagSet("helper", flag.ContinueOnError)
	listen := fs.String("listen", "", "")
	child := fs.Bool("child", false, "")
	setsid := fs.Bool("setsid", false, "")
	color := fs.Bool("color", false, "")
	spam := fs.Int("spam", 0, "")
	crash := fs.Int("crash", -1, "")
	after := fs.Duration("after", 0, "")
	termDelay := fs.Duration("term-delay", 0, "")
	ignoreTerm := fs.Bool("ignore-term", false, "")
	echo := fs.Bool("echo", false, "")
	life := fs.Duration("life", 2*time.Minute, "")
	parentExit := fs.Duration("parent-exit", 0, "")
	if err := fs.Parse(argv); err != nil {
		return 2
	}

	if *child {
		var args []string
		for _, a := range argv {
			if a != "--child" && a != "--setsid" && !strings.HasPrefix(a, "--parent-exit") {
				args = append(args, a)
			}
		}
		self, _ := os.Executable()
		cmd := exec.Command(self, append([]string{"__helper"}, args...)...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if *setsid {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		fmt.Printf("parent %d starting child\n", os.Getpid())
		if err := cmd.Start(); err != nil {
			fmt.Println("start child:", err)
			return 1
		}
		if *setsid {
			// Like a wrapper that daemonises its server: the parent goes
			// away on SIGTERM, the server stays unless rov finds it.
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var exit <-chan time.Time
			if *parentExit > 0 {
				exit = time.After(*parentExit)
			}
			select {
			case <-sig:
				return 0
			case <-exit:
				fmt.Println("parent exiting, child stays")
				return 0
			case err := <-done:
				return exitCode(err)
			}
		}
		return exitCode(cmd.Wait())
	}

	go func() {
		time.Sleep(*life)
		os.Exit(0)
	}()
	if *ignoreTerm {
		// The hangup that follows the pane's exit too.
		signal.Ignore(syscall.SIGHUP)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	go func() {
		for range sig {
			if *ignoreTerm {
				fmt.Println("ignoring SIGTERM")
				continue
			}
			fmt.Println("got SIGTERM")
			time.Sleep(*termDelay)
			os.Exit(0)
		}
	}()

	fmt.Printf("helper %d up\n", os.Getpid())
	if *color {
		fmt.Print("\x1b[31mred line\x1b[0m\n\x1b[1;32mbold green\x1b[0m plain tail\n")
	}
	for i := 1; i <= *spam; i++ {
		fmt.Printf("line %06d %s\n", i, strings.Repeat("x", 40))
	}
	if *listen != "" {
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			fmt.Println("listen:", err)
			return 98
		}
		defer ln.Close()
		fmt.Printf("listening on %d\n", ln.Addr().(*net.TCPAddr).Port)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	}
	if *echo {
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				fmt.Println("got: " + sc.Text())
			}
		}()
	}
	if *crash >= 0 {
		time.Sleep(*after)
		fmt.Printf("crashing with %d\n", *crash)
		return *crash
	}
	select {}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		return 1
	}
	return 1
}
