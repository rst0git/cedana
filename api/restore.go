package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cedana/cedana/api/services/gpu"
	"github.com/cedana/cedana/api/services/task"
	"github.com/cedana/cedana/container"
	"github.com/cedana/cedana/utils"
	"github.com/checkpoint-restore/go-criu/v6/rpc"
	"github.com/shirou/gopsutil/v3/process"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

func (c *Client) prepareRestore(opts *rpc.CriuOpts, args *task.RestoreArgs, checkpointPath string) (*string, *task.ProcessState, error) {
	c.timers.Start(utils.DecompressOp)
	tmpdir := "cedana_restore"
	// make temporary folder to decompress into
	err := os.Mkdir(tmpdir, 0755)
	if err != nil {
		return nil, nil, err
	}

	c.logger.Info().Msgf("decompressing %s to %s", checkpointPath, tmpdir)
	err = utils.UntarFolder(checkpointPath, tmpdir)

	if err != nil {
		c.logger.Fatal().Err(err).Msg("error decompressing checkpoint")
	}
	c.timers.Stop(utils.DecompressOp)

	// read serialized cedanaCheckpoint
	_, err = os.Stat(filepath.Join(tmpdir, "checkpoint_state.json"))
	if err != nil {
		c.logger.Fatal().Err(err).Msg("checkpoint_state.json not found, likely error in creating checkpoint")
		return nil, nil, err
	}

	data, err := os.ReadFile(filepath.Join(tmpdir, "checkpoint_state.json"))
	if err != nil {
		c.logger.Fatal().Err(err).Msg("error reading checkpoint_state.json")
		return nil, nil, err
	}

	var checkpointState task.ProcessState
	err = json.Unmarshal(data, &checkpointState)
	if err != nil {
		c.logger.Fatal().Err(err).Msg("error unmarshaling checkpoint_state.json")
		return nil, nil, err
	}

	// check open_fds. Useful for checking if process being restored
	// is a pts slave and for determining how to handle files that were being written to.
	// TODO: We should be looking at the images instead
	open_fds := checkpointState.ProcessInfo.OpenFds
	var isShellJob bool
	for _, f := range open_fds {
		if strings.Contains(f.Path, "pts") {
			isShellJob = true
			break
		}
	}
	opts.ShellJob = proto.Bool(isShellJob)

	c.restoreFiles(&checkpointState, tmpdir)

	if err := chmodRecursive(tmpdir, 0755); err != nil {
		c.logger.Fatal().Err(err).Msg("error changing permissions")
		return nil, nil, err
	}

	return &tmpdir, &checkpointState, nil
}

// chmodRecursive changes the permissions of the given path and all its contents.
func chmodRecursive(path string, mode os.FileMode) error {
	return filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(filePath, mode)
	})
}

func (c *Client) ContainerRestore(imgPath string, containerId string) error {
	logger := utils.GetLogger()
	logger.Info().Msgf("restoring container %s from %s", containerId, imgPath)
	err := container.Restore(imgPath, containerId)
	if err != nil {
		c.logger.Fatal().Err(err)
	}
	return nil
}

// restoreFiles looks at the files copied during checkpoint and copies them back to the
// original path, creating folders along the way.
func (c *Client) restoreFiles(ps *task.ProcessState, dir string) {
	_, err := os.Stat(dir)
	if err != nil {
		return
	}
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		for _, f := range ps.ProcessInfo.OpenWriteOnlyFilePaths {
			if info.Name() == filepath.Base(f) {
				// copy file to path
				err = os.MkdirAll(filepath.Dir(f), 0755)
				if err != nil {
					return err
				}

				c.logger.Info().Msgf("copying file %s to %s", path, f)
				// copyFile copies to folder, so grab folder path
				err := utils.CopyFile(path, filepath.Dir(f))
				if err != nil {
					return err
				}

			}
		}
		return nil
	})

	if err != nil {
		c.logger.Fatal().Err(err).Msg("error copying files")
	}
}

func (c *Client) prepareRestoreOpts() *rpc.CriuOpts {
	opts := rpc.CriuOpts{
		LogLevel:       proto.Int32(4),
		LogFile:        proto.String("cedana-restore.log"),
		TcpEstablished: proto.Bool(true),
	}

	return &opts

}

func (c *Client) criuRestore(opts *rpc.CriuOpts, nfy Notify, dir string) (*int32, error) {

	img, err := os.Open(dir)
	if err != nil {
		c.logger.Fatal().Err(err).Msg("could not open directory")
	}
	defer img.Close()

	opts.ImagesDirFd = proto.Int32(int32(img.Fd()))

	resp, err := c.CRIU.Restore(opts, &nfy)
	if err != nil {
		// cleanup along the way
		os.RemoveAll(dir)
		c.logger.Warn().Msgf("error restoring process: %v", err)
		return nil, err
	}

	c.logger.Info().Msgf("process restored: %v", resp)

	// clean up
	err = os.RemoveAll(dir)
	if err != nil {
		c.logger.Fatal().Err(err).Msg("error removing directory")
	}
	c.cleanupClient()
	return resp.Restore.Pid, nil
}

func patchPodmanRestore(opts *container.RuncOpts, containerId, imgPath string) error {
	ctx := context.Background()

	// Podman run -d state
	if !opts.Detatch {
		jsonData, err := os.ReadFile(opts.Bundle + "config.json")
		if err != nil {
			return err
		}

		var data map[string]interface{}

		if err := json.Unmarshal(jsonData, &data); err != nil {
			return err
		}

		data["process"].(map[string]interface{})["terminal"] = false
		updatedJSON, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(opts.Bundle+"config.json", updatedJSON, 0644); err != nil {
			return err
		}
	}

	// Here is the podman patch
	if err := utils.CRImportCheckpoint(ctx, imgPath, containerId); err != nil {
		return err
	}

	return nil
}

func recursivelyReplace(data interface{}, oldValue, newValue string) {
	switch v := data.(type) {
	case map[string]interface{}:
		for key, value := range v {
			if str, isString := value.(string); isString {
				v[key] = strings.Replace(str, oldValue, newValue, -1)
			} else {
				recursivelyReplace(value, oldValue, newValue)
			}
		}
	case []interface{}:
		for _, value := range v {
			recursivelyReplace(value, oldValue, newValue)
		}
	}
}

func (c *Client) RuncRestore(imgPath, containerId string, opts *container.RuncOpts) error {

	bundle := Bundle{Bundle: opts.Bundle}

	isPodman := checkIfPodman(bundle)

	if isPodman {
		var spec rspec.Spec
		parts := strings.Split(opts.Bundle, "/")
		oldContainerId := parts[6]

		runPath := "/run/containers/storage/overlay-containers/" + oldContainerId + "/userdata"
		newRunPath := "/run/containers/storage/overlay-containers/" + containerId
		newVarPath := "/var/lib/containers/storage/overlay/" + containerId + "/merged"

		parts[6] = containerId
		// exclude last part for rsync
		parts = parts[1 : len(parts)-1]
		newBundle := "/" + strings.Join(parts, "/")

		if err := rsyncDirectories(opts.Bundle, newBundle); err != nil {
			return err
		}

		if err := rsyncDirectories(runPath, newRunPath); err != nil {
			return err
		}

		configFile, err := os.ReadFile(filepath.Join(newBundle+"/userdata", "config.json"))
		if err != nil {
			return err
		}

		if err := json.Unmarshal(configFile, &spec); err != nil {
			return err
		}

		recursivelyReplace(&spec, oldContainerId, containerId)

		varPath := spec.Root.Path + "/"

		if err := os.Mkdir("/var/lib/containers/storage/overlay/"+containerId, 0644); err != nil {
			return err
		}

		if err := rsyncDirectories(varPath, newVarPath); err != nil {
			return err
		}

		spec.Root.Path = newVarPath

		updatedConfig, err := json.Marshal(spec)
		if err != nil {
			return err
		}

		if err := os.WriteFile(filepath.Join(newBundle+"/userdata", "config.json"), updatedConfig, 0644); err != nil {
			return err
		}

		opts.Bundle = newBundle + "/userdata"
	}

	err := container.RuncRestore(imgPath, containerId, *opts)
	if err != nil {
		return err
	}

	go func() {
		if isPodman {
			if err := patchPodmanRestore(opts, containerId, imgPath); err != nil {
				log.Fatal(err)
			}
		}
	}()

	return nil
}

// Define the rsync command and arguments
// Set the output and error streams to os.Stdout and os.Stderr to see the output of rsync
// Run the rsync command

// Using rsync instead of cp -r, for some reason cp -r was not copying all the files and directories over but rsync does...
func rsyncDirectories(source, destination string) error {
	cmd := exec.Command("sudo", "rsync", "-av", "--exclude=attach", source, destination)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) Restore(args *task.RestoreArgs) (*int32, error) {
	defer c.timeTrack(time.Now(), "restore")
	var dir *string
	var pid *int32

	opts := c.prepareRestoreOpts()
	nfy := Notify{
		Logger: c.logger,
	}

	dir, state, err := c.prepareRestore(opts, nil, args.CheckpointPath)
	if err != nil {
		return nil, err
	}

	var gpuCmd *exec.Cmd
	if state.GPUCheckpointed {
		nfy.PreResumeFunc = NotifyFunc{
			Avail: true,
			Callback: func() error {
				var err error
				gpuCmd, err = c.gpuRestore(*dir)
				return err
			},
		}
	}

	c.timers.Start(utils.CriuRestoreOp)
	pid, err = c.criuRestore(opts, nfy, *dir)
	if err != nil {
		return nil, err
	}
	c.timers.Stop(utils.CriuRestoreOp)

	if state.GPUCheckpointed {
		go func() {
			proc, err := process.NewProcess(*pid)
			if err != nil {
				c.logger.Error().Msgf("could not find process: %v", err)
				return
			}
			for {
				running, err := proc.IsRunning()
				if err != nil || !running {
					break
				}
				time.Sleep(1 * time.Second)
			}
			c.logger.Debug().Msgf("process %d exited, killing gpu-controller", *pid)
			gpuCmd.Process.Kill()
		}()
	}

	return pid, nil
}

func (c *Client) gpuRestore(dir string) (*exec.Cmd, error) {
	// TODO NR - propagate uid/guid too
	gpuCmd, err := StartGPUController(1000, 1000, c.logger)
	if err != nil {
		c.logger.Warn().Msgf("could not start gpu-controller: %v", err)
		return nil, err
	}

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))

	gpuConn, err := grpc.Dial("127.0.0.1:50051", opts...)
	if err != nil {
		log.Fatalf("fail to dial: %v", err)
	}
	defer gpuConn.Close()

	gpuServiceConn := gpu.NewCedanaGPUClient(gpuConn)

	args := gpu.RestoreRequest{
		Directory: dir,
	}
	resp, err := gpuServiceConn.Restore(c.ctx, &args)
	if err != nil {
		c.logger.Warn().Msgf("could not restore gpu: %v", err)
		return nil, err
	}

	if !resp.Success {
		return nil, fmt.Errorf("could not restore gpu")
	}

	return gpuCmd, nil
}
