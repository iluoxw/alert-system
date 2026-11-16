//go:build !unix

package agentclient

import (
	"errors"
	"os/exec"
)

func prepareGroup(*exec.Cmd) error { return errors.New("handler process groups require a Unix client") }
func killGroup(*exec.Cmd) error    { return nil }

const supportsGroups = false
