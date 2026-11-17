package iacbroker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Run the path traversal in a killable subprocess so a regression cannot hang the test runner.
func TestCCloudPathFormsAreBounded(t *testing.T) {
	if form := os.Getenv("IAC_CCLOUD_PATH_TEST_CHILD"); form != "" {
		b := ccloudFixture(t, "printf '%s' '"+validPods+"'")
		options := b.ccloud.options
		canonical, e := filepath.EvalSymlinks(options.PackageDir)
		if e != nil {
			t.Fatal(e)
		}
		options.PackageDir = canonical
		switch form {
		case "trailing":
			options.PackageDir += "/"
		case "dot":
			options.PackageDir += "/."
		case "dot-segment":
			options.PackageDir += "/dist/.."
		}
		b.ccloud = newCCloud(options, b.ccloud.manifestHash)
		ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
		defer cancel()
		if form == "cancelled" {
			cancel()
		}
		item, e := b.Query(ctx, "ccloud_l4")
		if e != nil {
			t.Fatal(e)
		}
		if form == "cancelled" {
			if item.Status != "unknown" {
				t.Fatal("cancelled query succeeded")
			}
			return
		}
		if item.Status != "ok" {
			t.Fatal("equivalent absolute package path failed", item.ReasonCode)
		}
		if b.ccloud.options.PackageDir != filepath.Clean(options.PackageDir) {
			t.Fatal("constructor did not normalize package root")
		}
		return
	}
	for _, form := range []string{"trailing", "dot", "dot-segment", "cancelled"} {
		t.Run(form, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCCloudPathFormsAreBounded$")
			cmd.Env = append(os.Environ(), "IAC_CCLOUD_PATH_TEST_CHILD="+form)
			output, e := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("package traversal ignored its deadline; child killed")
			}
			if e != nil {
				t.Fatalf("isolated path fixture failed: %v %s", e, output)
			}
		})
	}
}
