package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	"github.com/yarlson/aerie/pkg/config"
)

type DeploymentTestSuite struct {
	suite.Suite
	updater *Deployment
	network string
}

func TestDeploymentSuite(t *testing.T) {
	suite.Run(t, new(DeploymentTestSuite))
}

type LocalExecutor struct{}

func (e *LocalExecutor) RunCommand(ctx context.Context, command string, args ...string) (io.Reader, error) {
	cmd := exec.CommandContext(ctx, command, args...)

	var combinedOutput bytes.Buffer
	cmd.Stdout = &combinedOutput
	cmd.Stderr = &combinedOutput

	if err := cmd.Run(); err != nil {
		return nil, err
	}

	return bytes.NewReader(combinedOutput.Bytes()), nil
}

func (suite *DeploymentTestSuite) SetupSuite() {
	suite.network = "aerie-test-network"
	_ = exec.Command("docker", "network", "create", suite.network).Run()
}

func (suite *DeploymentTestSuite) TearDownSuite() {
	_ = exec.Command("docker", "network", "rm", suite.network).Run()
}

func (suite *DeploymentTestSuite) SetupTest() {
	executor := &LocalExecutor{}
	suite.updater = NewDeployment(executor)
}

func (suite *DeploymentTestSuite) createTempDir() string {
	tmpDir, err := os.MkdirTemp("", "docker-test")
	assert.NoError(suite.T(), err)
	return tmpDir
}

func (suite *DeploymentTestSuite) removeContainer(containerName string) {
	_ = exec.Command("docker", "stop", containerName).Run() // nolint: errcheck
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
}

func (suite *DeploymentTestSuite) inspectContainer(containerName string) map[string]interface{} {
	cmd := exec.Command("docker", "inspect", containerName)
	output, err := cmd.Output()
	assert.NoError(suite.T(), err)

	var containerInfo []map[string]interface{}
	err = json.Unmarshal(output, &containerInfo)
	assert.NoError(suite.T(), err)
	assert.Len(suite.T(), containerInfo, 1)

	return containerInfo[0]
}

func (suite *DeploymentTestSuite) TestUpdateService() {
	tmpDir := suite.createTempDir()
	defer os.RemoveAll(tmpDir)

	const (
		project     = "test-project"
		serviceName = "test-update-service"
		network     = "aerie-test-network"
		proxyName   = "nginx-proxy"
		proxyPort   = "443"
	)

	initialService := &config.Service{
		Name:  serviceName,
		Image: "nginx:1.19",
		Port:  80,
		EnvVars: []config.EnvVar{
			{
				Name:  "INITIAL_ENV",
				Value: "initial_value",
			},
		},
		Volumes: []string{
			tmpDir + ":/initial/path",
		},
		HealthCheck: &config.HealthCheck{
			Path:     "/",
			Interval: time.Second,
			Timeout:  time.Second,
			Retries:  30,
		},
	}

	suite.removeContainer(serviceName)
	suite.removeContainer(proxyName)

	err := suite.updater.InstallService(initialService, network)
	assert.NoError(suite.T(), err)

	defer suite.removeContainer(serviceName)
	defer suite.removeContainer(proxyName)

	cfg := &config.Config{
		Project: config.Project{
			Name:   project,
			Domain: "localhost",
			Email:  "test@example.com",
		},
		Services: []config.Service{
			{
				Name:  serviceName,
				Image: "nginx:1.20",
				Port:  80,
				EnvVars: []config.EnvVar{
					{
						Name:  "UPDATED_ENV",
						Value: "updated_value",
					},
				},
				Volumes: []string{
					tmpDir + ":/updated/path",
				},
				HealthCheck: &config.HealthCheck{
					Path:     "/",
					Interval: time.Second,
					Timeout:  time.Second,
					Retries:  30,
				},
				Routes: []config.Route{
					{
						PathPrefix:  "/",
						StripPrefix: false,
					},
				},
			},
		},
	}

	projectPath, err := suite.updater.prepareProjectFolder(project)
	assert.NoError(suite.T(), err)

	proxyCertPath := filepath.Join(projectPath, "localhost.crt")
	proxyKeyPath := filepath.Join(projectPath, "localhost.key")
	mkcertCmds := [][]string{
		{"mkcert", "-install"},
		{"mkcert", "-cert-file", proxyCertPath, "-key-file", proxyKeyPath, "localhost"},
	}

	for _, cmd := range mkcertCmds {
		if output, err := suite.updater.runCommand(context.Background(), cmd[0], cmd[1:]...); err != nil {
			assert.NoError(suite.T(), err)
			suite.T().Log(output)
			return
		}
	}

	suite.removeContainer("proxy")
	err = suite.updater.StartProxy(project, cfg, network)
	assert.NoError(suite.T(), err)

	defer suite.removeContainer("proxy")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	requestStats := struct {
		totalRequests  int32
		failedRequests int32
	}{}

	time.Sleep(5 * time.Second)

	for i := 0; i < 10; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
					resp, err := http.Get("https://localhost:" + proxyPort)
					atomic.AddInt32(&requestStats.totalRequests, 1)
					if err != nil || resp.StatusCode != http.StatusOK {
						atomic.AddInt32(&requestStats.failedRequests, 1)
					}
					if resp != nil {
						_ = resp.Body.Close()
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}()
	}

	time.Sleep(2 * time.Second)

	initialContainerID, err := suite.updater.getContainerID(serviceName, network)
	assert.NoError(suite.T(), err)

	updatedService := &cfg.Services[0]

	err = suite.updater.UpdateService(updatedService, network)
	assert.NoError(suite.T(), err)

	updatedContainerID, err := suite.updater.getContainerID(serviceName, network)
	assert.NoError(suite.T(), err)

	assert.NotEqual(suite.T(), initialContainerID, updatedContainerID)

	time.Sleep(2 * time.Second)

	cancel()

	fmt.Printf("Total requests: %d\n", requestStats.totalRequests)
	fmt.Printf("Failed requests: %d\n", requestStats.failedRequests)

	assert.Equal(suite.T(), int32(0), requestStats.failedRequests, "Expected zero failed requests during zero-downtime deployment")

	containerInfo := suite.inspectContainer(serviceName)

	suite.Run("Updated Container State and Config", func() {
		assert.Equal(suite.T(), "running", containerInfo["State"].(map[string]interface{})["Status"])
		assert.Contains(suite.T(), containerInfo["Config"].(map[string]interface{})["Image"], "nginx:1.20")
		assert.Equal(suite.T(), network, containerInfo["HostConfig"].(map[string]interface{})["NetworkMode"])
	})

	suite.Run("Updated Environment Variables", func() {
		env := containerInfo["Config"].(map[string]interface{})["Env"].([]interface{})
		assert.Contains(suite.T(), env, "UPDATED_ENV=updated_value")
		assert.NotContains(suite.T(), env, "INITIAL_ENV=initial_value")
	})

	suite.Run("Updated Volume Bindings", func() {
		binds := containerInfo["HostConfig"].(map[string]interface{})["Binds"].([]interface{})
		assert.Contains(suite.T(), binds, tmpDir+":/updated/path")
		assert.NotContains(suite.T(), binds, tmpDir+":/initial/path")
	})

	suite.Run("Updated Network Aliases", func() {
		networkSettings := containerInfo["NetworkSettings"].(map[string]interface{})
		networks := networkSettings["Networks"].(map[string]interface{})
		networkInfo := networks[network].(map[string]interface{})
		aliases := networkInfo["Aliases"].([]interface{})
		assert.Contains(suite.T(), aliases, serviceName)
	})

	suite.Run("Updated Health Checks", func() {
		err = suite.updater.performHealthChecks(serviceName, updatedService.HealthCheck)
		assert.NoError(suite.T(), err)
	})
}

func (suite *DeploymentTestSuite) TestCopyTextFile() {
	tmpDir, err := os.MkdirTemp("", "copyTextFile-test")
	suite.Require().NoError(err)
	defer os.RemoveAll(tmpDir)

	sourceContent := "This is a test file\nWith multiple lines\nAnd some 'quoted' text"
	destPath := filepath.Join(tmpDir, "destination.txt")

	err = suite.updater.copyTextFile(sourceContent, destPath)
	suite.Require().NoError(err)

	destContent, err := os.ReadFile(destPath)
	suite.Require().NoError(err)
	suite.Equal(strings.TrimSpace(sourceContent), strings.TrimSpace(string(destContent)))

	specialContent := "Line with 'single quotes'\nLine with \"double quotes\"\nLine with $dollar signs"
	specialDestPath := filepath.Join(tmpDir, "special_destination.txt")

	err = suite.updater.copyTextFile(specialContent, specialDestPath)
	suite.Require().NoError(err)

	specialDestContent, err := os.ReadFile(specialDestPath)
	suite.Require().NoError(err)
	suite.Equal(strings.TrimSpace(specialContent), strings.TrimSpace(string(specialDestContent)))
}

func (suite *DeploymentTestSuite) TestMakeProjectFolder() {
	projectName := "test-project"

	suite.Run("Successful folder creation", func() {
		err := suite.updater.makeProjectFolder(projectName)
		suite.Require().NoError(err)
	})
}