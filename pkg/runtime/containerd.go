package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/containerd"
	namespaces "github.com/containerd/containerd/api/services/namespaces/v1"
	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/plugin"
	"github.com/docker/docker/api/errdefs"
	"github.com/ernoaapa/can/pkg/model"
	"github.com/pkg/errors"

	log "github.com/sirupsen/logrus"
)

var (
	snapshotter = "overlayfs"
)

// ContainerdClient is containerd client wrapper
type ContainerdClient struct {
	client  *containerd.Client
	context context.Context
	timeout time.Duration
	address string
}

// NewContainerdClient creates new containerd client with given timeout
func NewContainerdClient(context context.Context, timeout time.Duration, address string) *ContainerdClient {
	return &ContainerdClient{
		context: context,
		timeout: timeout,
		address: address,
	}
}

func (c *ContainerdClient) getContext() (context.Context, context.CancelFunc) {
	var (
		ctx    = c.context
		cancel context.CancelFunc
	)

	if c.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	return ctx, cancel
}

func (c *ContainerdClient) getConnection(namespace string) (*containerd.Client, error) {
	client, err := containerd.New(c.address, containerd.WithDefaultNamespace(namespace))
	if err != nil {
		return client, errors.Wrapf(err, "Unable to create connection to containerd")
	}
	return client, nil
}

func (c *ContainerdClient) resetConnection() {
	c.client = nil
}

// GetContainers return all containerd containers
func (c *ContainerdClient) GetContainers(namespace string) (containers []containerd.Container, err error) {
	ctx, cancel := c.getContext()
	defer cancel()

	client, err := c.getConnection(namespace)
	if err != nil {
		return containers, err
	}
	containers, err = client.Containers(ctx)
	if err != nil {
		c.resetConnection()
		return containers, errors.Wrap(err, "Error while getting list of containers")
	}
	return containers, nil
}

// CreateContainer creates given container
func (c *ContainerdClient) CreateContainer(pod model.Pod, container model.Container) (containerd.Container, error) {
	ctx, cancel := c.getContext()
	defer cancel()

	client, err := c.getConnection(pod.GetNamespace())
	if err != nil {
		return nil, err
	}

	image, err := c.ensureImagePulled(pod.GetNamespace(), container.Image)
	if err != nil {
		return nil, err
	}

	spec, err := containerd.GenerateSpec(ctx, client, nil, containerd.WithImageConfig(image))
	if err != nil {
		return nil, err
	}

	log.Debugf("Create new container from image %s...", image.Name())
	created, err := client.NewContainer(ctx,
		container.ID,
		containerd.WithContainerLabels(getContainerLabels(pod, container)),
		containerd.WithSpec(spec),
		containerd.WithSnapshotter(snapshotter),
		containerd.WithNewSnapshotView(container.ID, image),
		containerd.WithRuntime(fmt.Sprintf("%s.%s", plugin.RuntimePlugin, "linux"), nil),
	)
	if err != nil {
		c.resetConnection()
		return nil, errors.Wrapf(err, "Failed to create new container from image %s", image.Name())
	}
	return created, nil
}

func (c *ContainerdClient) StartContainer(container containerd.Container) error {
	ctx, cancel := c.getContext()
	defer cancel()

	log.Debugf("Create task in container: %s", container.ID())
	task, err := container.NewTask(ctx, containerd.NullIO)
	if err != nil {
		c.resetConnection()
		return errors.Wrapf(err, "Error while creating task for container [%s]", container.ID())
	}

	log.Debugln("Starting task...")
	err = task.Start(ctx)
	if err != nil {
		c.resetConnection()
		return errors.Wrapf(err, "Failed to start task in container", container.ID())
	}
	log.Debugf("Task started (pid %d)", task.Pid())
	return nil
}

// StopContainer stops given container
func (c *ContainerdClient) StopContainer(container containerd.Container) error {
	ctx, cancel := c.getContext()
	defer cancel()

	task, err := container.Task(ctx, nil)
	if err == nil {
		task.Delete(ctx, containerd.WithProcessKill)
	}
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		c.resetConnection()
		return errors.Wrapf(err, "Failed to delete container %s", container.ID())
	}
	return nil
}

func (c *ContainerdClient) ensureImagePulled(namespace, ref string) (image containerd.Image, err error) {
	ctx, cancel := c.getContext()
	defer cancel()

	client, err := c.getConnection(namespace)
	if err != nil {
		return image, err
	}

	image, err = client.Pull(ctx, ref)
	if err != nil {
		c.resetConnection()
		return image, errors.Wrapf(err, "Error pulling image [%s]", ref)
	}

	log.Debugf("Unpacking container image [%s]...", image.Target().Digest)
	err = image.Unpack(ctx, snapshotter)
	if err != nil {
		c.resetConnection()
		return image, errors.Wrapf(err, "Error while unpacking image [%s]", image.Target().Digest)
	}

	return image, nil
}

// GetNamespaces return all namespaces what cand manages
func (c *ContainerdClient) GetNamespaces() ([]string, error) {
	ctx, cancel := c.getContext()
	defer cancel()

	client, err := c.getConnection(model.DefaultNamespace)
	if err != nil {
		return nil, err
	}

	resp, err := client.NamespaceService().List(ctx, &namespaces.ListNamespacesRequest{})
	if err != nil {
		return nil, err
	}

	return getNamespaces(resp.Namespaces), nil
}

func getNamespaces(namespaces []namespaces.Namespace) (result []string) {
	for _, namespace := range namespaces {
		if namespace.Name != "default" {
			result = append(result, namespace.Name)
		}
	}
	return result
}

// IsContainerRunning fetch container task information
func (c *ContainerdClient) IsContainerRunning(container containerd.Container) (bool, error) {
	ctx, cancel := c.getContext()
	defer cancel()

	_, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GetContainerTaskStatus resolves container status or return UNKNOWN
func (c *ContainerdClient) GetContainerTaskStatus(containerID string) string {

	ctx, cancel := c.getContext()
	defer cancel()

	client, err := c.getConnection(model.DefaultNamespace)
	if err != nil {
		log.Warnf("Unable to get connection for resolving task status for containerID %s", containerID)
		return "UNKNOWN"
	}

	resp, err := client.TaskService().Get(ctx, &tasks.GetRequest{
		ContainerID: containerID,
	})
	if err != nil {
		log.Warnf("Unable to resolve Container task status: %s", err)
		return "UNKNOWN"
	}

	return resp.Process.Status.String()
}
