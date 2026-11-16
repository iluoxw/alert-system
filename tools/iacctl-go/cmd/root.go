// Package cmd wires the cobra command tree.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/luoxw/iacctl-go/internal/prom"
	"github.com/luoxw/iacctl-go/internal/runtimecfg"
)

// version is stamped at build time:
//
//	go build -ldflags "-X github.com/luoxw/iacctl-go/cmd.version=0.1.0"
var version = "dev"

var configPath string

var rootCmd = &cobra.Command{
	Use:   "iacctl-go",
	Short: "IAC Go 版（auth-center PRO）只读可观测 CLI：thanos metrics + OpenObserve",
	Long: "iacctl-go inspects the Golang auth-center on PRO.\n" +
		"Metrics come from thanos-inner (CCloud psdcloud-app-monitor scrape, selected by\n" +
		"app_id); OpenObserve provides mysql gauges and the log/trace SQL search.\n" +
		"Counters are differenced locally: no increase(), no subqueries; counter queries\n" +
		"reach one step past the window; samples newer than the fetch time are dropped.\n" +
		"All commands are read-only.",
	Version:      version,
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := runtimecfg.Load(configPath)
		if err != nil {
			return err
		}
		cfg.Apply()
		return nil
	},
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "",
		"runtime config YAML (default $IACCTL_GO_CONFIG; built-in PRO defaults)")
	rootCmd.AddCommand(inspectCmd)
	rootCmd.AddCommand(toolsCmd)
	rootCmd.AddCommand(sourcesCmd)
	rootCmd.AddCommand(queryCmd)
	rootCmd.AddCommand(skillsCmd)
}

// newThanos builds the metrics client (no auth).
func newThanos() (*prom.Client, error) {
	b := runtimecfg.Current.Thanos
	return prom.New(b.BaseURL, b.ThanosPromPath(), "", "")
}

// newObs builds the OpenObserve client (Basic auth from the configured env).
func newObs() (*prom.Client, error) {
	b := runtimecfg.Current.Obs
	return prom.New(b.BaseURL, b.ObsPromPath(), b.ObsSearchPath(), b.AuthEnv)
}

// die prints to stderr and exits 2, mirroring the Python skill: an auth
// failure never produces a (potentially green) report.
func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}
