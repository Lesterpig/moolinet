package sandbox

import (
	"context"
	"errors"
	"io/ioutil"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

type DockerSandbox struct {
	client      *client.Client
	image       string
	logs        string
	containerId string
}

type commandOutput struct {
	output string
	err    error
}

// We should use a design pattern such as Fabric or Builder, or somethng similar
// In order to split the creation logic and the command logic.
// BUG(#2) We should not try to download the image every time we start a new container. We should check if it exists locally.
func NewDockerSandbox(image string) (*DockerSandbox, error) {
	s := new(DockerSandbox)
	err := (error)(nil)
	s.image = image
	s.logs = ""

	s.client, err = client.NewEnvClient()
	if err != nil {
		return nil, err
	}

	err = s.downloadImage()
	if err != nil {
		return nil, err
	}

	err = s.createContainer()
	if err != nil {
		return nil, err
	}

	err = s.startContainer()
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (s *DockerSandbox) Destroy() {
	err := s.client.ContainerRemove(
		context.Background(),
		s.containerId,
		types.ContainerRemoveOptions{
			Force: true,
		},
	)

	if err != nil {
		panic(err)
	}
}

// TODO: Add connection support
func (s *DockerSandbox) Run(command []string, timeout int, connection bool) (string, error) {
	s.setConnectivity(connection)

	execId, err := s.prepareCommand(command)
	if err != nil {
		return "", err
	}

	commandChannel := make(chan commandOutput, 1)
	go s.launchCommand(execId, commandChannel)

	select {
	case res := <-commandChannel:
		return res.output, res.err
	case <-time.After(time.Second * time.Duration(timeout)):
		return "", errors.New("The command has timeout")
	}
}

func (s *DockerSandbox) GetLogs() string {
	return s.logs
}

// We will create a container
// This part is a bit hacky because we will run a
// command that hangs but do nothing.
// It will prevent the container from stopping
// and will enable us to run our desired commands...
func (s *DockerSandbox) createContainer() error {
	container, err := s.client.ContainerCreate(
		context.Background(),
		&(container.Config{
			Cmd:   []string{"/bin/sh", "-c", "while true; do sleep 86400; done"},
			Image: s.image,
		}),
		&(container.HostConfig{}),
		&(network.NetworkingConfig{}),
		"") // We don't want a name for our container

	s.containerId = container.ID
	return err
}

func (s *DockerSandbox) checkImage() bool {
	summary, err := s.client.ImageList(
		context.Background(),
		types.ImageListOptions{},
	)

	if err != nil {
		s.logs += "Unable to query the image list for " + s.image + "\n"
		return false
	}

	for _, elem := range summary {
		for _, repoTag := range elem.RepoTags {
			repo := strings.Split(repoTag, ":")[0]
			if s.image == repo || s.image == repoTag {
				return true
			}
		}
	}
	return false
}

//BUG(#1) Logs are not checked for error when pulling an image
func (s *DockerSandbox) downloadImage() error {
	if s.checkImage() {
		return nil
	}

	log.Println("Downloading the image " + s.image)
	reader, err := s.client.ImageCreate(
		context.Background(),
		s.image,
		types.ImageCreateOptions{},
	)

	if err != nil {
		s.logs += "Unable to pull the image " + s.image + "\n"
		return err
	}
	defer reader.Close()

	bytesRead, err := ioutil.ReadAll(reader)
	if err != nil {
		s.logs += "Unable to read logs when pulling the image " + s.image + "\n"
		return err
	}
	s.logs += string(bytesRead[:])

	return nil
}

func (s *DockerSandbox) startContainer() error {
	err := s.client.ContainerStart(
		context.Background(),
		s.containerId,
		types.ContainerStartOptions{},
	)
	return err
}

func (s *DockerSandbox) prepareCommand(command []string) (string, error) {
	response, err := s.client.ContainerExecCreate(
		context.Background(),
		s.containerId,
		types.ExecConfig{
			Privileged:   false,
			Tty:          true,
			AttachStdout: true,
			AttachStderr: true,
			Cmd:          command,
		},
	)

	if err != nil {
		return "", err
	}
	return response.ID, nil
}

func (s *DockerSandbox) launchCommand(execId string, commandChannel chan commandOutput) {

	// We must specify the following ExecConfig
	// Otherwhise the result is corrupted
	// I don't know why...
	session, err := s.client.ContainerExecAttach(
		context.Background(),
		execId,
		types.ExecConfig{
			Tty:          true,
			AttachStdout: true,
			AttachStderr: true,
		},
	)

	defer session.Close()
	if err != nil {
		commandChannel <- commandOutput{"", err}
		return
	}

	bytesRead, err := ioutil.ReadAll(session.Reader)
	if err != nil {
		s.logs += "Unable to read logs while running the command ?? \n"
		commandChannel <- commandOutput{"", err}
		return
	}

	output := string(bytesRead[:])
	s.logs += output

	inspection, err := s.client.ContainerExecInspect(
		context.Background(),
		execId,
	)

	if err == nil && inspection.ExitCode != 0 {
		err = errors.New("Terminated with exit code " + strconv.Itoa(inspection.ExitCode))
	}

	commandChannel <- commandOutput{output, err}
}

func (s *DockerSandbox) setConnectivity(connection bool) {
	if connection {
		s.client.NetworkConnect(
			context.Background(),
			"bridge",
			s.containerId,
			&network.EndpointSettings{},
		)
	} else {
		s.client.NetworkDisconnect(
			context.Background(),
			"bridge",
			s.containerId,
			true,
		)
	}
}
