package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/luoxw/iacctl-go/internal/prom"
	"github.com/luoxw/iacctl-go/internal/series"
)

var (
	qSource  string
	qExpr    string
	qRange   bool
	qWindow  string
	qStep    int
	qSQL     string
	qSQLType string
	qSize    int
)

var queryCmd = &cobra.Command{
	Use:   "query",
	Short: "任意只读查询：query prom / query sql",
}

var queryPromCmd = &cobra.Command{
	Use:   "prom",
	Short: "只读 PromQL（--source thanos 或 obs）",
	RunE: func(cmd *cobra.Command, args []string) error {
		if qExpr == "" {
			return fmt.Errorf("--expr 必填")
		}
		var c *prom.Client
		var err error
		switch qSource {
		case "thanos":
			c, err = newThanos()
		case "obs":
			c, err = newObs()
		default:
			return fmt.Errorf("--source 只接受 thanos 或 obs，收到 %q", qSource)
		}
		if err != nil {
			die(err.Error())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if !qRange {
			rows, err := c.Prom(ctx, qExpr, float64(time.Now().Unix()))
			if err != nil {
				if _, ok := err.(*prom.AuthError); ok {
					die(err.Error())
				}
				return err
			}
			return printRows(rows)
		}
		secs, err := parseWindow(qWindow)
		if err != nil {
			return err
		}
		end := float64(time.Now().Unix())
		start := end - secs
		if qStep <= 0 {
			qStep = 300
		}
		rows, err := c.PromRange(ctx, qExpr, start, end, qStep)
		if err != nil {
			if _, ok := err.(*prom.AuthError); ok {
				die(err.Error())
			}
			return err
		}
		return printRows(rows)
	},
}

var querySQLCmd = &cobra.Command{
	Use:   "sql",
	Short: "OpenObserve 日志/trace SQL 检索（只读 SELECT）",
	RunE: func(cmd *cobra.Command, args []string) error {
		if qSQL == "" {
			return fmt.Errorf("--sql 必填")
		}
		if qSQLType != "logs" && qSQLType != "traces" {
			return fmt.Errorf("--type 只接受 logs 或 traces")
		}
		c, err := newObs()
		if err != nil {
			die(err.Error())
		}
		secs, err := parseWindow(qWindow)
		if err != nil {
			return err
		}
		end := float64(time.Now().Unix())
		start := end - secs
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		out, err := c.SearchSQL(ctx, qSQLType, qSQL, start, end, qSize)
		if err != nil {
			if _, ok := err.(*prom.AuthError); ok {
				die(err.Error())
			}
			return err
		}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	},
}

func printRows(rows []series.Row) error {
	raw, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

func parseWindow(w string) (float64, error) {
	if w == "" {
		return 3600, nil
	}
	var n float64
	var unit string
	if _, err := fmt.Sscanf(w, "%v%s", &n, &unit); err != nil || n <= 0 {
		return 0, fmt.Errorf("无法解析窗口 %q（例如 3h、90m、45m）", w)
	}
	switch unit {
	case "s":
		return n, nil
	case "m":
		return n * 60, nil
	case "h":
		return n * 3600, nil
	case "d":
		return n * 86400, nil
	}
	return 0, fmt.Errorf("无法解析窗口 %q（单位支持 s/m/h/d）", w)
}

func init() {
	queryPromCmd.Flags().StringVar(&qSource, "source", "thanos", "thanos 或 obs")
	queryPromCmd.Flags().StringVar(&qExpr, "expr", "", "PromQL 表达式")
	queryPromCmd.Flags().BoolVar(&qRange, "range", false, "窗口查询（默认即时）")
	queryPromCmd.Flags().StringVar(&qWindow, "window", "1h", "相对窗口")
	queryPromCmd.Flags().IntVar(&qStep, "step", 300, "range 步长（秒）")
	querySQLCmd.Flags().StringVar(&qSQL, "sql", "", "SQL（只读 SELECT）")
	querySQLCmd.Flags().StringVar(&qSQLType, "type", "logs", "logs 或 traces")
	querySQLCmd.Flags().StringVar(&qWindow, "window", "1h", "相对窗口")
	querySQLCmd.Flags().IntVar(&qSize, "size", 500, "返回行数上限")
	queryCmd.AddCommand(queryPromCmd, querySQLCmd)
}
