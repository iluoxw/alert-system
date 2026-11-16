package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/luoxw/iacctl-go/internal/prom"
	"github.com/luoxw/iacctl-go/internal/runtimecfg"
)

var sourcesProbe bool

var sourcesCmd = &cobra.Command{
	Use:   "sources",
	Short: "已配置的后端与凭据状态（JSON）；--probe 做只读连通性检查",
	RunE: func(cmd *cobra.Command, args []string) error {
		type src struct {
			Name    string `json:"name"`
			BaseURL string `json:"base_url"`
			Role    string `json:"role"`
			Auth    string `json:"auth"`
			Probe   string `json:"probe,omitempty"`
		}
		cfg := runtimecfg.Current
		out := []src{
			{Name: "thanos", BaseURL: cfg.Thanos.BaseURL, Role: "metrics (iac_*, process_cpu)", Auth: "none"},
			{Name: "obs", BaseURL: cfg.Obs.BaseURL, Role: "mysql 指标 + 日志/trace SQL", Auth: "missing " + cfg.Obs.AuthEnv},
		}
		if _, err := newObs(); err == nil {
			out[1].Auth = "configured (" + cfg.Obs.AuthEnv + ")"
		}
		if sourcesProbe {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			for i := range out {
				var c *prom.Client
				var err error
				if out[i].Name == "thanos" {
					c, err = newThanos()
				} else {
					c, err = newObs()
				}
				if err != nil {
					out[i].Probe = "error: " + err.Error()
					continue
				}
				t0 := time.Now()
				if _, err := c.Prom(ctx, "1", float64(time.Now().Unix())); err != nil {
					out[i].Probe = "fail: " + err.Error()
					continue
				}
				out[i].Probe = fmt.Sprintf("ok (%dms)", time.Since(t0).Milliseconds())
			}
		}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	},
}

func init() {
	sourcesCmd.Flags().BoolVar(&sourcesProbe, "probe", false, "对每个后端发一条只读即时查询")
}
