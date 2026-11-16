// Package skills embeds the Agent-facing skill docs that ship in lockstep
// with the iacctl-go binary (mirrors the Node version's skills embedding).
package skills

import "embed"

//go:embed iac-go-ops/SKILL.md iac-go-ops/references
var FS embed.FS

// ManagedNames is the ordered set of skills this binary owns and syncs.
var ManagedNames = []string{"iac-go-ops"}

// Root is the embed root inside FS.
const Root = "iac-go-ops"
