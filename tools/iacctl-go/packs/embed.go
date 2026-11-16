package packs

import "embed"

// FS holds the ServicePack YAML assets under packs/iac-go/.
//
//go:embed iac-go/pack.yaml iac-go/resources.yaml
var FS embed.FS
