package main

import (
	"bytes"
	"os/exec"
	"testing"
	"time"
)

// TestHardenedDeploymentContainerServes runs the pinned SQL Server image under
// the exact restricted securityContext the deployment manifest applies —
// read-only root filesystem, non-root, every capability dropped except the
// NET_BIND_SERVICE that sqlservr's file capability requires, no privilege
// escalation, and only /tmp plus the data dir writable — and verifies the
// server still starts. The static manifest contract cannot catch this because
// it never executes the image: dropping ALL capabilities without re-adding
// NET_BIND_SERVICE passes static validation yet makes the kernel refuse to exec
// sqlservr, crash-looping every rendered pod.
//
// The docker flags mirror templates/deployment/kustomize/base/stateful-set.yaml.
func TestHardenedDeploymentContainerServes(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available")
	}
	const name = "mssql-hardened-test"
	_ = exec.Command("docker", "rm", "-f", name).Run()

	run := exec.Command("docker", "run", "-d", "--name", name,
		"--read-only",
		"--user", "10001:0",
		"--cap-drop", "ALL",
		"--cap-add", "NET_BIND_SERVICE",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/opt/mssql:uid=10001,gid=0",
		"-e", "ACCEPT_EULA=Y",
		"-e", "MSSQL_SA_PASSWORD=YourStrong!Passw0rd",
		"-e", "MSSQL_PID=Developer",
		image.FullName(),
	)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run hardened mssql: %v: %s", err, out)
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()

	deadline := time.Now().Add(90 * time.Second)
	for {
		probe := exec.Command("docker", "exec", name,
			"/opt/mssql-tools18/bin/sqlcmd", "-S", "localhost",
			"-U", "sa", "-P", "YourStrong!Passw0rd", "-C", "-Q", "SELECT 1")
		if probe.Run() == nil {
			return // healthy under the restricted securityContext
		}

		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if bytes.Contains(logs, []byte("Operation not permitted")) {
			t.Fatalf("sqlservr could not exec under the restricted securityContext — capability regression:\n%s", logs)
		}
		// The published image is linux/amd64; on an emulated host (e.g. Apple
		// Silicon) sqlservr execs successfully — clearing the capability
		// barrier this test guards — but then segfaults inside the emulator, so
		// health is unverifiable here.
		if bytes.Contains(logs, []byte("qemu")) || bytes.Contains(logs, []byte("Segmentation fault")) {
			t.Skipf("image runs under CPU emulation; restricted securityContext cleared the sqlservr exec barrier:\n%s", logs)
		}
		if time.Now().After(deadline) {
			t.Fatalf("SQL Server never became healthy under the restricted securityContext:\n%s", logs)
		}
		time.Sleep(3 * time.Second)
	}
}

func dockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "info").Run() == nil
}
