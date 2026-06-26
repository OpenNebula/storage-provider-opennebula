package opennebula

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilexec "k8s.io/utils/exec"
)

type captureExec struct {
	lastCommand string
	lastArgs    []string
	output      []byte
	err         error
}

func (e *captureExec) Command(cmd string, args ...string) utilexec.Cmd {
	e.lastCommand = cmd
	e.lastArgs = append([]string(nil), args...)
	return &captureCmd{output: e.output, err: e.err}
}

func (e *captureExec) CommandContext(_ context.Context, cmd string, args ...string) utilexec.Cmd {
	return e.Command(cmd, args...)
}

func (e *captureExec) LookPath(file string) (string, error) {
	return file, nil
}

type captureCmd struct {
	output []byte
	err    error
}

func (c *captureCmd) Run() error                         { return c.err }
func (c *captureCmd) CombinedOutput() ([]byte, error)    { return c.output, c.err }
func (c *captureCmd) Output() ([]byte, error)            { return c.output, c.err }
func (c *captureCmd) SetDir(string)                      {}
func (c *captureCmd) SetStdin(io.Reader)                 {}
func (c *captureCmd) SetStdout(io.Writer)                {}
func (c *captureCmd) SetStderr(io.Writer)                {}
func (c *captureCmd) SetEnv([]string)                    {}
func (c *captureCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (c *captureCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }
func (c *captureCmd) Start() error                       { return c.err }
func (c *captureCmd) Wait() error                        { return c.err }
func (c *captureCmd) Stop()                              {}

func TestRunCephCommandFallsBackToEmptyConfigWhenUnset(t *testing.T) {
	exec := &captureExec{output: []byte("ok")}
	provider := &CephFSVolumeProvider{exec: exec}

	_, err := provider.runCephCommand(context.Background(), Datastore{
		ID: 104,
		CephFS: &CephFSDatastoreAttributes{
			Monitors: []string{"mon1", "mon2"},
		},
	}, map[string]string{
		cephProvisionerSecretAdminIDKey:  "admin",
		cephProvisionerSecretAdminKeyKey: "secret",
	}, "fs", "subvolume", "info")
	require.NoError(t, err)

	require.Equal(t, cephFsCommandBin, exec.lastCommand)
	require.GreaterOrEqual(t, len(exec.lastArgs), 8)
	assert.Equal(t, []string{"-c", "/dev/null", "-m", "mon1,mon2", "--id", "admin"}, exec.lastArgs[:6])
	assert.Equal(t, "--keyfile", exec.lastArgs[6])
	assert.NotEmpty(t, exec.lastArgs[7])
	assert.Equal(t, []string{"fs", "subvolume", "info"}, exec.lastArgs[8:])
}

func TestRunCephCommandFallsBackToEmptyConfigWhenConfiguredPathMissing(t *testing.T) {
	exec := &captureExec{output: []byte("ok")}
	provider := &CephFSVolumeProvider{exec: exec}

	_, err := provider.runCephCommand(context.Background(), Datastore{
		ID: 104,
		CephFS: &CephFSDatastoreAttributes{
			Monitors:         []string{"mon1"},
			OptionalCephConf: "/etc/ceph/ceph.conf",
		},
	}, map[string]string{
		cephProvisionerSecretAdminIDKey:  "admin",
		cephProvisionerSecretAdminKeyKey: "secret",
	}, "fs", "subvolume", "info")
	require.NoError(t, err)

	assert.Equal(t, []string{"-c", "/dev/null", "-m", "mon1", "--id", "admin"}, exec.lastArgs[:6])
}

func TestRunCephCommandUsesConfiguredCephConfWhenPresent(t *testing.T) {
	tempFile, err := os.CreateTemp("", "ceph-conf-*")
	require.NoError(t, err)
	require.NoError(t, tempFile.Close())
	t.Cleanup(func() { _ = os.Remove(tempFile.Name()) })

	exec := &captureExec{output: []byte("ok")}
	provider := &CephFSVolumeProvider{exec: exec}

	_, err = provider.runCephCommand(context.Background(), Datastore{
		ID: 104,
		CephFS: &CephFSDatastoreAttributes{
			Monitors:         []string{"mon1"},
			OptionalCephConf: tempFile.Name(),
		},
	}, map[string]string{
		cephProvisionerSecretAdminIDKey:  "admin",
		cephProvisionerSecretAdminKeyKey: "secret",
	}, "fs", "subvolume", "info")
	require.NoError(t, err)

	assert.Equal(t, []string{"-c", tempFile.Name(), "-m", "mon1", "--id", "admin"}, exec.lastArgs[:6])
}
