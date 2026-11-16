package cmd

import (
	"encoding/json"

	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/luoxw/iacctl-go/skills"
)

var (
	skillName  string
	skillFile  string
	skillCheck bool
)

var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "嵌入 SKILL 的列表 / 读取 / 同步（镜像 Node 版 iacctl 的机制）",
}

var skillsListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出嵌入的 skill 与文件（JSON）",
	RunE: func(cmd *cobra.Command, args []string) error {
		type entry struct {
			Name  string   `json:"name"`
			Files []string `json:"files"`
			Bytes int      `json:"bytes"`
		}
		var out []entry
		for _, name := range skills.ManagedNames {
			e := entry{Name: name}
			root := skills.Root
			fs.WalkDir(skills.FS, root, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				e.Files = append(e.Files, p[len(root)+1:])
				if b, _ := fs.ReadFile(skills.FS, p); b != nil {
					e.Bytes += len(b)
				}
				return nil
			})
			out = append(out, e)
		}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	},
}

var skillsReadCmd = &cobra.Command{
	Use:   "read",
	Short: "打印嵌入文档（--file 相对 skill 根，默认 SKILL.md）",
	RunE: func(cmd *cobra.Command, args []string) error {
		name := skillName
		if name == "" {
			name = skills.ManagedNames[0]
		}
		if name != skills.Root {
			return fmt.Errorf("--skill 只接受 %s", skills.Root)
		}
		file := skillFile
		if file == "" {
			file = "SKILL.md"
		}
		raw, err := fs.ReadFile(skills.FS, skills.Root+"/"+file)
		if err != nil {
			return fmt.Errorf("读取 %s: %w", file, err)
		}
		fmt.Print(string(raw))
		return nil
	},
}

var skillsSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "把嵌入文档写出到 ~/.agents/skills；--check 只比对不写",
	RunE: func(cmd *cobra.Command, args []string) error {
		base, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		target := filepath.Join(base, ".agents", "skills", skills.Root)
		var drifted []string
		err = fs.WalkDir(skills.FS, skills.Root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel := p[len(skills.Root)+1:]
			want, _ := fs.ReadFile(skills.FS, p)
			dst := filepath.Join(target, rel)
			got, rerr := os.ReadFile(dst)
			if rerr != nil || string(got) != string(want) {
				drifted = append(drifted, rel)
				if !skillCheck {
					if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
						return err
					}
					if err := os.WriteFile(dst, want, 0o644); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if skillCheck {
			if len(drifted) > 0 {
				return fmt.Errorf("外挂副本与二进制不一致（%d 个文件）：%v；运行 iacctl-go skills sync 修复", len(drifted), drifted)
			}
			fmt.Println("外挂副本与当前二进制一致")
			return nil
		}
		if len(drifted) > 0 {
			fmt.Printf("已同步 %d 个文件到 %s：%v\n", len(drifted), target, drifted)
		} else {
			fmt.Println("外挂副本已是最新：" + target)
		}
		return nil
	},
}

func init() {
	skillsReadCmd.Flags().StringVar(&skillName, "skill", "", "skill 名（默认 iac-go-ops）")
	skillsReadCmd.Flags().StringVar(&skillFile, "file", "", "文件（默认 SKILL.md）")
	skillsSyncCmd.Flags().BoolVar(&skillCheck, "check", false, "只比对不写")
	skillsCmd.AddCommand(skillsListCmd, skillsReadCmd, skillsSyncCmd)
}
