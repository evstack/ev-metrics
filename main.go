package main

import (
	"fmt"
	"os"

	"github.com/evstack/ev-metrics/cmd"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "ev-metrics",
		Short: "DA monitoring tool for ev-node data availability",
	}

	rootCmd.AddCommand(cmd.NewMonitorCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
