package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	"github.com/sharpintelligence/doorstep/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}
