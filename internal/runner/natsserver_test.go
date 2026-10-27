package runner

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// The integration tests run nats-server itself, the release paddock pins
// (scripts/nats_it.sh, nats:2.15.1), as a process of its own: nothing of NATS is
// mocked (R9). The release archive is downloaded once into the user's cache and
// checked against these sums, from the release's SHA256SUMS. A server built into
// the tests instead would bring nats-server's module graph into go.mod.
// TWR_TEST_NATS_SERVER names a binary to use instead, which must be this release.
const natsVersion = "v2.15.1"

var natsSHA256 = map[string]string{
	"darwin-amd64": "3cba5976a4fbd022669d3401cab4147faeb2b4c079712c882edb9ae4e52feb06",
	"darwin-arm64": "ed0fe39222ec2e317c4d0e1a9cdc16c63ec523d94f9355d24a902a4db2655ffd",
	"linux-amd64":  "bf5edc7b1ce98e885c368cb43a886eb148dc5d0551d99f3cf6141fb5e1954c70",
	"linux-arm64":  "e2fbab4cad7d3c76e3052eb959570393c9d73ba756b70bf8e9eea61e9ae6d49c",
}

// natsBinary is the nats-server the tests run, set by TestMain.
var natsBinary string

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) != "" {
		os.Exit(runChild())
	}
	bin, err := findNATSServer()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "nats-server:", err)
		os.Exit(1)
	}
	natsBinary = bin
	os.Exit(m.Run())
}

func findNATSServer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if bin := os.Getenv("TWR_TEST_NATS_SERVER"); bin != "" {
		out, err := exec.CommandContext(ctx, bin, "--version").Output() //nolint:gosec // the binary the person running the tests named
		if err != nil {
			return "", fmt.Errorf("TWR_TEST_NATS_SERVER=%s: %w", bin, err)
		}
		if !strings.Contains(string(out), natsVersion) {
			return "", fmt.Errorf("TWR_TEST_NATS_SERVER=%s is %q, not %s", bin, strings.TrimSpace(string(out)), natsVersion)
		}
		return bin, nil
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	sum, ok := natsSHA256[platform]
	if !ok {
		return "", fmt.Errorf("no %s release for %s pinned here; set TWR_TEST_NATS_SERVER", natsVersion, platform)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "those-who-run", "nats-server-"+natsVersion+"-"+platform)
	bin := filepath.Join(dir, "nats-server")
	archivePath := filepath.Join(dir, "nats-server.tar.gz")
	// The archive is kept beside the binary and checked on every use, and the
	// binary taken from it again: a cache someone else can write to is not
	// trusted as it stands.
	if archive, err := os.ReadFile(archivePath); err == nil { //nolint:gosec // the tests' own cache
		if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) == sum {
			return bin, extract(archive, "nats-server-"+natsVersion+"-"+platform+"/nats-server", bin)
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	name := "nats-server-" + natsVersion + "-" + platform
	url := "https://github.com/nats-io/nats-server/releases/download/" + natsVersion + "/" + name + ".tar.gz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != sum {
		return "", fmt.Errorf("%s has sha256 %x, want %s", url, got, sum)
	}
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		return "", err
	}
	return bin, extract(archive, name+"/nats-server", bin)
}

// extract writes the archive's file member to path, through a temporary file
// renamed into place, so a run cut short leaves no half binary for the next.
func extract(archive []byte, member, path string) error {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s is not in the archive", member)
		}
		if err != nil {
			return err
		}
		if h.Name != member {
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".nats-server-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		if _, err := io.Copy(tmp, io.LimitReader(tr, 128<<20)); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := errors.Join(tmp.Chmod(0o755), tmp.Close()); err != nil { //nolint:gosec // an executable
			return err
		}
		return os.Rename(tmp.Name(), path)
	}
}

// natsServer is a nats-server -js the test started.
type natsServer struct {
	cmd  *exec.Cmd
	port int
	done chan struct{}
}

// ClientURL is the server's address for a client.
func (s *natsServer) ClientURL() string { return "nats://127.0.0.1:" + strconv.Itoa(s.port) }

// stop stops the server and waits for it to go.
func (s *natsServer) stop() {
	_ = s.cmd.Process.Kill()
	<-s.done
}

// startNATS starts nats-server with JetStream on port (any free one for 0), its
// store in a directory of the test's, stopped when the test ends.
func startNATS(t *testing.T, port int) *natsServer {
	t.Helper()
	if port == 0 {
		port = freePort(t)
	}
	cmd := exec.Command(natsBinary, "-a", "127.0.0.1", "-p", strconv.Itoa(port), //nolint:gosec // the pinned nats-server
		"-js", "-sd", t.TempDir())
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &natsServer{cmd: cmd, port: port, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(s.done)
	}()
	t.Cleanup(s.stop)
	deadline := time.Now().Add(10 * time.Second)
	for {
		nc, err := nats.Connect(s.ClientURL())
		if err == nil {
			nc.Close()
			return s
		}
		select {
		case <-s.done:
			t.Fatalf("nats-server stopped: %s", out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("nats-server not ready after 10 s: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The environment of a logbook the tests run as a process of its own, to kill it
// with SIGKILL: a real crash, with no clean close of the store or the connection.
const (
	childEnv     = "LOGBOOK_TEST_CHILD"
	childNATS    = "LOGBOOK_TEST_NATS"
	childData    = "LOGBOOK_TEST_DATA"
	childKillLap = "LOGBOOK_TEST_KILL_LAP"
)

// runChild is the test binary as that process: it runs logbook, and kills itself
// with SIGKILL just before it writes lap LOGBOOK_TEST_KILL_LAP.
func runChild() int {
	cfg := testConfig(os.Getenv(childNATS), os.Getenv(childData))
	kill, _ := strconv.Atoi(os.Getenv(childKillLap))
	cfg.beforeLapWrite = func(_, _ string, lap int32) error {
		if int(lap) == kill {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
		return nil
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := Run(context.Background(), cfg, logger, "child"); err != nil {
		return 1
	}
	return 0
}

// startChild runs logbook as a process of its own, its log going to logs.
func startChild(t *testing.T, url, dir string, killLap int, l *logs) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // this test binary
	cmd.Env = append(os.Environ(), childEnv+"=1", childNATS+"="+url, childData+"="+dir,
		childKillLap+"="+strconv.Itoa(killLap))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			_, _ = l.Write(slices.Clone(sc.Bytes()))
		}
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}
