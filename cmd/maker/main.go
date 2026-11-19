package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/SENTINELITE/elgato-makers-cli/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cli.New(cli.Options{}).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "maker:", err)
		os.Exit(1)
	}
}
