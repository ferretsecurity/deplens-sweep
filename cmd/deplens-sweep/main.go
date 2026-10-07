package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ferretsecurity/deplens-sweep/internal/sweep"
)

func main() { os.Exit(run()) }
func run() int {
	fs := flag.NewFlagSet("deplens-sweep", flag.ContinueOnError)
	path := fs.String("config", "", "YAML configuration path")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}
	if *path == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: deplens-sweep --config config.yaml")
		return 1
	}
	cfg, err := sweep.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	batch, err := sweep.Run(context.Background(), cfg)
	if batch.ID != "" {
		json.NewEncoder(os.Stdout).Encode(batch)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
