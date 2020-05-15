package containerd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/typeurl"
	"github.com/davecgh/go-spew/spew"
	"github.com/testground/testground/pkg/logging"
)

type WorkerFn func(context.Context, *ContainerRef) error

const (
	workerShutdownTimeout = 1 * time.Minute
	workerShutdownTick    = 5 * time.Second
)

type Manager struct {
	logging.Logging

	Client *containerd.Client
}

type ContainerRef struct {
	logging.Logging
	ID      string
	Manager *Manager
}

func NewManager() *Manager {
	address := "/run/containerd/containerd.sock"
	containerd, err := containerd.New(address, containerd.WithDefaultNamespace("k8s.io"))
	if err != nil {
		panic(err)
	}

	return &Manager{
		Logging: logging.NewLogging(logging.NewLogger()),
		Client:  containerd,
	}
}

func (m *Manager) NewContainerRef(id string) *ContainerRef {
	return &ContainerRef{
		Logging: logging.NewLogging(m.S().With("container", id).Desugar()),
		ID:      id,
		Manager: m,
	}
}

func (c *ContainerRef) Id() string {
	return c.ID
}

func (c *ContainerRef) Env(ctx context.Context) ([]string, error) {
	container, err := c.Manager.Client.LoadContainer(ctx, c.ID)
	if err != nil {
		return nil, err
	}

	spew.Dump(container)

	return nil, errors.New("wtf env")
}

func (c *ContainerRef) Hostname(ctx context.Context) (string, error) {
	container, err := c.Manager.Client.LoadContainer(ctx, c.ID)
	if err != nil {
		return "", err
	}

	spew.Dump(container)

	return "", errors.New("wtf hostname")
}

func (c *ContainerRef) Labels(ctx context.Context) (map[string]string, error) {
	container, err := c.Manager.Client.LoadContainer(ctx, c.ID)
	if err != nil {
		return nil, err
	}

	spew.Dump(container.Labels(ctx))

	return nil, errors.New("wtf labels")
}

func (c *ContainerRef) Pid(ctx context.Context) (int, error) {
	container, err := c.Manager.Client.LoadContainer(ctx, c.ID)
	if err != nil {
		return 0, err
	}

	spew.Dump(container.Labels(ctx))

	return 0, errors.New("pid labels")
}

func (c *ContainerRef) IsRunning(ctx context.Context) (bool, error) {
	container, err := c.Manager.Client.LoadContainer(ctx, c.ID)
	if err != nil {
		return false, err
	}

	task, err := container.Task(ctx, cio.Load)
	if err != nil {
		return false, err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return false, err
	}
	if status.Status == containerd.Running {
		return true, nil
	}

	return false, nil
}

func (m *Manager) Watch(ctx context.Context, worker WorkerFn, labels ...string) error {
	type workerHandle struct {
		done   chan struct{}
		cancel context.CancelFunc
	}

	// Manage workers.
	managers := make(map[string]workerHandle)

	defer func() {
		// wait for the running managers to exit
		// They'll get canceled when we close the main context (deferred
		// below).
		for _, mg := range managers {
			<-mg.done
		}
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stop := func(containerID string) {
		wh, ok := managers[containerID]
		if !ok {
			return
		}

		wh.cancel()
		delete(managers, containerID)

		timeout := time.NewTimer(workerShutdownTimeout)
		defer timeout.Stop()

		ticker := time.NewTicker(workerShutdownTick)
		defer ticker.Stop()

		for {
			select {
			case <-wh.done:
				return
			case <-timeout.C:
				m.S().Panicw("timed out waiting for container worker to stop", "container", containerID)
				return
			case <-ticker.C:
				m.S().Errorw("waiting for container worker to stop", "container", containerID)
			}
		}
	}

	start := func(containerID string) {
		if _, ok := managers[containerID]; ok {
			return
		}

		cctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		managers[containerID] = workerHandle{
			done:   done,
			cancel: cancel,
		}

		go func() {
			defer close(done)
			handle := m.NewContainerRef(containerID)
			err := worker(cctx, handle)
			if err != nil {
				if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") { // docker doesn't wrap errors
					handle.S().Debugf("sidecar worker failed: %s", err)
				} else {
					handle.S().Errorf("sidecar worker failed: %s", err)
				}
			}
		}()
	}

	// Manage existing containers.
	nodes, err := m.Client.Containers(ctx)
	if err != nil {
		return err
	}

	for _, n := range nodes {
		start(n.ID())
	}

	// Manage new containers.
	es := m.Client.EventService()

	filters := []string{
		"topic==\"/containers/create\"",
		"topic==\"/containers/delete\"",
		"topic==\"/containers/update\"",
	}
	eventCh, errs := es.Subscribe(ctx, filters...)

	for {
		select {
		case event := <-eventCh:
			iface, err := typeurl.UnmarshalAny(event.Event)
			if err != nil {
				return err
			}
			switch event.Event.TypeUrl {
			case "containerd.events.ContainerCreate":
				containerCreate := iface.(*events.ContainerCreate)
				start(containerCreate.ID)
			case "containerd.events.ContainerDelete":
				containerCreate := iface.(*events.ContainerDelete)
				stop(containerCreate.ID)
			default:
				return fmt.Errorf("unexpected event: %v", event.Event)
			}
		case err := <-errs:
			return err
		}
	}

}

func (m *Manager) Close() error {
	return m.Client.Close()
}
