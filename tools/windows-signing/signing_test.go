package signing_test

import (
	"os"
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
	for _, required := range []string{"environment: release", "repository: rongxinzy/RongxinAI", "name: signed-sidecar", "verify-return.mjs", "--verify-tag", "name: unsigned-sidecar-binaries"} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("Missing signing boundary: %s", required)
		}
	}
	if strings.Contains(workflow, "CERTUM_") || strings.Contains(workflow, "setup-certum-signing") {
		t.Fatal("Signing credentials must remain exclusively in RongxinAI")
	}
	if strings.Index(publish, "Copy-Item -LiteralPath signed-runtime/cc-connect-sidecar-windows-amd64.exe") > strings.Index(publish, "sha256sum cc-connect-sidecar-*") {
		t.Fatal("Checksums must be generated after replacing unsigned Windows bytes with the signed artifact")
	}
	for _, removed := range []string{"Assert-WindowsRuntimeSignature", "RUNTIME_SIGNER_THUMBPRINT", "runtime-authenticode.ps1"} {
		if strings.Contains(workflow, removed) {
			t.Fatalf("Runtime publication must not require signature verification: %s", removed)
		}
	}
}
