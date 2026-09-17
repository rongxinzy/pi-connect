package signing_test

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestSidecarReleaseSignsBeforeChecksums(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/zhiyuan-sidecar-release.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	publish := workflow[strings.Index(workflow, "  publish:"):]
	if !strings.Contains(publish, "actions/checkout@v6") {
		t.Fatal("Publication must check out the repository so GitHub CLI can resolve its target")
	}
	for _, required := range []string{"needs: [build, sign-windows]", "environment: release", "setup-certum-signing", "tools/windows-signing/sign-runtime.ps1", "name: signed-windows-sidecar"} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("Missing signing boundary: %s", required)
		}
	}
	if strings.LastIndex(workflow, "name: signed-windows-sidecar") > strings.Index(workflow, "sha256sum cc-connect-sidecar-*") {
		t.Fatal("Checksums must be generated after replacing unsigned Windows bytes with the signed artifact")
	}
}

func TestAuthenticodePolicy(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell Authenticode policy tests run on Windows CI")
	}
	output, err := exec.Command("powershell.exe", "-NoProfile", "-File", "runtime-authenticode.test.ps1").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	t.Log(string(output))
}
