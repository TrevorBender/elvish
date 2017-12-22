package clientcommon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/boltdb/bolt"
	daemonapi "github.com/elves/elvish/daemon/api"
	"github.com/elves/elvish/eval"
	"github.com/elves/elvish/eval/re"
	"github.com/elves/elvish/program/daemon"
	"github.com/elves/elvish/store/storedefs"
)

const (
	daemonWaitOneLoop = 10 * time.Millisecond
	daemonWaitLoops   = 100
	daemonWaitTotal   = daemonWaitOneLoop * daemonWaitLoops
)

const upgradeDbNotice = `If you upgraded Elvish from a pre-0.10 version, you need to upgrade your database by following instructions in https://github.com/elves/upgrade-db-for-0.10/`

// InitRuntime initializes the runtime. The caller is responsible for calling
// CleanupRuntime at some point.
func InitRuntime(binpath, sockpath, dbpath string) (*eval.Evaler, string) {
	var dataDir string
	var err error

	// Determine data directory.
	dataDir, err = storedefs.EnsureDataDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot create data directory ~/.elvish")
	} else {
		if dbpath == "" {
			dbpath = filepath.Join(dataDir, "db")
		}
	}

	// Determine runtime directory.
	runDir, err := getSecureRunDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot get runtime dir /tmp/elvish-$uid, falling back to data dir ~/.elvish:", err)
		runDir = dataDir
	}
	if sockpath == "" {
		sockpath = filepath.Join(runDir, "sock")
	}

	toSpawn := &daemon.Daemon{
		BinPath:       binpath,
		DbPath:        dbpath,
		SockPath:      sockpath,
		LogPathPrefix: filepath.Join(runDir, "daemon.log-"),
	}
	var cl *daemonapi.Client
	if sockpath != "" && dbpath != "" {
		cl = daemonapi.NewClient(sockpath)
		_, statErr := os.Stat(sockpath)
		killed := false
		if statErr == nil {
			// Kill the daemon if it is outdated.
			version, err := cl.Version()
			if err != nil {
				fmt.Fprintln(os.Stderr, "warning: socket exists but not responding version RPC:", err)
				// TODO(xiaq): Remove this when the SQLite-backed database
				// becomes an unmemorable past (perhaps 6 months after the
				// switch to boltdb).
				if err.Error() == bolt.ErrInvalid.Error() {
					fmt.Fprintln(os.Stderr, upgradeDbNotice)
				}
				goto spawnDaemonEnd
			}
			logger.Printf("daemon serving version %d, want version %d", version, daemonapi.Version)
			if version < daemonapi.Version {
				pid, err := cl.Pid()
				if err != nil {
					fmt.Fprintln(os.Stderr, "warning: socket exists but not responding pid RPC:", err)
					cl.Close()
					cl = nil
					goto spawnDaemonEnd
				}
				cl.Close()
				logger.Printf("killing outdated daemon with pid %d", pid)
				p, err := os.FindProcess(pid)
				if err != nil {
					err = p.Kill()
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "warning: failed to kill outdated daemon process:", err)
					cl = nil
					goto spawnDaemonEnd
				}
				logger.Println("killed outdated daemon")
				killed = true
			}
		}
		if os.IsNotExist(statErr) || killed {
			logger.Println("socket does not exists, starting daemon")
			err := toSpawn.Spawn()
			if err != nil {
				fmt.Fprintln(os.Stderr, "warning: cannot start daemon:", err)
			} else {
				logger.Println("started daemon")
			}
			for i := 0; i <= daemonWaitLoops; i++ {
				_, err := cl.Version()
				if err == nil {
					logger.Println("daemon online")
					goto spawnDaemonEnd
				} else if err.Error() == bolt.ErrInvalid.Error() {
					fmt.Fprintln(os.Stderr, upgradeDbNotice)
					goto spawnDaemonEnd
				} else if i == daemonWaitLoops {
					fmt.Fprintf(os.Stderr, "cannot connect to daemon after %v: %v\n", daemonWaitTotal, err)
					goto spawnDaemonEnd
				}
				time.Sleep(daemonWaitOneLoop)
			}
		}
	}
spawnDaemonEnd:

	ev := eval.NewEvaler()
	ev.SetLibDir(filepath.Join(dataDir, "lib"))
	// TODO(xiaq): Maybe install daemon module asynchronously
	ev.InstallDaemon(cl, toSpawn)
	// TODO(xiaq): Installation of the re module might belong somewhere else.
	ev.InstallModule("re", re.Namespace())
	return ev, dataDir
}

// CleanupRuntime cleans up the runtime.
func CleanupRuntime(ev *eval.Evaler) {
	if ev.DaemonClient != nil {
		err := ev.DaemonClient.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: failed to close connection to daemon:", err)
		}
	}
}

var (
	ErrBadOwner      = errors.New("bad owner")
	ErrBadPermission = errors.New("bad permission")
)
