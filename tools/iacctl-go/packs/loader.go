package packs

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Resources is the parsed packs/iac-go/resources.yaml.
type Resources struct {
	Env      string `yaml:"env"`
	Selector struct {
		Thanos string `yaml:"thanos"`
		Obs    string `yaml:"obs"`
	} `yaml:"selector"`
	Resources struct {
		ClusterID      string `yaml:"cluster_id"`
		IacApp         string `yaml:"iac_app"`
		Namespace      string `yaml:"namespace"`
		Service        string `yaml:"service"`
		MysqlService   string `yaml:"mysql_service"`
		LogStream      string `yaml:"log_stream"`
		TraceStream    string `yaml:"trace_stream"`
		ExpectReplicas int    `yaml:"expect_replicas"`
	} `yaml:"resources"`
}

// LoadIacGo parses the embedded iac-go resources.
func LoadIacGo() (Resources, error) {
	var r Resources
	raw, err := FS.ReadFile("iac-go/resources.yaml")
	if err != nil {
		return r, fmt.Errorf("读取 iac-go resources: %w", err)
	}
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("解析 iac-go resources: %w", err)
	}
	if r.Selector.Thanos == "" || r.Selector.Obs == "" || r.Resources.Service == "" {
		return r, fmt.Errorf("iac-go resources 不完整：selector/resources 必填")
	}
	return r, nil
}
