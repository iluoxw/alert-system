package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/luoxw/iacctl-go/internal/inspect"
	"github.com/luoxw/iacctl-go/internal/prom"
	"github.com/luoxw/iacctl-go/packs"
)

var (
	insWindow string
	insStart  string
	insEnd    string
	insFormat string
)

var inspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "PRO 巡检垂直切片（L0/L1/L3-mysql），总体取最严重项",
	Long: "Runs the PRO inspection slice: l0.sample_age (thanos instant),\n" +
		"l0.stats_gap (OpenObserve SQL, 30-minute slices), l1.qps / l1.5xx / l1.401 /\n" +
		"l1.admission (thanos range + local differencing) and l3.mysql_running\n" +
		"(OpenObserve gauge). Thresholds are provisional for PRO (known-items #22).\n" +
		"Layers not implemented here are covered by the Python skill.",
	RunE: func(cmd *cobra.Command, args []string) error {
		res, err := packs.LoadIacGo()
		if err != nil {
			return err
		}
		var start, end time.Time
		var label string
		if insStart != "" || insEnd != "" {
			if insStart == "" || insEnd == "" {
				return fmt.Errorf("--start 和 --end 必须同时给出")
			}
			s, err1 := time.ParseInLocation("2006-01-02T15:04:05", insStart, inspect.CST)
			e, err2 := time.ParseInLocation("2006-01-02T15:04:05", insEnd, inspect.CST)
			if err1 != nil || err2 != nil {
				return fmt.Errorf("--start/--end 须是 ISO 时间（CST），例如 2026-11-11T22:00:00")
			}
			start, end, label = s, e, insStart+"–"+insEnd+" CST"
		} else {
			secs, err := parseWindow(insWindow)
			if err != nil {
				return err
			}
			if secs < 300 {
				return fmt.Errorf("窗口至少 5 分钟")
			}
			end = time.Now()
			start = end.Add(-time.Duration(secs) * time.Second)
			label = insWindow
		}

		thanos, err := newThanos()
		if err != nil {
			die(err.Error())
		}
		obs, err := newObs()
		if err != nil {
			die(err.Error())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		result, err := inspect.Run(ctx, thanos, obs, res, start, end, label)
		if err != nil {
			if _, ok := err.(*prom.AuthError); ok {
				die(err.Error())
			}
			return err
		}

		switch insFormat {
		case "summary":
			fmt.Println(result.RenderSummary())
		case "json":
			raw, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(raw))
		default:
			fmt.Println(result.RenderSummary())
			raw, _ := json.MarshalIndent(result, "", "  ")
			fmt.Println()
			fmt.Println(string(raw))
		}
		return nil
	},
}

func init() {
	inspectCmd.Flags().StringVar(&insWindow, "window", "1h", "相对窗口，例如 3h、90m")
	inspectCmd.Flags().StringVar(&insStart, "start", "", "绝对开始时间（CST，与 --window 互斥）")
	inspectCmd.Flags().StringVar(&insEnd, "end", "", "绝对结束时间（CST）")
	inspectCmd.Flags().StringVar(&insFormat, "format", "both", "summary | json | both")
}
