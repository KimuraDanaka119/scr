package handle

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"encoding/json"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.org/x/net/context"
)

// PayLoad as stdin pass to ric container's stdin
type PayLoad struct {
	F []*oneFile `json:"files"`
	A *argument  `json:"argument"`
	I string     `json:"stdin"`
	L string     `json:"language"`
}

// Worker store all infomations about the run job
type Worker struct {
	Image       string // images name
	containerID string
	tmpID       string
	cli         *client.Client
	ctx         context.Context
	// ric's stdin stdout stderr
	ricIn  io.Reader
	ricOut bytes.Buffer
	ricErr bytes.Buffer
}

// Load payload to worker's stdin
// language and image info from request url
func (w *Worker) loadInfo(ar *Arun, language, image string) error {

	var payload PayLoad
	payload.F = ar.Files
	payload.A = ar.Argument
	payload.I = ar.Stdin
	payload.L = language

	js, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	w.ricIn = bytes.NewBuffer(js)
	w.Image = image
	return nil
}

func (w *Worker) createContainer() (*types.ContainerJSON, error) {
	ctx := context.Background()
	cli, err := client.NewEnvClient()
	w.cli = cli
	w.ctx = ctx
	if err != nil {
		return nil, err
	}

	config := &container.Config{
		Image:        w.Image,
		Cmd:          []string{"/home/ric/run"},
		AttachStdin:  true, // Attach the standard input, makes possible user interaction
		AttachStdout: true, // Attach the standard output
		AttachStderr: true,
		Tty:          false,
		OpenStdin:    true,
		StdinOnce:    true,
	}
	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			CPUPeriod: 100000,
			CPUQuota:  50000,
			Memory:    100 * 1024 * 1024,
			// advanced kernel-level features
			// CPURealtimePeriod : 1000000,
			// CPURealtimeRuntime: 950000,

			DiskQuota: 5 * 1024 * 1024,
		},
		Privileged: false,
		LogConfig: container.LogConfig{
			Type: "json-file",
		},
	}
	resp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, "")
	if err != nil {
		if resp.ID != "" {
			w.tmpID = resp.ID
		}
		return nil, err
	}
	inspect, err := cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		w.tmpID = resp.ID
		return nil, err
	}
	w.containerID = resp.ID
	w.tmpID = resp.ID

	return &inspect, nil
}

func (w *Worker) attachContainer() (err error) {
	options := types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	}

	fmt.Println("container", w.containerID, "Attaching...")
	hijacked, err := w.cli.ContainerAttach(w.ctx, w.containerID, options)
	if err != nil {
		return
	}
	defer hijacked.Close()

	fmt.Println("Container", w.containerID, "Starting ...")
	err = w.cli.ContainerStart(w.ctx, w.containerID, types.ContainerStartOptions{})
	if err != nil {
		return
	}

	fmt.Println("Container", w.containerID, "Waiting for attach to finish...")
	attachCh := make(chan error, 2)

	// Copy any output to the build trace
	go func() {
		_, err := stdcopy.StdCopy(&w.ricOut, &w.ricErr, hijacked.Reader)
		if err != nil {
			attachCh <- err
		}
	}()

	// Write the input to the container and close its STDIN
	go func() {
		_, err := io.Copy(hijacked.Conn, w.ricIn)
		hijacked.CloseWrite()
		if err != nil {
			attachCh <- err
		}
	}()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- w.waitForContainer()
	}()

	select {
	case <-w.ctx.Done():
		w.killContainer(w.containerID, waitCh)
		err = errors.New("Aborted")

	case err = <-attachCh:
		w.killContainer(w.containerID, waitCh)
		fmt.Println("Container", w.containerID, "attach finished with", err)

	case err = <-waitCh:
		fmt.Println("Container", w.containerID, "wait finished with", err)
	}
	return
}

func (w *Worker) waitForContainer() error {
	fmt.Println("Container", w.containerID, "Waiting...")

	retries := 0
	// Use active wait
	for {
		container, err := w.cli.ContainerInspect(w.ctx, w.containerID)
		if err != nil {
			fmt.Println(err.Error())
			if client.IsErrNotFound(err) {
				return err
			}

			if retries > 3 {
				return err
			}

			retries++
			time.Sleep(time.Second)
			continue
		}

		// Reset retry timer
		retries = 0
		if container.State.Running {
			time.Sleep(time.Second)
			continue
		}

		if container.State.ExitCode != 0 {
			return fmt.Errorf("exit code %d", container.State.ExitCode)
		}

		return nil
	}
}

func (w *Worker) killContainer(id string, waitCh chan error) (err error) {
	for {

		fmt.Println("Container", id, " Killing ...")
		w.cli.ContainerKill(w.ctx, id, "SIGKILL")

		// Wait for signal that container were killed
		// or retry after some time
		select {
		case err = <-waitCh:
			return

		case <-time.After(time.Second):
		}
	}
}
