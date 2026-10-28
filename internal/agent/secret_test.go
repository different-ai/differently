package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLoadOrCreateSecretIsStable(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateSecret(dir)
	if err != nil || len(a) < 32 {
		t.Fatalf("secret %q %v", a, err)
	}
	b, err := LoadOrCreateSecret(dir)
	if err != nil || a != b {
		t.Fatalf("secret changed across restarts: %q then %q (%v)", a, b, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "agent-secret")); st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode %v", st.Mode().Perm())
	}
}

// A PID that no longer belongs to the VM's Firecracker process must never
// be signalled.
func TestLauncherOnlySignalsItsOwnProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process identity checks use /proc")
	}
	dir := t.TempDir()
	vm := &VM{Dir: dir}
	// "; :" keeps the shell alive so its command line still names the socket.
	mine := exec.Command("sh", "-c", "sleep 30; :", vm.APISocket())
	other := exec.Command("sleep", "30")
	for _, c := range []*exec.Cmd{mine, other} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	}
	l := ExecLauncher{}
	vm.PID = mine.Process.Pid
	if !l.Alive(vm) {
		t.Fatal("own process not recognised")
	}
	impostor := &VM{Dir: dir, PID: other.Process.Pid} // the VM's PID was reused
	if l.Alive(impostor) {
		t.Fatal("unrelated process treated as the VM")
	}
	if err := l.Kill(impostor); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if other.ProcessState != nil || other.Process.Signal(syscallZero()) != nil {
		t.Fatal("unrelated process was signalled")
	}
	if err := l.Kill(vm); err != nil {
		t.Fatal(err)
	}
	_ = mine.Wait()
	if mine.ProcessState == nil || mine.ProcessState.Success() {
		t.Fatal("own process was not killed")
	}
}
