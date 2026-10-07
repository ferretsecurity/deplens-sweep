package sweep

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

func scannerIdentity(cfg ScanConfig) (string, error) {
	path, err := exec.LookPath(cfg.Executable)
	if err != nil {
		return "", errors.New("deplens executable is unavailable")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("cannot read deplens executable")
	}
	sum := sha256.Sum256(data)
	version := "sha256:" + hex.EncodeToString(sum[:])
	if info, err := buildinfo.ReadFile(path); err == nil && info.Main.Version != "" {
		version = info.Main.Version + " " + version
	}
	return version, nil
}

func gitEnv(c Connection, askpass string) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_TRACE") || name == "GIT_CURL_VERBOSE" || name == "GIT_CONFIG_PARAMETERS" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "LC_ALL=C", "GIT_ASKPASS="+askpass, "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1", "SWEEP_GIT_TOKEN="+c.token, "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/dev/null")
	return env
}

// Each external command owns a process group, so cancellation also stops its
// descendants. Clean up the group after Wait in case its leader exited early.
func processCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return stopProcessGroup(cmd) }
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd
}
func stopProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
func commandOutput(cmd *exec.Cmd) ([]byte, error) {
	output, err := cmd.Output()
	if cleanupErr := stopProcessGroup(cmd); err == nil {
		err = cleanupErr
	}
	return output, err
}
func contextFailure(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("repository clone-and-scan timeout exceeded")
	}
	if ctx.Err() != nil {
		return errors.New("repository attempt canceled")
	}
	return nil
}
func gitCommand(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := processCommand(ctx, "git", args...)
	cmd.Env = env
	setProcessOwner(cmd, ctx)
	out, err := commandOutput(cmd)
	if failure := contextFailure(ctx); failure != nil {
		return "", failure
	}
	if err != nil {
		var exit *exec.ExitError
		temporary := errors.As(err, &exit) && temporaryCloneError(exit.Stderr)
		return "", &gitOperationError{temporary: temporary}
	}
	return strings.TrimSpace(string(out)), nil
}
func acquire(ctx context.Context, c Connection, repo Repository, checkout, askpass string) error {
	if err := validateCloneURL(c, repo.CloneURL); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`).MatchString(repo.Revision) {
		return errors.New("invalid target revision")
	}
	env := gitEnv(c, askpass)
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		_, err = gitCommand(ctx, env, "clone", "--depth", "1", "--no-checkout", "--single-branch", "--no-recurse-submodules", "--branch", repo.DefaultBranch, "--", repo.CloneURL, checkout)
		if err == nil {
			break
		}
		var operation *gitOperationError
		if !errors.As(err, &operation) || !operation.temporary {
			return err
		}
		if attempt == maxAttempts {
			return errors.New("Git clone retries exhausted after 3 attempts (temporary network or server error)")
		}
		if cleanupErr := os.RemoveAll(checkout); cleanupErr != nil {
			return errors.New("cannot remove incomplete checkout before clone retry")
		}
		if waitErr := waitRetry(ctx, retryDelay(attempt)); waitErr != nil {
			return contextFailure(ctx)
		}
	}
	if err != nil {
		return err
	}
	// The default branch can advance after discovery. Fetch the resolved commit when
	// it was not included in the shallow clone, then verify the detached checkout.
	if _, err = gitCommand(ctx, env, "-C", checkout, "cat-file", "-e", repo.Revision+"^{commit}"); err != nil {
		if _, err = gitCommand(ctx, env, "-C", checkout, "fetch", "--depth", "1", "origin", repo.Revision); err != nil {
			return err
		}
	}
	if _, err = gitCommand(ctx, env, "-C", checkout, "checkout", "--detach", repo.Revision); err != nil {
		return err
	}
	actual, err := gitCommand(ctx, env, "-C", checkout, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if actual != repo.Revision {
		return errors.New("checkout revision differs from resolved commit")
	}
	return nil
}
func scanArguments(cfg ScanConfig, checkout string) []string {
	args := []string{"--json"}
	if cfg.Rules != "" {
		args = append(args, "--rules", cfg.Rules)
	}
	for _, p := range cfg.ExtendRules {
		args = append(args, "--extend-rules", p)
	}
	// A nonempty effective ignore list makes the scanner's inherited defaults explicit.
	ignore := strings.Join(cfg.Ignore, ",")
	if ignore == "" {
		ignore = ","
	}
	args = append(args, "--ignore", ignore)
	for _, p := range cfg.DisableRules {
		args = append(args, "--disable-rule", p)
	}
	for _, p := range cfg.DisableChecks {
		args = append(args, "--disable-check", p)
	}
	for _, p := range cfg.ExcludePresets {
		args = append(args, "--exclude-preset", p)
	}
	return append(args, checkout)
}
func analyze(ctx context.Context, cfg Config, checkout string) (any, error) {
	cmd := processCommand(ctx, cfg.Deplens.Executable, scanArguments(cfg.Deplens, checkout)...)
	hidden := map[string]bool{"SWEEP_GIT_TOKEN": true}
	for _, c := range cfg.Connections {
		hidden[c.TokenEnv] = true
	}
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		if !hidden[name] {
			cmd.Env = append(cmd.Env, e)
		}
	}
	setProcessOwner(cmd, ctx)
	raw, processErr := commandOutput(cmd)
	var output any
	decodeErr := json.Unmarshal(raw, &output)
	if decodeErr != nil {
		output = nil
	}
	if failure := contextFailure(ctx); failure != nil {
		return output, failure
	}
	if decodeErr != nil {
		return nil, errors.New("deplens produced missing or invalid JSON output")
	}
	if processErr != nil {
		return output, errors.New("deplens process failed")
	}
	if err := completeAnalysis(output); err != nil {
		return output, err
	}
	return output, nil
}
func completeAnalysis(output any) error {
	root, ok := output.(map[string]any)
	if !ok {
		return errors.New("deplens output is not a scan object")
	}
	if root["schema_version"] != float64(1) {
		return errors.New("unsupported or missing deplens output schema")
	}
	if _, ok := root["root"].(string); !ok {
		return errors.New("deplens output has no scan root")
	}
	for _, key := range []string{"sources", "check_runs", "findings"} {
		value, exists := root[key]
		if !exists {
			return fmt.Errorf("deplens output is missing %s", key)
		}
		if value != nil {
			if _, ok := value.([]any); !ok {
				return fmt.Errorf("deplens output has invalid %s", key)
			}
		}
	}
	sources, _ := root["sources"].([]any)
	for _, raw := range sources {
		source, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid dependency source")
		}
		analysis, ok := source["analysis"].(map[string]any)
		if !ok {
			return errors.New("missing source analysis")
		}
		switch analysis["presence"] {
		case "present", "absent", "unknown":
		default:
			return errors.New("invalid or missing dependency presence")
		}
		switch analysis["extraction"] {
		case "complete", "unsupported":
		case "partial", "failed":
			return errors.New("deplens analysis is incomplete")
		default:
			return errors.New("invalid source extraction state")
		}
		diagnostics, _ := source["diagnostics"].([]any)
		for _, raw := range diagnostics {
			if diagnostic, ok := raw.(map[string]any); ok && diagnostic["severity"] == "error" {
				return errors.New("deplens reported an analysis error")
			}
		}
	}
	runs, _ := root["check_runs"].([]any)
	for _, raw := range runs {
		run, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid check run")
		}
		switch run["status"] {
		case "completed", "skipped":
		case "failed":
			return errors.New("deplens check execution failed")
		default:
			return errors.New("invalid check run status")
		}
	}
	return nil
}
func writeAskpass(dir string) (string, error) {
	path := filepath.Join(dir, "askpass")
	// This file contains no token. Git receives credentials from its child environment.
	const script = "#!/bin/sh\ncase \"$1\" in\n*Username*) printf '%s\\n' oauth2 ;;\n*Password*) printf '%s\\n' \"$SWEEP_GIT_TOKEN\" ;;\n*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		return "", err
	}
	return path, nil
}
