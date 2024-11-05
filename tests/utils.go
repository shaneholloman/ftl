package tests

import (
	"context"
	"fmt"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const (
	sshPort = "22/tcp"
)

type TestContainer struct {
	Container testcontainers.Container
	SshPort   nat.Port
}

func SetupTestContainer(t *testing.T) (*TestContainer, error) {
	ctx := context.Background()

	buildCtx, err := createBuildContext()
	require.NoError(t, err)
	defer os.RemoveAll(buildCtx)

	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    buildCtx,
				Dockerfile: "Dockerfile",
			},
			ExposedPorts: []string{sshPort},
			Privileged:   true, // Required for Docker daemon
			WaitingFor: wait.ForAll(
				wait.ForListeningPort(sshPort),
			),
			Env: map[string]string{
				"DOCKER_TLS_CERTDIR": "", // Disable TLS for testing
			},
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to start Container: %w", err)
	}

	mappedPort, err := container.MappedPort(ctx, nat.Port(sshPort))
	if err != nil {
		return nil, fmt.Errorf("failed to get mapped port: %w", err)
	}

	return &TestContainer{
		Container: container,
		SshPort:   mappedPort,
	}, nil
}

func createBuildContext() (string, error) {
	dir, err := os.MkdirTemp("", "dockersync-test")
	if err != nil {
		return "", err
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("failed to get current file path")
	}
	packageDir := filepath.Dir(currentFile)

	dockerfile := filepath.Join(dir, "Dockerfile")
	if err := copyFile(filepath.Join(packageDir, "testdata", "Dockerfile"), dockerfile); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	entrypoint := filepath.Join(dir, "entrypoint.sh")
	if err := copyFile(filepath.Join(packageDir, "testdata", "entrypoint.sh"), entrypoint); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	return dir, nil
}

func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}