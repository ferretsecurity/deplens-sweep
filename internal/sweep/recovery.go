package sweep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type workOwner struct {
	Format  string `json:"format"`
	Results string `json:"results"`
	Run     string `json:"run"`
	PID     int    `json:"pid"`
	Started string `json:"process_started"`
	Token   string `json:"token"`
}
type workOwnerKey struct{}

func prepareWorkspace(cfg Config, batchID string) (string, workOwner, error) {
	base := cfg.TempDir
	if base == "" {
		base = filepath.Join(cfg.ResultsDir, ".work")
	}
	sum := sha256.Sum256([]byte(cfg.ResultsDir))
	scope := filepath.Join(base, ".deplens-sweep-"+hex.EncodeToString(sum[:16]))
	if err := os.MkdirAll(scope, 0700); err != nil {
		return "", workOwner{}, errors.New("cannot create checkout namespace")
	}
	info, err := os.Lstat(scope)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", workOwner{}, errors.New("checkout namespace is not a directory")
	}
	if err := recoverWork(scope, cfg.ResultsDir); err != nil {
		return "", workOwner{}, err
	}
	started, _, err := processIdentity(os.Getpid())
	if err != nil {
		return "", workOwner{}, errors.New("cannot identify workspace owner")
	}
	owner := workOwner{Format: "deplens-sweep-work-v1", Results: cfg.ResultsDir, Run: "run-" + batchID, PID: os.Getpid(), Started: started, Token: newID()}
	work := filepath.Join(scope, owner.Run)
	if err := os.Mkdir(work, 0700); err != nil {
		return "", workOwner{}, errors.New("cannot create temporary workspace")
	}
	if err := saveJSON(filepath.Join(work, ".owner.json"), owner); err != nil {
		os.RemoveAll(work)
		return "", workOwner{}, errors.New("cannot save workspace ownership")
	}
	return work, owner, nil
}

func removeWorkspace(work string) {
	os.RemoveAll(work)
	// Remove an empty namespace after normal completion. Unknown entries keep it.
	os.Remove(filepath.Dir(work))
}

func prepareStaging(cfg Config, owner workOwner) (string, error) {
	scope := filepath.Join(cfg.ResultsDir, ".staging")
	if err := os.MkdirAll(scope, 0700); err != nil {
		return "", errors.New("cannot create result staging directory")
	}
	info, err := os.Lstat(scope)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("result staging location is not a directory")
	}
	if err := recoverWork(scope, cfg.ResultsDir); err != nil {
		return "", err
	}
	stage := filepath.Join(scope, owner.Run)
	if err := os.Mkdir(stage, 0700); err != nil {
		return "", errors.New("cannot create result staging workspace")
	}
	if err := saveJSON(filepath.Join(stage, ".owner.json"), owner); err != nil {
		os.RemoveAll(stage)
		return "", errors.New("cannot save staging ownership")
	}
	return stage, nil
}

func recoverWork(scope, results string) error {
	entries, err := os.ReadDir(scope)
	if err != nil {
		return errors.New("cannot inspect abandoned work")
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		var owner workOwner
		if !readSavedJSON(scope, filepath.Join(entry.Name(), ".owner.json"), &owner) || owner.Format != "deplens-sweep-work-v1" || owner.Results != results || owner.Run != entry.Name() || owner.PID <= 0 || owner.Started == "" || owner.Token == "" {
			continue
		}
		started, _, err := processIdentity(owner.PID)
		if err == nil && started == owner.Started {
			return errors.New("temporary workspace still has an active owner")
		}
		if err != nil && !os.IsNotExist(err) {
			return errors.New("cannot verify abandoned workspace owner")
		}
		if err := stopOwnedProcesses(owner.Token); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(scope, entry.Name())); err != nil {
			return errors.New("cannot remove abandoned checkout")
		}
	}
	return nil
}

func processIdentity(pid int) (string, string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", "", err
	}
	// comm may contain spaces or parentheses; fields after its last ')' are fixed.
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return "", "", errors.New("invalid process identity")
	}
	fields := strings.Fields(string(raw)[end+1:])
	if len(fields) < 20 {
		return "", "", errors.New("invalid process identity")
	}
	return fields[19], fields[0], nil
}

func stopOwnedProcesses(token string) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return errors.New("cannot inspect abandoned processes")
		}
		active := false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid == os.Getpid() {
				continue
			}
			started, state, err := processIdentity(pid)
			if err != nil || state == "Z" {
				continue
			}
			environ, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
			if err != nil || !strings.Contains("\x00"+string(environ), "\x00SWEEP_WORK_OWNER="+token+"\x00") {
				continue
			}
			// Pin the process before rechecking its creation identity. Signaling a
			// pidfd cannot hit a replacement process if the numeric PID is recycled.
			fd, err := unix.PidfdOpen(pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				return errors.New("cannot safely stop abandoned command; checkout preserved")
			}
			current, state, err := processIdentity(pid)
			if err != nil || current != started || state == "Z" {
				unix.Close(fd)
				continue
			}
			active = true
			signalErr := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			unix.Close(fd)
			if signalErr != nil && !errors.Is(signalErr, syscall.ESRCH) {
				return errors.New("cannot stop abandoned command")
			}
		}
		if !active {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("abandoned command remains active; checkout preserved")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func setProcessOwner(cmd *exec.Cmd, ctx context.Context) {
	if owner, ok := ctx.Value(workOwnerKey{}).(workOwner); ok {
		filtered := cmd.Env[:0]
		for _, entry := range cmd.Env {
			if !strings.HasPrefix(entry, "SWEEP_WORK_OWNER=") {
				filtered = append(filtered, entry)
			}
		}
		cmd.Env = append(filtered, fmt.Sprintf("SWEEP_WORK_OWNER=%s", owner.Token))
	}
}

func lockResults(root string) (*os.File, string, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, "", errors.New("cannot create results directory")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, "", errors.New("cannot resolve results directory")
	}
	file, err := os.OpenFile(filepath.Join(canonical, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, "", errors.New("cannot open results directory lock")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, "", errors.New("results directory lock is not a regular file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, "", errors.New("results directory is locked by another invocation")
		}
		return nil, "", errors.New("cannot lock results directory")
	}
	// Leave the inode in place. Removing a released lock creates a race with a
	// process that already opened that inode. Close also releases locks after a crash.
	return file, canonical, nil
}
