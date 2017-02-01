package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/driver/env"
	"github.com/hashicorp/nomad/command/agent/consul"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"

	ctestutils "github.com/hashicorp/nomad/client/testutil"
)

func TestExecDriver_Fingerprint(t *testing.T) {
	ctestutils.ExecCompatible(t)
	task := &structs.Task{
		Name:      "foo",
		Driver:    "exec",
		Resources: structs.DefaultResources(),
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewExecDriver(ctx.DriverCtx)
	node := &structs.Node{
		Attributes: map[string]string{
			"unique.cgroup.mountpoint": "/sys/fs/cgroup",
		},
	}
	apply, err := d.Fingerprint(&config.Config{}, node)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !apply {
		t.Fatalf("should apply")
	}
	if node.Attributes["driver.exec"] == "" {
		t.Fatalf("missing driver")
	}
}

func TestExecDriver_StartOpen_Wait(t *testing.T) {
	ctestutils.ExecCompatible(t)
	task := &structs.Task{
		Name:   "sleep",
		Driver: "exec",
		Config: map[string]interface{}{
			"command": "/bin/sleep",
			"args":    []string{"5"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: basicResources,
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewExecDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("prestart err: %v", err)
	}
	handle, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle == nil {
		t.Fatalf("missing handle")
	}

	// Attempt to open
	handle2, err := d.Open(ctx.ExecCtx, handle.ID())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle2 == nil {
		t.Fatalf("missing handle")
	}

	handle.Kill()
	handle2.Kill()
}

func TestExecDriver_KillUserPid_OnPluginReconnectFailure(t *testing.T) {
	ctestutils.ExecCompatible(t)
	task := &structs.Task{
		Name:   "sleep",
		Driver: "exec",
		Config: map[string]interface{}{
			"command": "/bin/sleep",
			"args":    []string{"1000000"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: basicResources,
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewExecDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("prestart err: %v", err)
	}
	handle, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle == nil {
		t.Fatalf("missing handle")
	}
	defer handle.Kill()

	id := &execId{}
	if err := json.Unmarshal([]byte(handle.ID()), id); err != nil {
		t.Fatalf("Failed to parse handle '%s': %v", handle.ID(), err)
	}
	pluginPid := id.PluginConfig.Pid
	proc, err := os.FindProcess(pluginPid)
	if err != nil {
		t.Fatalf("can't find plugin pid: %v", pluginPid)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("can't kill plugin pid: %v", err)
	}

	// Attempt to open
	handle2, err := d.Open(ctx.ExecCtx, handle.ID())
	if err == nil {
		t.Fatalf("expected error")
	}
	if handle2 != nil {
		handle2.Kill()
		t.Fatalf("expected handle2 to be nil")
	}

	// Test if the userpid is still present
	userProc, _ := os.FindProcess(id.UserPid)

	for retry := 3; retry > 0; retry-- {
		if err = userProc.Signal(syscall.Signal(0)); err != nil {
			// Process is gone as expected; exit
			return
		}

		// Killing processes is async; wait and check again
		time.Sleep(time.Second)
	}
	if err = userProc.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("expected user process to die")
	}
}

func TestExecDriver_Start_Wait(t *testing.T) {
	ctestutils.ExecCompatible(t)
	task := &structs.Task{
		Name:   "sleep",
		Driver: "exec",
		Config: map[string]interface{}{
			"command": "/bin/sleep",
			"args":    []string{"2"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: basicResources,
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewExecDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("prestart err: %v", err)
	}
	handle, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle == nil {
		t.Fatalf("missing handle")
	}

	// Update should be a no-op
	err = handle.Update(task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Task should terminate quickly
	select {
	case res := <-handle.WaitCh():
		if !res.Successful() {
			t.Fatalf("err: %v", res)
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*5) * time.Second):
		t.Fatalf("timeout")
	}
}

func TestExecDriver_Start_Wait_AllocDir(t *testing.T) {
	ctestutils.ExecCompatible(t)

	exp := []byte{'w', 'i', 'n'}
	file := "output.txt"
	task := &structs.Task{
		Name:   "sleep",
		Driver: "exec",
		Config: map[string]interface{}{
			"command": "/bin/bash",
			"args": []string{
				"-c",
				fmt.Sprintf(`sleep 1; echo -n %s > ${%s}/%s`, string(exp), env.AllocDir, file),
			},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: basicResources,
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewExecDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("prestart err: %v", err)
	}
	handle, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle == nil {
		t.Fatalf("missing handle")
	}

	// Task should terminate quickly
	select {
	case res := <-handle.WaitCh():
		if !res.Successful() {
			t.Fatalf("err: %v", res)
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*5) * time.Second):
		t.Fatalf("timeout")
	}

	// Check that data was written to the shared alloc directory.
	outputFile := filepath.Join(ctx.AllocDir.SharedDir, file)
	act, err := ioutil.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("Couldn't read expected output: %v", err)
	}

	if !reflect.DeepEqual(act, exp) {
		t.Fatalf("Command outputted %v; want %v", act, exp)
	}
}

func TestExecDriver_Start_Kill_Wait(t *testing.T) {
	ctestutils.ExecCompatible(t)
	task := &structs.Task{
		Name:   "sleep",
		Driver: "exec",
		Config: map[string]interface{}{
			"command": "/bin/sleep",
			"args":    []string{"100"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources:   basicResources,
		KillTimeout: 10 * time.Second,
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewExecDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("prestart err: %v", err)
	}
	handle, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle == nil {
		t.Fatalf("missing handle")
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		err := handle.Kill()
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	// Task should terminate quickly
	select {
	case res := <-handle.WaitCh():
		if res.Successful() {
			t.Fatal("should err")
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*10) * time.Second):
		t.Fatalf("timeout")
	}
}

func TestExecDriver_Signal(t *testing.T) {
	ctestutils.ExecCompatible(t)
	task := &structs.Task{
		Name:   "signal",
		Driver: "exec",
		Config: map[string]interface{}{
			"command": "/bin/bash",
			"args":    []string{"test.sh"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources:   basicResources,
		KillTimeout: 10 * time.Second,
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewExecDriver(ctx.DriverCtx)

	testFile := filepath.Join(ctx.ExecCtx.TaskDir.Dir, "test.sh")
	testData := []byte(`
at_term() {
    echo 'Terminated.'
    exit 3
}
trap at_term USR1
while true; do
    sleep 1
done
	`)
	if err := ioutil.WriteFile(testFile, testData, 0777); err != nil {
		t.Fatalf("Failed to write data: %v", err)
	}

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("prestart err: %v", err)
	}
	handle, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle == nil {
		t.Fatalf("missing handle")
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		err := handle.Signal(syscall.SIGUSR1)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	// Task should terminate quickly
	select {
	case res := <-handle.WaitCh():
		if res.Successful() {
			t.Fatal("should err")
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*6) * time.Second):
		t.Fatalf("timeout")
	}

	// Check the log file to see it exited because of the signal
	outputFile := filepath.Join(ctx.ExecCtx.TaskDir.LogDir, "signal.stdout.0")
	act, err := ioutil.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("Couldn't read expected output: %v", err)
	}

	exp := "Terminated."
	if strings.TrimSpace(string(act)) != exp {
		t.Logf("Read from %v", outputFile)
		t.Fatalf("Command outputted %v; want %v", act, exp)
	}
}

func TestExecDriverUser(t *testing.T) {
	ctestutils.ExecCompatible(t)
	task := &structs.Task{
		Name:   "sleep",
		Driver: "exec",
		User:   "alice",
		Config: map[string]interface{}{
			"command": "/bin/sleep",
			"args":    []string{"100"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources:   basicResources,
		KillTimeout: 10 * time.Second,
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewExecDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("prestart err: %v", err)
	}
	handle, err := d.Start(ctx.ExecCtx, task)
	if err == nil {
		handle.Kill()
		t.Fatalf("Should've failed")
	}
	msg := "user alice"
	if !strings.Contains(err.Error(), msg) {
		t.Fatalf("Expecting '%v' in '%v'", msg, err)
	}
}

// TestExecDriver_HandlerExec ensures the exec driver's handle properly executes commands inside the chroot.
func TestExecDriver_HandlerExec(t *testing.T) {
	ctestutils.ExecCompatible(t)
	task := &structs.Task{
		Name:   "sleep",
		Driver: "exec",
		Config: map[string]interface{}{
			"command": "/bin/sleep",
			"args":    []string{"9000"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: basicResources,
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewExecDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("prestart err: %v", err)
	}
	handle, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle == nil {
		t.Fatalf("missing handle")
	}

	// Exec a command that should work
	out, code, err := handle.(consul.ScriptExecutor).Exec(context.TODO(), "/usr/bin/stat", []string{"/alloc"})
	if err != nil {
		t.Fatalf("error exec'ing stat: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected `stat /alloc` to succeed but exit code was: %d", code)
	}
	if expected := 100; len(out) < expected {
		t.Fatalf("expected at least %d bytes of output but found %d:\n%s", expected, len(out), out)
	}

	// Exec a command that should fail
	out, code, err = handle.(consul.ScriptExecutor).Exec(context.TODO(), "/usr/bin/stat", []string{"lkjhdsaflkjshowaisxmcvnlia"})
	if err != nil {
		t.Fatalf("error exec'ing stat: %v", err)
	}
	if code == 0 {
		t.Fatalf("expected `stat` to fail but exit code was: %d", code)
	}
	if expected := "No such file or directory"; !bytes.Contains(out, []byte(expected)) {
		t.Fatalf("expected output to contain %q but found: %q", expected, out)
	}

	if err := handle.Kill(); err != nil {
		t.Fatalf("error killing exec handle: %v", err)
	}
}
