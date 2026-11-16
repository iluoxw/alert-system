package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

type toolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Args        string `json:"args"`
	Source      string `json:"source"`
}

var toolsCmd = &cobra.Command{
	Use:   "tools",
	Short: "Agent 工具面：命令清单与输入约定（JSON）",
	RunE: func(cmd *cobra.Command, args []string) error {
		tools := []toolSpec{
			{"inspect", "PRO 巡检垂直切片：L0 样本年龄 / L0 stats 连续性 / L1 流量与 5xx / L3 mysql；总体取最严重项", "--window 1h --format json", "thanos+obs"},
			{"query prom", "任意只读 PromQL；--source thanos 或 obs；默认即时，--range 走窗口", "--source thanos --expr '...' [--range --window 1h --step 300]", "thanos|obs"},
			{"query sql", "OpenObserve 日志/trace SQL 检索（只读 SELECT）", "--sql 'SELECT ...' --window 1h [--type logs]", "obs"},
			{"sources", "已配置后端与凭据状态；--probe 做连通性检查", "[--probe]", "none"},
			{"skills", "嵌入 SKILL 的 list/read/sync（同步到 ~/.agents/skills）", "list | read [--file …] | sync [--check]", "none"},
			{"tools", "本清单", "", "none"},
		}
		out, err := json.MarshalIndent(tools, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	},
}
