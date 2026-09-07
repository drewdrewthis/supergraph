package features

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

// registerSubscribeSteps wires features/subscribe.feature. It reuses the generic
// `world` (same binPath/cfgPath/listen the other suites drive) plus a background
// subprocess for the --once scenario, which must stay blocked on the websocket
// push while a separate step POSTs the triggering hook event.
func registerSubscribeSteps(sc *godog.ScenarioContext, w *world) {
	sc.Step(lit("I run `supergraph subscribe bogusField`"), func() error {
		w.runCLI("subscribe", "bogusField")
		return nil
	})
	sc.Step(lit("exit code is 1"), w.assertExitCode1)
	sc.Step(lit("stdout is empty"), w.assertSubscribeStdoutEmpty)

	sc.Step(lit("I run `supergraph subscribe claudeSessionUpdated --once` against it in the background"), w.startSubscribeBG)
	sc.Step(lit("the subscribe process exits 0 within 5s"), w.waitSubscribeExit0)
	sc.Step(lit("its stdout is exactly one compact JSON line naming type `claude.session.updated`"), func() error {
		return w.assertSubscribeOneLineOfType("claude.session.updated")
	})

	sc.Step(lit("I run `supergraph subscribe tmuxEvents --once --endpoint http://127.0.0.1:1/graphql`"), func() error {
		w.runCLI("subscribe", "tmuxEvents", "--once", "--endpoint", "http://127.0.0.1:1/graphql")
		return nil
	})
	sc.Step(lit("exit code is 2"), w.assertExitCode2)
	sc.Step(lit("stderr is not empty"), w.assertSubscribeStderrNotEmpty)
}

func (w *world) assertSubscribeStdoutEmpty() error {
	if w.lastStdout != "" {
		return fmt.Errorf("stdout = %q, want empty", w.lastStdout)
	}
	return nil
}

func (w *world) assertSubscribeStderrNotEmpty() error {
	if w.lastStderr == "" {
		return errors.New("stderr is empty, want an error")
	}
	return nil
}

func (w *world) assertExitCode1() error {
	if w.lastExit != 1 {
		return fmt.Errorf("exit=%d, want 1 (stderr=%q)", w.lastExit, w.lastStderr)
	}
	return nil
}

func (w *world) assertExitCode2() error {
	if w.lastExit != 2 {
		return fmt.Errorf("exit=%d, want 2 (stderr=%q)", w.lastExit, w.lastStderr)
	}
	return nil
}

// startSubscribeBG starts `supergraph subscribe claudeSessionUpdated --once` as a
// background subprocess against the already-running w.serve, so a later step can
// POST the triggering hook event while this process blocks on the first push.
func (w *world) startSubscribeBG() error {
	full := []string{
		"--config", w.cfgPath,
		"subscribe", "claudeSessionUpdated", "--once",
		"--endpoint", "http://" + w.listen + "/graphql",
		// --ready-notify makes the client print "subscribed" to stderr once the
		// subscription is registered server-side; we block on that below so the
		// next step's hook POST can't race the handshake and be missed (the bus is
		// live pub-sub with no replay).
		"--ready-notify",
	}
	cmd := exec.Command(binPath, full...)
	w.subStdout = &safeBuf{}
	w.subStderr = &safeBuf{}
	cmd.Stdout = w.subStdout
	cmd.Stderr = w.subStderr
	if err := cmd.Start(); err != nil {
		return err
	}
	w.subCmd = cmd
	w.subDone = make(chan struct{})
	go func() {
		w.subWaitErr = cmd.Wait()
		close(w.subDone)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(w.subStderr.String(), "subscribed") {
			return nil
		}
		select {
		case <-w.subDone:
			return fmt.Errorf("subscribe process exited before subscribing; stderr=%q", w.subStderr.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("subscribe process never reported subscribed within 5s; stderr=%q", w.subStderr.String())
}

func (w *world) waitSubscribeExit0() error {
	if w.subCmd == nil {
		return errors.New("no subscribe process was started")
	}
	select {
	case <-w.subDone:
	case <-time.After(5 * time.Second):
		_ = w.subCmd.Process.Kill()
		<-w.subDone
		return fmt.Errorf("subscribe process did not exit within 5s; stdout=%q stderr=%q",
			w.subStdout.String(), w.subStderr.String())
	}
	code := 0
	var ee *exec.ExitError
	if w.subWaitErr != nil {
		if errors.As(w.subWaitErr, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	if code != 0 {
		return fmt.Errorf("subscribe process exited %d; stderr=%q", code, w.subStderr.String())
	}
	return nil
}

func (w *world) assertSubscribeOneLineOfType(wantType string) error {
	out := strings.TrimRight(w.subStdout.String(), "\n")
	lines := strings.Split(out, "\n")
	if len(lines) != 1 || lines[0] == "" {
		return fmt.Errorf("subscribe stdout = %q, want exactly one line", w.subStdout.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		return fmt.Errorf("subscribe stdout line is not valid JSON: %w (%q)", err, lines[0])
	}
	if got["type"] != wantType {
		return fmt.Errorf(`decoded "type" = %v, want %q (line: %q)`, got["type"], wantType, lines[0])
	}
	return nil
}

// stopSubscribeBG kills a still-running background subscribe subprocess, called
// from world.cleanup() so a scenario that fails before the process exits never
// leaks it.
func (w *world) stopSubscribeBG() {
	if w.subCmd == nil {
		return
	}
	select {
	case <-w.subDone:
	default:
		_ = w.subCmd.Process.Kill()
		<-w.subDone
	}
	w.subCmd = nil
}
