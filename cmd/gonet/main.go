package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gonet/internal/client"
	"gonet/internal/server"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "server":
		if err := runServer(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "client":
		if err := runClient(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "version":
		fmt.Printf("gonet version=%s commit=%s date=%s\n", version, commit, date)
	default:
		usage()
		os.Exit(2)
	}
}

func runServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	listen := fs.String("listen", ":7000", "listen address for udp/tcp control")
	stale := fs.Duration("stale", 90*time.Second, "client stale timeout")
	requestTTL := fs.Duration("request-ttl", 2*time.Minute, "connect request ttl")
	cleanup := fs.Duration("cleanup", 15*time.Second, "cleanup interval")
	spread := fs.Int("predict-spread", 4, "predicted port spread per side")
	if err := fs.Parse(args); err != nil {
		return err
	}

	srv := server.New(server.Config{
		Listen:           *listen,
		ClientStaleAfter: *stale,
		RequestTTL:       *requestTTL,
		CleanupInterval:  *cleanup,
		PredictSpread:    *spread,
	})
	return srv.Run(ctx)
}

func runClient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	connectAddr := fs.String("connect", "", "server address, for example 1.2.3.4:7000")
	group := fs.String("group", "default", "group name")
	control := fs.String("control", "udp", "control protocol: udp or tcp")
	punchTimeout := fs.Duration("punch-timeout", 8*time.Second, "hole punching timeout")
	spread := fs.Int("predict-spread", 4, "expected peer port spread")
	forceRelay := fs.Bool("force-relay", false, "skip p2p punching and use control-channel relay")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *connectAddr == "" {
		return fmt.Errorf("--connect is required")
	}

	cli, err := client.New(client.Config{
		ServerAddr:    *connectAddr,
		Group:         *group,
		ControlProto:  *control,
		PunchTimeout:  *punchTimeout,
		PredictSpread: *spread,
		ForceRelay:    *forceRelay,
	})
	if err != nil {
		return err
	}
	return cli.Run(ctx, os.Stdin, os.Stdout)
}

func usage() {
	fmt.Println("Usage:")
	fmt.Println("  gonet server [--listen :7000]")
	fmt.Println("  gonet client --connect 127.0.0.1:7000 --group demo [--control udp|tcp]")
	fmt.Println("  gonet version")
}
