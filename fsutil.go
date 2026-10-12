package main

import (
	"errors"
	"io/fs"
	"runtime"
	"strings"
	"time"
)

var (
	renameTries   = 50
	renameBackoff = 20 * time.Millisecond
	renameFn      = replaceFile
	// renameRetryOn: only Windows has transient sharing violations; elsewhere one attempt.
	renameRetryOn = runtime.GOOS == "windows"
)

// renameRetry renames tmp over path (replaceFile), retrying a failed rename (50 x 20 ms).
// On Windows replaceFile uses the POSIX rename, which replaces a target that readers hold
// open with FILE_SHARE_DELETE; a handle without it (os.ReadFile in wr ui or a reader of
// runs/config, an antivirus scan) still refuses until it closes, hence the retries.
// A missing tmp is not retried.
func renameRetry(tmp, path string) error {
	var err error
	tries := renameTries
	if !renameRetryOn {
		tries = 1
	}
	for i := 0; i < tries; i++ {
		if err = renameFn(tmp, path); err == nil || errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if i+1 < tries {
			time.Sleep(renameBackoff)
		}
	}
	return err
}

// envKeyIs reports whether the KEY=value entry e has key k; Windows env keys are case-insensitive.
func envKeyIs(e, k string) bool {
	i := strings.IndexByte(e, '=')
	if i == 0 { // Windows per-drive entries like =C:=C:\x
		i = strings.IndexByte(e[1:], '=') + 1
	}
	if i < 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(e[:i], k)
	}
	return e[:i] == k
}
