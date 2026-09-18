package signing_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	generator := "node tools/windows-signing/prepare-sidecar-release.mjs"
	if !strings.Contains(publish, generator) || strings.Index(publish, "Copy-Item -LiteralPath signed-runtime/cc-connect-sidecar-windows-amd64.exe") > strings.Index(publish, generator) {
		t.Fatal("Checksums must be generated after replacing unsigned Windows bytes with the signed artifact")
	}
	for _, removed := range []string{"Assert-WindowsRuntimeSignature", "RUNTIME_SIGNER_THUMBPRINT", "runtime-authenticode.ps1"} {
		if strings.Contains(workflow, removed) {
			t.Fatalf("Runtime publication must not require signature verification: %s", removed)
		}
	}
}

func TestSidecarReleaseChecksumsDoNotDependOnWindowsSha256sumFormat(t *testing.T) {
	directory := t.TempDir()
	filenames := []string{"cc-connect-sidecar-windows-amd64.exe", "cc-connect-sidecar-darwin-amd64", "cc-connect-sidecar-darwin-arm64", "cc-connect-sidecar-linux-amd64"}
	expected := map[string]string{}
	var checksums strings.Builder
	for _, filename := range filenames {
		bytes := []byte("MZ\x00\r\n" + filename)
		if err := os.WriteFile(filepath.Join(directory, filename), bytes, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(bytes)
		expected[filename] = hex.EncodeToString(hash[:])
		checksums.WriteString(expected[filename] + "  " + filename + "\n")
	}
	revision := strings.Repeat("a", 40)
	command := func(tag, sha string) *exec.Cmd {
		return exec.Command("node", "prepare-sidecar-release.mjs", directory, tag, sha)
	}
	if output, err := command("zhiyuan-sidecar-v4", revision).CombinedOutput(); err != nil {
		t.Fatalf("Generator failed: %v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(directory, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != checksums.String() {
		t.Fatalf("Noncanonical checksums: %s", data)
	}
	data, err = os.ReadFile(filepath.Join(directory, "runtime-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion                         int
		SourceRepository, Tag, SourceRevision string
		Sha256                                map[string]string
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || manifest.SourceRepository != "https://github.com/rongxinzy/pi-connect" || manifest.Tag != "zhiyuan-sidecar-v4" || manifest.SourceRevision != revision || len(manifest.Sha256) != len(expected) {
		t.Fatalf("Invalid manifest: %+v", manifest)
	}
	for filename, hash := range expected {
		if manifest.Sha256[filename] != hash {
			t.Fatalf("Wrong hash for %s", filename)
		}
	}
	for _, args := range [][2]string{{"not-a-tag", revision}, {"zhiyuan-sidecar-v4", "invalid"}} {
		if err := command(args[0], args[1]).Run(); err == nil {
			t.Fatal("Invalid provenance accepted")
		}
	}
	if err := os.Remove(filepath.Join(directory, filenames[0])); err != nil {
		t.Fatal(err)
	}
	if err := command("zhiyuan-sidecar-v4", revision).Run(); err == nil {
		t.Fatal("Missing binary accepted")
	}
}
